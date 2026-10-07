package cache

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"
)

// synthMsg is one message of a synthetic Proton-like mailbox: header columns
// only, since Proton Bridge gives no X-GM-THRID and threading runs on
// Message-ID, References, In-Reply-To and subject.
type synthMsg struct {
	id, msgID, irt, subject, from, to string
	refs                              []string
	arrival                           int64
}

// synthCorpus builds n messages with realistic shapes: standalone mail,
// header-less automated "Re:" replies sharing a few subjects, ordinary reply
// chains (some with a missing first message), a few very long list threads,
// and some duplicate Message-IDs and reference loops.
func synthCorpus(n int, seed uint64) []synthMsg {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	span := int64(3 * 365 * 24 * 3600)
	people := make([]string, 600)
	for i := range people {
		people[i] = fmt.Sprintf("person%d@dom%d.example", i, i%80)
	}
	var out []synthMsg
	seq := 0
	next := func() int { seq++; return seq }
	add := func(m synthMsg) {
		m.id = fmt.Sprintf("pm:%d", next())
		out = append(out, m)
	}
	pool := make([]string, 300)
	for i := range pool {
		pool[i] = fmt.Sprintf("Re: Notification %d", i)
	}
	// A handful of long mailing-list threads, 2% of the corpus each.
	for l := 0; l < 5 && len(out) < n; l++ {
		size := n / 50
		base := t0 + r.Int64N(span/2)
		root := fmt.Sprintf("list%d-0@lists.example", l)
		var ids []string
		for k := 0; k < size; k++ {
			mid := fmt.Sprintf("list%d-%d@lists.example", l, k)
			m := synthMsg{msgID: mid, subject: fmt.Sprintf("[list%d] discussion", l), from: people[r.IntN(len(people))], to: "me@example.com", arrival: base + int64(k)*600 + r.Int64N(500)}
			if k > 0 {
				par := ids[r.IntN(len(ids))]
				m.irt = par
				m.refs = []string{root}
				if par != root {
					m.refs = append(m.refs, par)
				}
				m.subject = "Re: " + m.subject
			}
			ids = append(ids, mid)
			add(m)
		}
	}
	for len(out) < n {
		x := r.IntN(100)
		start := t0 + r.Int64N(span)
		switch {
		case x < 35: // standalone
			add(synthMsg{msgID: fmt.Sprintf("s%d@news.example", next()), subject: fmt.Sprintf("Weekly digest %d", seq), from: people[r.IntN(len(people))], to: "me@example.com", arrival: start})
		case x < 38: // header-less automated replies sharing a subject
			add(synthMsg{msgID: fmt.Sprintf("a%d@auto.example", next()), subject: pool[r.IntN(len(pool))], from: people[r.IntN(len(people))], to: "me@example.com", arrival: start})
		case x < 39: // no Message-ID at all
			add(synthMsg{subject: "Re: hello", from: people[r.IntN(len(people))], to: "me@example.com", arrival: start})
		default: // a reply chain
			k := 2 + int(r.ExpFloat64()*3)
			if k > 60 {
				k = 60
			}
			who := []string{people[r.IntN(len(people))], people[r.IntN(len(people))], "me@example.com"}
			tag := next()
			subj := fmt.Sprintf("Project %d", tag)
			var chain []string
			missingFirst := r.IntN(10) == 0
			for j := 0; j < k; j++ {
				mid := fmt.Sprintf("c%d-%d@x.example", tag, j)
				m := synthMsg{msgID: mid, subject: subj, from: who[r.IntN(len(who))], to: who[r.IntN(len(who))], arrival: start + int64(j)*int64(1+r.IntN(40000))}
				if j > 0 {
					m.subject = "Re: " + subj
					m.irt = chain[len(chain)-1-r.IntN(min(2, len(chain)))]
					m.refs = append([]string(nil), chain...)
					if r.IntN(8) == 0 {
						m.refs = nil // In-Reply-To only
					}
				}
				chain = append(chain, mid)
				if j == 0 && missingFirst {
					continue // the parent is never received
				}
				switch r.IntN(500) {
				case 0: // duplicate Message-ID
					d := m
					d.arrival += 5
					add(d)
				case 1: // reference loop: first message names the last one
					if j == 0 {
						m.refs = []string{fmt.Sprintf("c%d-%d@x.example", tag, k-1)}
					}
				}
				add(m)
			}
		}
	}
	out = out[:n]
	sort.SliceStable(out, func(i, j int) bool { return out[i].arrival < out[j].arrival })
	for i := range out {
		out[i].id = fmt.Sprintf("pm:%07d", i)
	}
	return out
}

// loadSynth writes the corpus the way the header backfill leaves it: header
// columns and the id index filled, nothing threaded.
func loadSynth(tb testing.TB, c *Cache, account string, ms []synthMsg) {
	tb.Helper()
	ctx := context.Background()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		tb.Fatal(err)
	}
	for _, m := range ms {
		th := threadHeaders{MessageID: m.msgID, InReplyTo: m.irt, Refs: m.refs}
		if _, err := tx.ExecContext(ctx, `INSERT INTO messages (account, stable_id, blob_sha256, from_addr, to_addr, subject, date_unix, internal_date, message_id, in_reply_to, references_json)
VALUES (?, ?, 'none', ?, ?, ?, ?, ?, ?, ?, ?)`, account, m.id, m.from, m.to, m.subject, m.arrival, m.arrival, m.msgID, m.irt, th.refsJSON()); err != nil {
			tb.Fatal(err)
		}
		if err := addRefsTx(ctx, tx, account, m.id, m.subject, th); err != nil {
			tb.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
}

func synthN(def int) int {
	if s := os.Getenv("MAILMCP_BENCH_N"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			return n
		}
	}
	return def
}

// BenchmarkBackfillThreadsProton runs the per-message threads backfill over a
// synthetic Proton mailbox (MAILMCP_BENCH_N messages, default 20000).
func BenchmarkBackfillThreadsProton(b *testing.B) {
	n := synthN(20000)
	old := backfillPause
	backfillPause = 0
	defer func() { backfillPause = old }()
	b.StopTimer()
	for i := 0; i < b.N; i++ {
		c, err := Open(b.TempDir())
		if err != nil {
			b.Fatal(err)
		}
		c.SetOwners(map[string][]string{"p": {"me@example.com"}})
		loadSynth(b, c, "p", synthCorpus(n, 1))
		b.StartTimer()
		start := time.Now()
		if err := c.BackfillThreads(context.Background(), nil); err != nil {
			b.Fatal(err)
		}
		el := time.Since(start)
		b.StopTimer()
		b.ReportMetric(float64(n)/el.Seconds(), "msgs/s")
		_ = c.Close()
	}
}
