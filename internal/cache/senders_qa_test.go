package cache

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// QA tests for the senders table, kind rules, reply crediting, tags and caps.

type sqMsg struct {
	acct, id, from, to, subj string
	when                     time.Time
	hdr                      []string
}

func (m sqMsg) raw() []byte {
	r := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s>\r\n", m.from, m.to, m.subj, m.when.Format(time.RFC1123Z), m.id)
	for _, h := range m.hdr {
		r += h + "\r\n"
	}
	return []byte(r + "\r\nbody\r\n")
}

// sqJob inserts the messages without counting them (the way messages that
// predate the senders job exist), then starts the job over and runs it.
func sqJob(t *testing.T, c *Cache, ms []sqMsg) {
	t.Helper()
	for _, m := range ms {
		sqInsert(t, c, m)
	}
	sqSentAll(t, c)
	restartSenders(t, c)
	if err := c.RunSenders(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

func sqInsert(t *testing.T, c *Cache, m sqMsg) int64 {
	t.Helper()
	raw := m.raw()
	sum, err := c.putBlob(raw)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := c.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	rid, ins, err := insertMessageRowTx(context.Background(), tx, m.acct, headerInfo{stableID: m.id, size: int64(len(raw)), internal: m.when}, sum, parseMessage(raw))
	if err != nil || !ins {
		t.Fatal(err, ins)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return rid
}

// sqSentAll files every message from the owner address in a \Sent folder (the
// only place an owner reply is believed), per account.
func sqSentAll(t *testing.T, c *Cache) {
	t.Helper()
	for _, q := range []string{
		`INSERT OR IGNORE INTO folders (account, folder, uidvalidity, attrs) SELECT DISTINCT account, 'Sent', 1, '\Sent' FROM messages`,
		`INSERT OR IGNORE INTO membership (account, stable_id, folder, uid, uidvalidity) SELECT account, stable_id, 'Sent', rowid, 1 FROM messages WHERE lower(from_addr) LIKE '%me@home.test%'`,
	} {
		if _, err := c.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

// sqIncremental inserts the messages the way refresh does: observe, then flush
// in the same transaction. batch=true uses one transaction for all of them,
// false one transaction per message.
func sqIncremental(t *testing.T, c *Cache, ms []sqMsg, batch bool) {
	t.Helper()
	ctx := context.Background()
	step := 1
	if batch {
		step = len(ms)
	}
	for i := 0; i < len(ms); i += step {
		part := ms[i:min(i+step, len(ms))]
		dbtx, err := c.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		sb := c.newSenderBatch()
		for _, m := range part {
			raw := m.raw()
			sum, err := c.putBlob(raw)
			if err != nil {
				t.Fatal(err)
			}
			p := parseMessage(raw)
			info := headerInfo{stableID: m.id, size: int64(len(raw)), internal: m.when}
			rid, ins, err := insertMessageRowTx(ctx, dbtx, m.acct, info, sum, p)
			if err != nil || !ins {
				t.Fatal(err, ins)
			}
			sb.observe(m.acct, rid, info, p)
		}
		if err := sb.flushTx(ctx, dbtx); err != nil {
			t.Fatal(err)
		}
		if err := dbtx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	// Replies are credited by the sweep that ends a refresh.
	sqSentAll(t, c)
	rows, err := c.db.Query(`SELECT DISTINCT account FROM messages`)
	if err != nil {
		t.Fatal(err)
	}
	var accts []string
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		accts = append(accts, a)
	}
	_ = rows.Close()
	for _, a := range accts {
		if err := c.creditSentReplies(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
}

// sqDump is every derived column of the senders table, one line per row.
func sqDump(t *testing.T, c *Cache) string {
	t.Helper()
	rows, err := c.db.Query(`SELECT account, addr, domain, display_names_json, counts_json, first_at, last_at, n_msgs, n_from_me, n_to_me,
n_replied_by_me, list_id, has_list_unsubscribe, n_list, n_unsub, n_gh, n_txn_subj, kind, kind_source FROM senders ORDER BY account, addr`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var sb strings.Builder
	for rows.Next() {
		var (
			a, ad, dom, nj, cj, lid, kind, src string
			first, last                        int64
			n, fm, tm, rep, unsub, nl, nu, gh  int
			txn                                int
		)
		if err := rows.Scan(&a, &ad, &dom, &nj, &cj, &first, &last, &n, &fm, &tm, &rep, &lid, &unsub, &nl, &nu, &gh, &txn, &kind, &src); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&sb, "%s|%s|%s|%s|%s|%d|%d|%d|%d|%d|%d|%s|%d|%d|%d|%d|%d|%s|%s\n", a, ad, dom, nj, cj, first, last, n, fm, tm, rep, lid, unsub, nl, nu, gh, txn, kind, src)
	}
	return sb.String()
}

func sqSum(t *testing.T, c *Cache, acct string) (sum, msgs int) {
	t.Helper()
	if err := c.db.QueryRow(`SELECT COALESCE(SUM(n_msgs), 0) FROM senders WHERE account = ?`, acct).Scan(&sum); err != nil {
		t.Fatal(err)
	}
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE account = ?`, acct).Scan(&msgs); err != nil {
		t.Fatal(err)
	}
	return
}

var sqT0 = time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)

// sqFixture: two accounts, owner replies, a list, a notifier, a shop. Parents
// come before their replies.
func sqFixture() []sqMsg {
	h := time.Hour
	return []sqMsg{
		{"a1", "p1", "Alice <alice@x.example>", "me@home.test", "hello", sqT0, nil},
		{"a1", "p2", "Alice <alice@x.example>", "me@home.test", "again", sqT0.Add(h), nil},
		{"a1", "r1", "Me <me@home.test>", "alice@x.example", "Re: hello", sqT0.Add(2 * h), []string{"In-Reply-To: <p1>", "References: <p1>"}},
		{"a1", "n1", "News <news@lists.example>", "me@home.test", "weekly", sqT0, []string{"List-Id: <weekly.lists.example>", "List-Unsubscribe: <mailto:u@lists.example>"}},
		{"a1", "g1", "GitHub <notifications@github.com>", "me@home.test", "[r] PR", sqT0, []string{"X-GitHub-Reason: mention"}},
		{"a1", "s1", "Shop <no-reply@shop.example>", "me@home.test", "Your order 42 has shipped", sqT0, nil},
		{"a2", "q1", "Alice <alice@x.example>", "me2@home.test", "other account", sqT0, nil},
		{"a2", "q2", "Me <me2@home.test>", "alice@x.example", "Re: other account", sqT0.Add(h), []string{"In-Reply-To: <q1>"}},
		{"a2", "q3", "Bol <info@bol.com>", "me2@home.test", "Uw bestelling", sqT0, nil},
	}
}

func sqOpen(t *testing.T) *Cache {
	c := thrOpen(t)
	c.SetOwners(map[string][]string{"a1": {"me@home.test"}, "a2": {"me2@home.test"}})
	return c
}

func TestQASendersSumOfCountsEqualsMessageCountPerAccount(t *testing.T) {
	c := sqOpen(t)
	sqJob(t, c, sqFixture())
	for acct, want := range map[string]int{"a1": 6, "a2": 3} {
		sum, msgs := sqSum(t, c, acct)
		if sum != want || msgs != want {
			t.Errorf("%s: SUM(n_msgs)=%d, messages=%d, want %d", acct, sum, msgs, want)
		}
	}
}

func TestQASendersRerunChangesNothing(t *testing.T) {
	c := sqOpen(t)
	sqJob(t, c, sqFixture())
	first := sqDump(t, c)
	for i := 0; i < 2; i++ {
		if err := c.RunSenders(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := sqDump(t, c); got != first {
		t.Errorf("RunSenders re-run changed the table:\n%s\nvs\n%s", first, got)
	}
	// A full rebuild (schema-bump style) gives the same table too.
	restartSenders(t, c)
	if err := c.RunSenders(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if got := sqDump(t, c); got != first {
		t.Errorf("rebuild differs:\n%s\nvs\n%s", first, got)
	}
}

func TestQASendersJobEqualsIncrementalResult(t *testing.T) {
	job := sqOpen(t)
	sqJob(t, job, sqFixture())
	for _, batch := range []bool{true, false} {
		inc := sqOpen(t)
		sqIncremental(t, inc, sqFixture(), batch)
		if a, b := sqDump(t, job), sqDump(t, inc); a != b {
			t.Errorf("batch=%v: job and incremental differ:\njob:\n%s\nincremental:\n%s", batch, a, b)
		}
	}
}

func TestQASendersMailArrivingDuringJobCountedOnce(t *testing.T) {
	c := sqOpen(t)
	ms := sqFixture()
	for _, m := range ms {
		sqInsert(t, c, m)
	}
	restartSenders(t, c) // snapshot: the nine messages above
	// Mail arrives after the snapshot, before and while the job runs: refresh counts it itself.
	late1 := sqMsg{"a1", "late1", "Alice <alice@x.example>", "me@home.test", "late", sqT0.Add(48 * time.Hour), nil}
	late2 := sqMsg{"a1", "late2", "Fresh <fresh@y.example>", "me@home.test", "new sender", sqT0.Add(49 * time.Hour), nil}
	sqIncremental(t, c, []sqMsg{late1}, true)
	if err := c.RunSenders(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	sqIncremental(t, c, []sqMsg{late2}, true) // and after the job finished
	sum, msgs := sqSum(t, c, "a1")
	if sum != msgs || msgs != 8 {
		t.Errorf("SUM(n_msgs)=%d messages=%d, want 8 both", sum, msgs)
	}
	if r := sndRowAcct(t, c, "a1", "alice@x.example"); r.NMsgs != 3 {
		t.Errorf("alice counted %d times, want 3", r.NMsgs)
	}
	if r := sndRowAcct(t, c, "a1", "fresh@y.example"); r.NMsgs != 1 {
		t.Errorf("fresh counted %d times, want 1", r.NMsgs)
	}
	// And the result is what a clean rebuild says.
	got := sqDump(t, c)
	restartSenders(t, c)
	if err := c.RunSenders(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if want := sqDump(t, c); got != want {
		t.Errorf("incremental+job differs from a clean job:\n%s\nvs\n%s", got, want)
	}
}

func sndRowAcct(t *testing.T, c *Cache, acct, addr string) SenderRow {
	t.Helper()
	rows, _, err := c.ListSenders(context.Background(), SenderQuery{Account: acct, Query: addr, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Addr == addr {
			return r
		}
	}
	t.Fatalf("no sender %s in %s", addr, acct)
	return SenderRow{}
}

func TestQASendersRestartMidJobDoesNotDoubleCount(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	c.SetOwners(map[string][]string{"a1": {"me@home.test"}})
	const n = 3*sendersBatch + 50
	var ms []sqMsg
	for i := 0; i < n; i++ {
		ms = append(ms, sqMsg{"a1", fmt.Sprintf("m%d", i), fmt.Sprintf("S%d <s%d@x.example>", i%17, i%17), "me@home.test", "hi", sqT0.Add(time.Duration(i) * time.Minute), nil})
	}
	for _, m := range ms {
		sqInsert(t, c, m)
	}
	restartSenders(t, c)

	// Cancel the job in the middle of its second batch transaction: the first
	// batch is committed, the second is rolled back.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int64
	real := c.now
	c.now = func() time.Time {
		if calls.Add(1) == 3 {
			cancel()
		}
		return real()
	}
	err = c.RunSenders(ctx, nil)
	c.now = real
	if err == nil {
		t.Fatal("RunSenders was not interrupted")
	}
	var processed, done int
	if err := c.db.QueryRow(`SELECT processed, done FROM backfill WHERE name = 'senders'`).Scan(&processed, &done); err != nil {
		t.Fatal(err)
	}
	if done != 0 || processed != sendersBatch {
		t.Fatalf("after the interrupt: processed=%d done=%d, want %d and 0 (a committed first batch only)", processed, done, sendersBatch)
	}
	if sum, _ := sqSum(t, c, "a1"); sum != processed {
		t.Fatalf("senders hold %d counts but the cursor says %d messages", sum, processed)
	}

	// Reopen the cache (process restart) and let the job finish.
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c2.Close() })
	c2.SetOwners(map[string][]string{"a1": {"me@home.test"}})
	if err := c2.RunSenders(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if sum, msgs := sqSum(t, c2, "a1"); sum != n || msgs != n {
		t.Errorf("after restart: SUM(n_msgs)=%d messages=%d, want %d", sum, msgs, n)
	}
	got := sqDump(t, c2)
	restartSenders(t, c2)
	if err := c2.RunSenders(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if want := sqDump(t, c2); got != want {
		t.Errorf("interrupted job differs from an uninterrupted one")
	}
}

// ---- owner kind is sticky ----

func sqOwnerSetup(t *testing.T) *Cache {
	c := sqOpen(t)
	sqJob(t, c, sqFixture())
	return c
}

func TestQAOwnerKindSurvivesNewMailRecomputeAndRestart(t *testing.T) {
	c := sqOwnerSetup(t)
	ctx := context.Background()
	// shop is transactional by rule; the owner says human.
	if old, err := c.SetSenderKind(ctx, "a1", "no-reply@shop.example", KindHuman); err != nil || old.Kind != KindTransactional || old.Source != SourceRule {
		t.Fatalf("set: %+v %v", old, err)
	}
	// New mail through refresh with every signal the rules look at.
	sqIncremental(t, c, []sqMsg{{"a1", "s2", "Shop <no-reply@shop.example>", "me@home.test", "Invoice 7 shipped", sqT0.Add(72 * time.Hour),
		[]string{"List-Id: <shop.example>", "List-Unsubscribe: <mailto:x@shop.example>", "X-GitHub-Reason: mention"}}}, true)
	r := sndRowAcct(t, c, "a1", "no-reply@shop.example")
	if r.Kind != KindHuman || r.KindSource != SourceOwner || r.NMsgs != 2 {
		t.Errorf("after new mail: %+v", r)
	}
	// Job restart (schema-bump style reset) keeps the owner's kind, recounts.
	restartSenders(t, c)
	if err := c.RunSenders(ctx, nil); err != nil {
		t.Fatal(err)
	}
	r = sndRowAcct(t, c, "a1", "no-reply@shop.example")
	if r.Kind != KindHuman || r.KindSource != SourceOwner || r.NMsgs != 2 {
		t.Errorf("after job restart: %+v", r)
	}
	// Other senders are rebuilt by rule, not kept.
	if g := sndRowAcct(t, c, "a1", "notifications@github.com"); g.KindSource != SourceRule {
		t.Errorf("rule sender source: %+v", g)
	}
}

func TestQAUndoSetSenderKindRestoresOldKindAndSource(t *testing.T) {
	c := sqOwnerSetup(t)
	ctx := context.Background()
	const shop = "no-reply@shop.example"

	// rule -> owner -> undo: back to rule.
	old, _ := c.SetSenderKind(ctx, "a1", shop, KindList)
	if err := c.RestoreSenderKind(ctx, "a1", shop, KindList, old); err != nil {
		t.Fatal(err)
	}
	if r := sndRowAcct(t, c, "a1", shop); r.Kind != KindTransactional || r.KindSource != SourceRule {
		t.Errorf("rule restore: %+v", r)
	}

	// owner -> owner -> undo: back to the first owner decision, still owner.
	_, _ = c.SetSenderKind(ctx, "a1", shop, KindList)
	old2, _ := c.SetSenderKind(ctx, "a1", shop, KindNotification)
	if old2.Kind != KindList || old2.Source != SourceOwner {
		t.Fatalf("old2: %+v", old2)
	}
	if err := c.RestoreSenderKind(ctx, "a1", shop, KindNotification, old2); err != nil {
		t.Fatal(err)
	}
	if r := sndRowAcct(t, c, "a1", shop); r.Kind != KindList || r.KindSource != SourceOwner {
		t.Errorf("owner restore: %+v", r)
	}

	// llm -> owner -> undo: back to the llm decision.
	if _, err := c.db.Exec(`UPDATE senders SET kind = 'human', kind_source = 'llm' WHERE account = 'a1' AND addr = ?`, shop); err != nil {
		t.Fatal(err)
	}
	old3, _ := c.SetSenderKind(ctx, "a1", shop, KindTransactional)
	if old3.Kind != KindHuman || old3.Source != SourceLLM {
		t.Fatalf("old3: %+v", old3)
	}
	if err := c.RestoreSenderKind(ctx, "a1", shop, KindTransactional, old3); err != nil {
		t.Fatal(err)
	}
	if r := sndRowAcct(t, c, "a1", shop); r.Kind != KindHuman || r.KindSource != SourceLLM {
		t.Errorf("llm restore: %+v", r)
	}
}

func TestQAUndoSetSenderKindRefusedWhenKindChangedAgain(t *testing.T) {
	c := sqOwnerSetup(t)
	ctx := context.Background()
	const shop = "no-reply@shop.example"
	old1, _ := c.SetSenderKind(ctx, "a1", shop, KindList)
	if _, err := c.SetSenderKind(ctx, "a1", shop, KindNotification); err != nil {
		t.Fatal(err)
	}
	if err := c.RestoreSenderKind(ctx, "a1", shop, KindList, old1); !errors.Is(err, ErrSenderChanged) {
		t.Errorf("undo of the older change: %v, want ErrSenderChanged", err)
	}
	if r := sndRowAcct(t, c, "a1", shop); r.Kind != KindNotification || r.KindSource != SourceOwner {
		t.Errorf("a refused undo changed the row: %+v", r)
	}
	// Same kind but no longer owner-set (source moved to llm): also refused.
	if _, err := c.db.Exec(`UPDATE senders SET kind_source = 'llm' WHERE account = 'a1' AND addr = ?`, shop); err != nil {
		t.Fatal(err)
	}
	if err := c.RestoreSenderKind(ctx, "a1", shop, KindNotification, old1); !errors.Is(err, ErrSenderChanged) {
		t.Errorf("undo over a non-owner source: %v", err)
	}
}

func TestQAUndoWithRulePreviousSourceRerunsRules(t *testing.T) {
	c := sqOwnerSetup(t)
	ctx := context.Background()
	const news = "news@lists.example" // list by rule
	old, err := c.SetSenderKind(ctx, "a1", news, KindHuman)
	if err != nil || old.Kind != KindList || old.Source != SourceRule {
		t.Fatalf("%+v %v", old, err)
	}
	// While owner-set, a GitHub-flagged message arrives: counts move, kind stays.
	sqIncremental(t, c, []sqMsg{{"a1", "n2", "News <news@lists.example>", "me@home.test", "weekly 2", sqT0.Add(24 * time.Hour),
		[]string{"List-Id: <weekly.lists.example>", "X-GitHub-Reason: subscribed"}}}, true)
	if r := sndRowAcct(t, c, "a1", news); r.Kind != KindHuman {
		t.Fatalf("owner kind moved: %+v", r)
	}
	if err := c.RestoreSenderKind(ctx, "a1", news, KindHuman, old); err != nil {
		t.Fatal(err)
	}
	// The rules run again on the current counts (a forged X-GitHub-Reason off
	// github.com no longer matters): list by rule.
	if r := sndRowAcct(t, c, "a1", news); r.Kind != KindList || r.KindSource != SourceRule {
		t.Errorf("after undo: %+v, want list by rule", r)
	}
}

// ---- kind rules ----

func TestQAKindRulesTable(t *testing.T) {
	cases := []struct {
		name string
		in   KindInputs
		want string
	}{
		{"github", KindInputs{Addr: "notifications@github.com"}, KindNotification},
		{"github subdomain", KindInputs{Addr: "noreply@mail.github.com"}, KindNotification},
		{"gitlab", KindInputs{Addr: "gitlab@mg.gitlab.com", NMsgs: 2, NAuto: 2}, KindNotification},
		{"linear", KindInputs{Addr: "notifications@linear.app", NMsgs: 2, NAuto: 2}, KindNotification},
		{"vanta", KindInputs{Addr: "no-reply@vanta.com", NMsgs: 2, NAuto: 2}, KindNotification},
		{"steady", KindInputs{Addr: "team@steady.space", NMsgs: 2, NAuto: 2}, KindNotification},
		{"betterstack", KindInputs{Addr: "alerts@betterstack.com", NMsgs: 2, NAuto: 2}, KindNotification},
		{"slack", KindInputs{Addr: "notification@slack.com", NMsgs: 2, NAuto: 2}, KindNotification},
		{"notifier even if replied", KindInputs{Addr: "notifications@github.com", NReplied: 3}, KindNotification},
		{"notifier even with order subject", KindInputs{Addr: "noreply@github.com", NTxn: 2}, KindNotification},
		{"forged x-github-reason on a non-github sender is ignored", KindInputs{Addr: "someone@corp.example", NMsgs: 1, NGH: 1}, KindHuman},
		{"replied: human", KindInputs{Addr: "info@bol.com", NReplied: 1}, KindHuman},
		{"replied beats list", KindInputs{Addr: "owner-x@lists.example", NList: 9, NReplied: 1}, KindHuman},
		{"amazon.nl", KindInputs{Addr: "order-update@amazon.nl"}, KindTransactional},
		{"amazon.de", KindInputs{Addr: "versandbestaetigung@amazon.de"}, KindTransactional},
		{"aliexpress.com", KindInputs{Addr: "transaction@notice.aliexpress.com"}, KindTransactional},
		{"bol.com", KindInputs{Addr: "info@bol.com"}, KindTransactional},
		{"noreply + order subject", KindInputs{Addr: "noreply@acme.example", NTxn: 1}, KindTransactional},
		{"no-reply + order subject", KindInputs{Addr: "no-reply@acme.example", NTxn: 1}, KindTransactional},
		{"do_not_reply + order subject", KindInputs{Addr: "do_not_reply@acme.example", NTxn: 1}, KindTransactional},
		{"noreply + order beats unsubscribe", KindInputs{Addr: "noreply@acme.example", NTxn: 1, NUnsub: 5}, KindTransactional},
		{"noreply + list-unsubscribe", KindInputs{Addr: "noreply@acme.example", NUnsub: 1}, KindNotification},
		{"noreply + list-id and unsubscribe", KindInputs{Addr: "noreply@acme.example", NList: 1, NUnsub: 1}, KindNotification},
		{"noreply alone", KindInputs{Addr: "noreply@acme.example"}, KindHuman},
		{"list-id", KindInputs{Addr: "dev@lists.example", NList: 1}, KindList},
		{"list-id and unsubscribe, not noreply", KindInputs{Addr: "dev@lists.example", NList: 1, NUnsub: 1}, KindList},
		{"unsubscribe on a campaign address, not noreply", KindInputs{Addr: "news@brand.example", NUnsub: 3}, KindList},
		{"unsubscribe on a plain address without a majority", KindInputs{Addr: "bob@brand.example", NMsgs: 5, NUnsub: 1}, KindHuman},
		{"order subject, not noreply", KindInputs{Addr: "bob@acme.example", NTxn: 3}, KindHuman},
		{"plain human", KindInputs{Addr: "alice@home.example"}, KindHuman},
		{"explicit domain wins over addr", KindInputs{Addr: "x@y.example", Domain: "github.com", NMsgs: 1, NGH: 1}, KindNotification},
	}
	for _, c := range cases {
		if got := ClassifySender(c.in); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestQATransactionalSubjectsInFourLanguages(t *testing.T) {
	yes := []string{
		"Your order 123 has shipped", "Invoice #42", "Receipt for your payment", "Re: shipping confirmation",
		"Bestelling 123 verzonden", "Uw factuur", "Je pakket is bezorgd",
		"Ihre Rechnung", "Rechnung Nr. 5", "Ihre Bestellbestätigung",
		"Votre commande", "Votre facture", "Livraison de votre colis",
		"Ihre Bestellung 123 ist unterwegs", // German for "your order": the DE words must match too
	}
	for _, s := range yes {
		if !IsTransactionalSubject(s) {
			t.Errorf("%q should have a transactional shape", s)
		}
	}
	for _, s := range []string{"hello", "lunch tomorrow?", "weekly digest", "meeting notes"} {
		if IsTransactionalSubject(s) {
			t.Errorf("%q should not have a transactional shape", s)
		}
	}
}

func TestQARegistrableDomainMultiPartTLDs(t *testing.T) {
	for in, want := range map[string]string{
		"a@mail.shop.co.uk":   "shop.co.uk",
		"a@shop.co.uk":        "shop.co.uk",
		"a@x.y.shop.com.au":   "shop.com.au",
		"a@shop.com.au":       "shop.com.au",
		"a@mail.amazon.co.uk": "amazon.co.uk",
		"a@amazon.nl":         "amazon.nl",
		"a@news.bol.com":      "bol.com",
		"a@co.uk":             "co.uk", // a bare public suffix has no registrable domain: itself
		"a@UPPER.Shop.CO.UK.": "shop.co.uk",
		"a@x.org":             "x.org",
	} {
		if got := RegistrableDomain(in); got != want {
			t.Errorf("RegistrableDomain(%q) = %q, want %q", in, got, want)
		}
	}
	if got := ClassifySender(KindInputs{Addr: "shipping@mail.amazon.co.uk"}); got != KindTransactional {
		t.Errorf("amazon.co.uk via subdomain: %s", got)
	}
	if got := ClassifySender(KindInputs{Addr: "noreply@mail.shop.com.au", NTxn: 1}); got != KindTransactional {
		t.Errorf("com.au noreply order: %s", got)
	}
}

// ---- n_replied_by_me ----

func sqReplyFixtures(t *testing.T, name string, ms []sqMsg, check func(t *testing.T, c *Cache), checkJob bool) {
	t.Run(name+"/refresh", func(t *testing.T) {
		c := sqOpen(t)
		sqIncremental(t, c, ms, false) // one transaction per message, in arrival order
		check(t, c)
	})
	if checkJob {
		t.Run(name+"/job", func(t *testing.T) {
			c := sqOpen(t)
			sqJob(t, c, ms)
			check(t, c)
		})
	}
}

func replied(t *testing.T, c *Cache, addr string) int {
	t.Helper()
	return sndRowAcct(t, c, "a1", addr).NRepliedByMe
}

func TestQAReplyViaInReplyToCreditsParentSenderOnce(t *testing.T) {
	ms := []sqMsg{
		{"a1", "p1", "Alice <alice@x.example>", "me@home.test", "q", sqT0, nil},
		{"a1", "p2", "Bob <bob@x.example>", "me@home.test", "other", sqT0, nil},
		// In-Reply-To and References name different messages: In-Reply-To wins, once.
		{"a1", "r1", "Me <me@home.test>", "alice@x.example", "Re: q", sqT0.Add(time.Hour), []string{"In-Reply-To: <p1>", "References: <p2> <p1>"}},
	}
	sqReplyFixtures(t, "in-reply-to", ms, func(t *testing.T, c *Cache) {
		if n := replied(t, c, "alice@x.example"); n != 1 {
			t.Errorf("alice credited %d times, want 1", n)
		}
		if n := replied(t, c, "bob@x.example"); n != 0 {
			t.Errorf("bob credited %d times, want 0", n)
		}
		if n := replied(t, c, "me@home.test"); n != 0 {
			t.Errorf("the owner credited itself %d times", n)
		}
	}, true)
}

func TestQAReplyViaReferencesUsesLastID(t *testing.T) {
	ms := []sqMsg{
		{"a1", "c1", "Carol <carol@x.example>", "me@home.test", "t", sqT0, nil},
		{"a1", "d1", "Dave <dave@x.example>", "me@home.test", "Re: t", sqT0.Add(time.Minute), nil},
		{"a1", "e1", "Erin <erin@x.example>", "me@home.test", "Re: t", sqT0.Add(2 * time.Minute), nil},
		{"a1", "r1", "Me <me@home.test>", "erin@x.example", "Re: t", sqT0.Add(time.Hour), []string{"References: <c1> <d1> <e1>"}},
	}
	sqReplyFixtures(t, "references", ms, func(t *testing.T, c *Cache) {
		if n := replied(t, c, "erin@x.example"); n != 1 {
			t.Errorf("erin (last reference) credited %d, want 1", n)
		}
		for _, a := range []string{"carol@x.example", "dave@x.example"} {
			if n := replied(t, c, a); n != 0 {
				t.Errorf("%s credited %d, want 0", a, n)
			}
		}
	}, true)
}

func TestQAReplyBeforeParentCreditsNobody(t *testing.T) {
	// Documented limit: the parent must be in the cache when the reply is
	// counted. Here the owner's reply is indexed first (a different folder
	// fetched first, say), the parent later, each in its own refresh.
	ms := []sqMsg{
		{"a1", "r1", "Me <me@home.test>", "alice@x.example", "Re: q", sqT0.Add(time.Hour), []string{"In-Reply-To: <p1>"}},
		{"a1", "p1", "Alice <alice@x.example>", "me@home.test", "q", sqT0, nil},
	}
	c := sqOpen(t)
	sqIncremental(t, c, ms, false)
	// Credit happens in the sweep at the end of the refresh, when the parent is
	// cached, so the order of arrival no longer matters.
	if n := replied(t, c, "alice@x.example"); n != 1 {
		t.Errorf("reply indexed before its parent credited alice %d times, want 1", n)
	}
	// Same refresh batch: the parent is in the table when the batch is flushed.
	c2 := sqOpen(t)
	sqIncremental(t, c2, ms, true)
	t.Logf("same-batch out-of-order reply credits alice %d time(s)", replied(t, c2, "alice@x.example"))
	// The job sees both messages at once and so does credit: a later full
	// rebuild is where such a reply is finally counted.
	c3 := sqOpen(t)
	sqJob(t, c3, ms)
	t.Logf("job over the same messages credits alice %d time(s)", replied(t, c3, "alice@x.example"))
}

func TestQAOwnerMailWithoutParentCreditsNobody(t *testing.T) {
	ms := []sqMsg{
		{"a1", "p1", "Alice <alice@x.example>", "me@home.test", "q", sqT0, nil},
		{"a1", "o1", "Me <me@home.test>", "alice@x.example", "fresh mail", sqT0.Add(time.Hour), nil},
		{"a1", "o2", "Me <me@home.test>", "alice@x.example", "Re: unknown", sqT0.Add(2 * time.Hour), []string{"In-Reply-To: <nowhere>", "References: <nowhere>"}},
		{"a1", "o3", "Me <me@home.test>", "alice@x.example", "Re: empty", sqT0.Add(3 * time.Hour), []string{"References:"}},
	}
	sqReplyFixtures(t, "no-parent", ms, func(t *testing.T, c *Cache) {
		if n := replied(t, c, "alice@x.example"); n != 0 {
			t.Errorf("alice credited %d, want 0", n)
		}
		if r := sndRowAcct(t, c, "a1", "me@home.test"); r.NMsgs != 3 || r.NRepliedByMe != 0 {
			t.Errorf("owner row: %+v", r)
		}
	}, true)
}

func TestQAMailingListThreadDoesNotCreditEveryParticipant(t *testing.T) {
	ms := []sqMsg{
		{"a1", "l1", "A <a@list.example>", "dev@lists.example", "[dev] topic", sqT0, []string{"List-Id: <dev.lists.example>"}},
		{"a1", "l2", "B <b@list.example>", "dev@lists.example", "Re: [dev] topic", sqT0.Add(time.Minute), []string{"List-Id: <dev.lists.example>", "In-Reply-To: <l1>", "References: <l1>"}},
		{"a1", "l3", "C <c@list.example>", "dev@lists.example", "Re: [dev] topic", sqT0.Add(2 * time.Minute), []string{"List-Id: <dev.lists.example>", "In-Reply-To: <l2>", "References: <l1> <l2>"}},
		{"a1", "r1", "Me <me@home.test>", "dev@lists.example", "Re: [dev] topic", sqT0.Add(time.Hour), []string{"In-Reply-To: <l3>", "References: <l1> <l2> <l3>"}},
	}
	sqReplyFixtures(t, "list-thread", ms, func(t *testing.T, c *Cache) {
		got := replied(t, c, "a@list.example") + replied(t, c, "b@list.example") + replied(t, c, "c@list.example")
		if got != 1 || replied(t, c, "c@list.example") != 1 {
			t.Errorf("credits a=%d b=%d c=%d, want only c=1", replied(t, c, "a@list.example"), replied(t, c, "b@list.example"), replied(t, c, "c@list.example"))
		}
	}, true)
}

// ---- caps ----

func TestQAPerAccountCapOf1000TagsAnd200SavedQueries(t *testing.T) {
	c := sqOpen(t)
	sqJob(t, c, sqFixture())
	ctx := context.Background()
	ids := []string{"p1"}
	for i := 0; i < maxTagsPerAcc; i++ {
		if _, err := c.AddTags(ctx, "a1", fmt.Sprintf("t%d", i), ids, ""); err != nil {
			t.Fatalf("tag %d: %v", i, err)
		}
	}
	if _, err := c.AddTags(ctx, "a1", "one-too-many", ids, ""); !errors.Is(err, ErrTagLimit) {
		t.Errorf("1001st tag: %v, want ErrTagLimit", err)
	}
	// An existing tag still works on more messages, and other accounts are unaffected.
	if added, err := c.AddTags(ctx, "a1", "t0", []string{"p2"}, ""); err != nil || len(added) != 1 {
		t.Errorf("existing tag at the cap: %v %v", added, err)
	}
	if _, err := c.AddTags(ctx, "a2", "one-too-many", []string{"q1"}, ""); err != nil {
		t.Errorf("other account: %v", err)
	}
	if tl, _ := c.ListTags(ctx, "a1"); len(tl) != maxTagsPerAcc {
		t.Errorf("%d tags listed", len(tl))
	}

	for i := 0; i < maxSavedPerAc; i++ {
		if err := c.SaveQuery(ctx, "a1", fmt.Sprintf("q%d", i), SavedFilters{Query: "x"}, ""); err != nil {
			t.Fatalf("query %d: %v", i, err)
		}
	}
	if err := c.SaveQuery(ctx, "a1", "one-too-many", SavedFilters{Query: "x"}, ""); !errors.Is(err, ErrTagLimit) {
		t.Errorf("201st saved query: %v, want ErrTagLimit", err)
	}
	if err := c.SaveQuery(ctx, "a1", "q0", SavedFilters{Query: "replaced"}, ""); err != nil {
		t.Errorf("replacing at the cap: %v", err)
	}
	if err := c.SaveQuery(ctx, "a2", "one-too-many", SavedFilters{Query: "x"}, ""); err != nil {
		t.Errorf("other account: %v", err)
	}
	if l, _ := c.ListSavedQueries(ctx, "a1"); len(l) != maxSavedPerAc {
		t.Errorf("%d saved queries listed", len(l))
	}
}

func TestQASavedQueryJSONCapAndNoteCap(t *testing.T) {
	c := thrOpen(t)
	ctx := context.Background()
	big := SavedFilters{Query: "x", Folder: strings.Repeat("f", maxSavedJSON)}
	if err := c.SaveQuery(ctx, "acc", "big", big, ""); !errors.Is(err, ErrTagLimit) {
		t.Errorf("saved query over 4KB: %v, want ErrTagLimit", err)
	}
	if _, err := c.GetSavedQuery(ctx, "acc", "big"); !errors.Is(err, ErrNoSavedQuery) {
		t.Errorf("an over-cap query was stored: %v", err)
	}
	ok := SavedFilters{Query: "x", Folder: strings.Repeat("f", maxSavedJSON-200)}
	if err := c.SaveQuery(ctx, "acc", "ok", ok, ""); err != nil {
		t.Errorf("saved query just under 4KB: %v", err)
	}
	if err := c.SaveQuery(ctx, "acc", "n500", SavedFilters{Query: "x"}, strings.Repeat("é", maxNoteRunes)); err != nil {
		t.Errorf("note of 500 runes: %v", err)
	}
	if err := c.SaveQuery(ctx, "acc", "n501", SavedFilters{Query: "x"}, strings.Repeat("é", maxNoteRunes+1)); !errors.Is(err, ErrTagLimit) {
		t.Errorf("note of 501 runes: %v, want ErrTagLimit", err)
	}
	if err := c.SaveQuery(ctx, "acc", "q513", SavedFilters{Query: strings.Repeat("a", maxQueryBytes+1)}, ""); !errors.Is(err, ErrQueryLimit) {
		t.Errorf("query over its cap: %v", err)
	}
}

func TestQAOwnerReplyCountsOnlyFromTheSentFolder(t *testing.T) {
	ms := []sqMsg{
		{"a1", "p1", "Alice <alice@x.example>", "me@home.test", "q", sqT0, nil},
		{"a1", "p2", "Bob <bob@x.example>", "me@home.test", "q2", sqT0, nil},
		{"a1", "p3", "Carol <carol@x.example>", "me@home.test", "q3", sqT0, nil},
		// From the owner, but filed in INBOX: a forged From looks exactly like this.
		{"a1", "r1", "Me <me@home.test>", "alice@x.example", "Re: q", sqT0.Add(time.Hour), []string{"In-Reply-To: <p1>"}},
		// In Sent, but not from an owner address.
		{"a1", "r2", "Mallory <mallory@evil.example>", "bob@x.example", "Re: q2", sqT0.Add(time.Hour), []string{"In-Reply-To: <p2>"}},
		// The genuine one: from the owner and in Sent.
		{"a1", "r3", "Me <me@home.test>", "carol@x.example", "Re: q3", sqT0.Add(time.Hour), []string{"In-Reply-To: <p3>"}},
	}
	c := sqOpen(t)
	sqJob(t, c, nil)
	for _, m := range ms {
		sqInsert(t, c, m)
	}
	for _, q := range []string{
		`INSERT INTO folders (account, folder, uidvalidity, attrs) VALUES ('a1', 'Sent', 1, '\Sent')`,
		`INSERT INTO membership (account, stable_id, folder, uid, uidvalidity) SELECT account, stable_id, 'Sent', rowid, 1 FROM messages WHERE stable_id IN ('r2', 'r3')`,
	} {
		if _, err := c.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	// The messages were inserted after the job snapshot: count them the way refresh does.
	restartSenders(t, c)
	if err := c.RunSenders(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	for addr, want := range map[string]int{"alice@x.example": 0, "bob@x.example": 0, "carol@x.example": 1} {
		if got := sndRowAcct(t, c, "a1", addr).NRepliedByMe; got != want {
			t.Errorf("%s credited %d, want %d", addr, got, want)
		}
	}
}

func TestQAReplyBeforeParentIsNeverCreditedLater(t *testing.T) {
	// Documented limit: a reply is looked at once. If its parent is not cached
	// when it is looked at, it credits nobody, and the parent arriving later
	// does not change that (until the senders table is rebuilt).
	c := sqOpen(t)
	sqJob(t, c, nil)
	reply := sqMsg{"a1", "r1", "Me <me@home.test>", "alice@x.example", "Re: q", sqT0.Add(time.Hour), []string{"In-Reply-To: <p1>"}}
	parent := sqMsg{"a1", "p1", "Alice <alice@x.example>", "me@home.test", "q", sqT0, nil}
	sqIncremental(t, c, []sqMsg{reply}, true)
	sqIncremental(t, c, []sqMsg{parent}, true)
	if n := sndRowAcct(t, c, "a1", "alice@x.example").NRepliedByMe; n != 0 {
		t.Errorf("a reply that arrived before its parent credited alice %d times, want 0", n)
	}
}

func TestQANotifierDomainNeedsAnAutomatedMajority(t *testing.T) {
	for _, tc := range []struct {
		name         string
		nmsgs, nauto int
		want         string
	}{
		{"all automated", 4, 4, KindNotification},
		{"three of four", 4, 3, KindNotification},
		{"exactly half is not a majority", 4, 2, KindHuman},
		{"a person at the notifier's domain", 4, 0, KindHuman},
		{"one automated of three", 3, 1, KindHuman},
	} {
		if got := ClassifySender(KindInputs{Addr: "someone@linear.app", NMsgs: tc.nmsgs, NAuto: tc.nauto}); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
	if got := ClassifySender(KindInputs{Addr: "someone@linear.app", NMsgs: 4, NAuto: 4, NReplied: 1}); got != KindHuman {
		t.Errorf("replied to a notifier-domain sender: %s, want human", got)
	}
	if got := ClassifySender(KindInputs{Addr: "bob@github.com", NMsgs: 1, NGH: 1}); got != KindNotification {
		t.Errorf("x-github-reason at github.com: %s", got)
	}
}
