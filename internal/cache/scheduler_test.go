package cache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestIsBusy(t *testing.T) {
	if !isBusy(fmt.Errorf("begin: %w", errors.New("database is locked (5) (SQLITE_BUSY)"))) {
		t.Error("BUSY not recognised")
	}
	if isBusy(errors.New("disk full")) || isBusy(nil) {
		t.Error("non-BUSY taken for BUSY")
	}
}

func TestRetryBusyBacksOffShortAndCounts(t *testing.T) {
	c := openCache(t)
	calls := 0
	began := time.Now()
	err := c.retryBusy(context.Background(), quiet(), "t", func() error {
		calls++
		if calls < 3 {
			return errors.New("database is locked (5) (SQLITE_BUSY)")
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("err %v calls %d", err, calls)
	}
	if d := time.Since(began); d > 3*time.Second {
		t.Errorf("two BUSY retries took %v; the backoff must be short, not 30s", d)
	}
	if c.BackfillBusyCount() != 2 {
		t.Errorf("busy count %d", c.BackfillBusyCount())
	}
	// A non-BUSY error is returned at once.
	boom := errors.New("boom")
	if err := c.retryBusy(context.Background(), quiet(), "t", func() error { return boom }); !errors.Is(err, boom) {
		t.Errorf("got %v", err)
	}
}

func TestRunBackfillsRunsFts2BeforeThreadsAndReportsPending(t *testing.T) {
	backfillPause, backfillMinYield = 0, 0
	c := openCache(t)
	seedLegacy(t, c, 700)
	c = reopenAsPreUpgrade(t, c)
	ctx := tctx(t)
	if st, _ := c.ThreadsBackfillStatus(ctx); st.State != "pending" {
		t.Errorf("threads before start: %+v", st)
	}
	if st, _ := c.BackfillStatus(ctx); st.State != "pending" {
		t.Errorf("fts2 before start: %+v", st)
	}
	var mu sync.Mutex
	var order []string
	h := &orderHandler{mu: &mu, order: &order}
	c.RunBackfills(ctx, slog.New(h))
	for _, want := range []string{"fts2 backfill complete", "thread backfill done"} {
		found := false
		for _, m := range order {
			found = found || m == want
		}
		if !found {
			t.Fatalf("missing log %q in %v", want, order)
		}
	}
	idx := func(s string) int {
		for i, m := range order {
			if m == s {
				return i
			}
		}
		return -1
	}
	if idx("fts2 backfill complete") > idx("thread backfill done") {
		t.Errorf("threads finished before fts2: %v", order)
	}
	f, _ := c.BackfillStatus(ctx)
	th, _ := c.ThreadsBackfillStatus(ctx)
	if f.State != "complete" || th.State != "complete" {
		t.Errorf("states after run: %+v %+v", f, th)
	}
}

type orderHandler struct {
	mu    *sync.Mutex
	order *[]string
}

func (h *orderHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *orderHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	*h.order = append(*h.order, r.Message)
	h.mu.Unlock()
	return nil
}
func (h *orderHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *orderHandler) WithGroup(string) slog.Handler      { return h }

func TestBackfillYieldsToForegroundWriter(t *testing.T) {
	c := openCache(t)
	end := c.foreground()
	released := make(chan struct{})
	go func() { time.Sleep(300 * time.Millisecond); end(); close(released) }()
	began := time.Now()
	if !c.yield(context.Background(), 0, 0) {
		t.Fatal("yield reported cancel")
	}
	if time.Since(began) < 250*time.Millisecond {
		t.Errorf("yield returned in %v while a foreground writer was active", time.Since(began))
	}
	<-released
}

// synthMessage is a mail of a realistic size (about 3 KB), part of a
// reply chain of 4, from one of 50 senders.
func synthMessage(i int) []byte {
	chain, pos := i/4, i%4
	var hdr strings.Builder
	fmt.Fprintf(&hdr, "From: Sender %d <s%d@example.com>\r\nTo: Owner <owner@example.com>\r\n", i%50, i%50)
	fmt.Fprintf(&hdr, "Subject: %stopic %d\r\nDate: Mon, 02 Jan 2026 15:04:05 +0000\r\nMessage-ID: <c%d-%d@example.com>\r\n",
		map[bool]string{true: "Re: ", false: ""}[pos > 0], chain, chain, pos)
	if pos > 0 {
		fmt.Fprintf(&hdr, "In-Reply-To: <c%d-%d@example.com>\r\nReferences:", chain, pos-1)
		for k := 0; k < pos; k++ {
			fmt.Fprintf(&hdr, " <c%d-%d@example.com>", chain, k)
		}
		hdr.WriteString("\r\n")
	}
	hdr.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
	body := strings.Repeat(fmt.Sprintf("Message %d of the synthetic mailbox discusses topic %d and some words to index. ", i, chain), 35)
	return []byte(hdr.String() + body + "\r\n> quoted line of the earlier mail\r\n")
}

// TestMeasureBackfills runs both jobs, one after the other, over a synthetic
// mailbox with a refresh-like writer running beside them, and prints the
// rates, the longest write hold and the writer's worst wait. It is a
// measurement, gated by BFS_MEASURE=<messages>.
func TestMeasureBackfills(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("BFS_MEASURE"))
	if n == 0 {
		t.Skip("set BFS_MEASURE=<messages> to run")
	}
	fg := os.Getenv("BFS_FG") != "0"
	c := openCache(t)
	tx, err := c.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		sum, err := c.putBlob(synthMessage(i))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO messages (account, stable_id, blob_sha256, from_addr, subject, date_unix, internal_date) VALUES ('a', ?, ?, 'Sender', 'S', ?, ?)`,
			fmt.Sprintf("m%06d", i), sum, 1700000000+int64(i), 1700000000+int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	c = reopenAsPreUpgrade(t, c)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	var phase atomic.Int32
	var worst [2]atomic.Int64
	var writes atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // a refresh-like writer: 20 inserts in one transaction every 250ms
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(250 * time.Millisecond):
			}
			began := time.Now()
			end := func() {}
			if fg {
				end = c.foreground()
			}
			tx, err := c.db.BeginTx(ctx, nil)
			if err != nil {
				t.Errorf("refresh writer: %v", err)
				end()
				return
			}
			wait := time.Since(began)
			for k := 0; k < 20; k++ {
				_, _ = tx.Exec(`INSERT INTO folders (account, folder, uidvalidity) VALUES ('w', ?, 1) ON CONFLICT DO NOTHING`, fmt.Sprintf("f%d-%d", i, k))
			}
			_ = tx.Commit()
			end()
			p := phase.Load()
			for {
				old := worst[p].Load()
				if int64(wait) <= old || worst[p].CompareAndSwap(old, int64(wait)) {
					break
				}
			}
			writes.Add(1)
		}
	}()
	t0 := time.Now()
	c.RunBackfill(ctx, quiet())
	d1 := time.Since(t0)
	hold1 := c.MaxBackfillHold()
	phase.Store(1)
	t1 := time.Now()
	if err := c.BackfillThreads(ctx, quiet()); err != nil {
		t.Fatal(err)
	}
	d2 := time.Since(t1)
	close(stop)
	wg.Wait()
	t.Logf("messages=%d foreground_flag=%v", n, fg)
	t.Logf("fts2:    %v  %.0f msg/s", d1.Round(time.Millisecond), float64(n)/d1.Seconds())
	t.Logf("threads: %v  %.0f msg/s", d2.Round(time.Millisecond), float64(n)/d2.Seconds())
	t.Logf("max write hold: fts2 phase %v, overall %v", hold1.Round(time.Millisecond), c.MaxBackfillHold().Round(time.Millisecond))
	t.Logf("refresh writer: %d writes, worst BEGIN wait fts2 phase %v, threads phase %v; backfill BUSY count %d",
		writes.Load(), time.Duration(worst[0].Load()).Round(time.Millisecond), time.Duration(worst[1].Load()).Round(time.Millisecond), c.BackfillBusyCount())
	var nt int
	_ = c.db.QueryRow(`SELECT COUNT(*) FROM message_thread`).Scan(&nt)
	if nt != n {
		t.Errorf("threaded %d of %d", nt, n)
	}
}
