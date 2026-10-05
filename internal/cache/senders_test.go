package cache

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"
)

func TestClassifySender(t *testing.T) {
	cases := []struct {
		name string
		in   KindInputs
		want string
	}{
		{"github notifications", KindInputs{Addr: "notifications@github.com"}, KindNotification},
		{"gh reason on any sender", KindInputs{Addr: "bob@corp.example", NGH: 1}, KindNotification},
		{"linear", KindInputs{Addr: "notifications@linear.app"}, KindNotification},
		{"replied beats noreply+list", KindInputs{Addr: "noreply@x.example", NList: 3, NReplied: 1}, KindHuman},
		{"notifier beats replied", KindInputs{Addr: "notifications@github.com", NReplied: 2}, KindNotification},
		{"amazon any tld", KindInputs{Addr: "auto-confirm@amazon.co.uk"}, KindTransactional},
		{"amazonaws is not amazon", KindInputs{Addr: "x@amazonaws.com"}, KindHuman},
		{"bol", KindInputs{Addr: "info@bol.com"}, KindTransactional},
		{"noreply with order subject", KindInputs{Addr: "no-reply@shop.example", NTxn: 1}, KindTransactional},
		{"noreply with list-unsubscribe", KindInputs{Addr: "noreply@brand.example", NUnsub: 4}, KindNotification},
		{"do-not-reply with list id", KindInputs{Addr: "do-not-reply@brand.example", NList: 2}, KindNotification},
		{"noreply without signals", KindInputs{Addr: "noreply@brand.example"}, KindHuman},
		{"list id", KindInputs{Addr: "owner-list@lists.example", NList: 5}, KindList},
		{"human", KindInputs{Addr: "alice@home.example", NMsgs: 3}, KindHuman},
		{"order subject alone stays human", KindInputs{Addr: "alice@home.example", NTxn: 2}, KindHuman},
	}
	for _, c := range cases {
		if got := ClassifySender(c.in); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestRegistrableDomain(t *testing.T) {
	for in, want := range map[string]string{
		"a@mail.github.com":  "github.com",
		"a@x.co.uk":          "x.co.uk",
		"a@deep.sub.bol.com": "bol.com",
		"a@user.github.io":   "user.github.io", // private suffix: the user's page is the registrable unit
		"a@localhost":        "localhost",
		"a@[127.0.0.1]":      "127.0.0.1",
		"A@Example.COM":      "example.com",
		"":                   "",
	} {
		if got := RegistrableDomain(in); got != want {
			t.Errorf("RegistrableDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func sndAdd(t *testing.T, c *Cache, id, mid, from, to, subj string, when time.Time, hdr ...string) {
	t.Helper()
	raw := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s>\r\n", from, to, subj, when.Format(time.RFC1123Z), mid)
	for _, h := range hdr {
		raw += h + "\r\n"
	}
	raw += "\r\nbody\r\n"
	sum, err := c.putBlob([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := c.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := insertMessageTx(context.Background(), tx, "acc", headerInfo{stableID: id, size: int64(len(raw)), internal: when}, sum, parseMessage([]byte(raw))); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// restartSenders makes the senders job start over on the current messages, the
// way a schema bump would.
func restartSenders(t *testing.T, c *Cache) {
	t.Helper()
	if _, err := c.db.Exec(`DELETE FROM backfill WHERE name = 'senders'`); err != nil {
		t.Fatal(err)
	}
	if err := c.initSenders(); err != nil {
		t.Fatal(err)
	}
}

func sndRow(t *testing.T, c *Cache, addr string) SenderRow {
	t.Helper()
	rows, _, err := c.ListSenders(context.Background(), SenderQuery{Account: "acc", Query: addr, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Addr == addr {
			return r
		}
	}
	t.Fatalf("no sender %s", addr)
	return SenderRow{}
}

func sndFixture(t *testing.T) *Cache {
	c := thrOpen(t)
	c.SetOwners(map[string][]string{"acc": {"me@home.test"}})
	t0 := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	sndAdd(t, c, "m1", "a1@x", "Alice <alice@x.example>", "me@home.test", "hello", t0)
	sndAdd(t, c, "m2", "a2@x", "Alice A. <ALICE@x.example>", "me@home.test", "again", t0.Add(time.Hour))
	sndAdd(t, c, "m3", "me1@x", "Me <me@home.test>", "alice@x.example", "Re: hello", t0.Add(2*time.Hour), "In-Reply-To: <a1@x>", "References: <a1@x>")
	sndAdd(t, c, "m4", "n1@x", "News <news@lists.example>", "me@home.test", "weekly", t0, "List-Id: <weekly.lists.example>", "List-Unsubscribe: <mailto:u@lists.example>")
	sndAdd(t, c, "m5", "g1@x", "GitHub <notifications@github.com>", "me@home.test", "[r] PR", t0, "X-GitHub-Reason: mention")
	sndAdd(t, c, "m6", "s1@x", "Shop <no-reply@shop.example>", "me@home.test", "Your order 42 has shipped", t0)
	sndAdd(t, c, "m7", "r1@x", "Me <me@home.test>", "news@lists.example", "Re: weekly", t0.Add(time.Hour), "In-Reply-To: <zzz@nowhere>")
	return c
}

func checkFixture(t *testing.T, c *Cache) {
	a := sndRow(t, c, "alice@x.example")
	if a.NMsgs != 2 || a.NRepliedByMe != 1 || a.Kind != KindHuman || a.KindSource != SourceRule || a.NToMe != 2 {
		t.Errorf("alice: %+v", a)
	}
	if a.Name != "Alice" && a.Name != "Alice A." {
		t.Errorf("alice name %q", a.Name)
	}
	if n := sndRow(t, c, "news@lists.example"); n.Kind != KindList || n.ListID != "weekly.lists.example" || n.NRepliedByMe != 0 || !n.HasUnsub {
		t.Errorf("news: %+v", n)
	}
	if g := sndRow(t, c, "notifications@github.com"); g.Kind != KindNotification {
		t.Errorf("github: %+v", g)
	}
	if s := sndRow(t, c, "no-reply@shop.example"); s.Kind != KindTransactional {
		t.Errorf("shop: %+v", s)
	}
	if me := sndRow(t, c, "me@home.test"); me.NMsgs != 2 || me.NRepliedByMe != 0 {
		t.Errorf("me: %+v", me)
	}
	rows, total, err := c.ListSenders(context.Background(), SenderQuery{Account: "acc", Kind: KindTransactional, NeverReplied: true})
	if err != nil || total != 1 || rows[0].Addr != "no-reply@shop.example" {
		t.Errorf("gate query: %v %d %+v", err, total, rows)
	}
}

func TestSendersJobCountsEveryMessageOnce(t *testing.T) {
	c := sndFixture(t)
	// Old rows predate the column: the job must fill list_unsub from the blob.
	if _, err := c.db.Exec(`UPDATE messages SET list_unsub = NULL`); err != nil {
		t.Fatal(err)
	}
	restartSenders(t, c)
	if err := c.RunSenders(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	checkFixture(t, c)
	var nulls, total int
	_ = c.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE list_unsub IS NULL`).Scan(&nulls)
	_ = c.db.QueryRow(`SELECT SUM(n_msgs) FROM senders`).Scan(&total)
	if nulls != 0 || total != 7 {
		t.Errorf("null list_unsub = %d, summed n_msgs = %d (want 0, 7)", nulls, total)
	}
	// Running again changes nothing (done).
	if err := c.RunSenders(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	_ = c.db.QueryRow(`SELECT SUM(n_msgs) FROM senders`).Scan(&total)
	if total != 7 {
		t.Errorf("second run counted again: %d", total)
	}
}

func TestSendersIncrementalMatchesJob(t *testing.T) {
	// Incremental path: the job row is created on an empty cache, so every
	// message is newer than its snapshot and is counted at insert.
	c := thrOpen(t)
	c.SetOwners(map[string][]string{"acc": {"me@home.test"}})
	if st, _ := c.SendersStatus(context.Background()); !st.Complete {
		t.Fatalf("job on an empty cache should be complete: %+v", st)
	}
	sndFixtureInto(t, c)
	checkFixture(t, c)
}

// sndFixtureInto adds the fixture's messages through the batch the way refresh
// does (one transaction, observe then flush).
func sndFixtureInto(t *testing.T, c *Cache) *Cache {
	t.Helper()
	src := sndFixture(t)
	rows, err := src.db.Query(`SELECT stable_id, blob_sha256 FROM messages ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	type m struct{ id, sum string }
	var ms []m
	for rows.Next() {
		var x m
		_ = rows.Scan(&x.id, &x.sum)
		ms = append(ms, x)
	}
	_ = rows.Close()
	ctx := context.Background()
	tx, err := c.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	sb := c.newSenderBatch()
	for _, x := range ms {
		raw, err := readFile(src.BlobPath(x.sum))
		if err != nil {
			t.Fatal(err)
		}
		sum, _ := c.putBlob(raw)
		p := parseMessage(raw)
		rid, ins, err := insertMessageRowTx(ctx, tx, "acc", headerInfo{stableID: x.id, size: int64(len(raw)), internal: p.Date}, sum, p)
		if err != nil || !ins {
			t.Fatal(err, ins)
		}
		sb.observe("acc", rid, headerInfo{stableID: x.id, internal: p.Date}, p)
	}
	if err := sb.flushTx(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestOwnerKindSurvivesRulesAndRestart(t *testing.T) {
	c := sndFixture(t)
	restartSenders(t, c)
	if err := c.RunSenders(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	old, err := c.SetSenderKind(ctx, "acc", "News <NEWS@lists.example>", KindHuman)
	if err != nil || old.Kind != KindList || old.Source != SourceRule {
		t.Fatalf("SetSenderKind: %+v %v", old, err)
	}
	// A new list message must not flip it back.
	tx, _ := c.db.Begin()
	sb := c.newSenderBatch()
	sb.add("acc", "News <news@lists.example>", "me@home.test", "weekly 2", 0, time.Now().Unix(), "<weekly.lists.example>", false, false, "", "")
	if err := sb.flushTx(ctx, tx); err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit()
	if r := sndRow(t, c, "news@lists.example"); r.Kind != KindHuman || r.KindSource != SourceOwner || r.NMsgs != 2 {
		t.Errorf("after rule pass: %+v", r)
	}
	// A restart of the job (schema bump) keeps the owner's decision.
	restartSenders(t, c)
	if err := c.RunSenders(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if r := sndRow(t, c, "news@lists.example"); r.KindSource != SourceOwner || r.Kind != KindHuman || r.NMsgs != 1 {
		t.Errorf("after restart: %+v", r)
	}
	// Undo: wrong current kind is refused, right one restores the rule.
	if err := c.RestoreSenderKind(ctx, "acc", "news@lists.example", KindList, old); err != ErrSenderChanged {
		t.Errorf("restore with stale kind: %v", err)
	}
	if err := c.RestoreSenderKind(ctx, "acc", "news@lists.example", KindHuman, old); err != nil {
		t.Fatal(err)
	}
	if r := sndRow(t, c, "news@lists.example"); r.KindSource != SourceRule || r.Kind != KindList {
		t.Errorf("after restore: %+v", r)
	}
	if _, err := c.SetSenderKind(ctx, "acc", "nobody@x.example", KindList); err != ErrSenderNotFound {
		t.Errorf("unknown sender: %v", err)
	}
}

func TestSendersQueryFilters(t *testing.T) {
	c := sndFixture(t)
	restartSenders(t, c)
	if err := c.RunSenders(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	addrs := func(q SenderQuery) []string {
		q.Account = "acc"
		rows, _, err := c.ListSenders(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, r.Addr)
		}
		return out
	}
	if got := addrs(SenderQuery{MinMsgs: 2}); !slices.Equal(got, []string{"alice@x.example", "me@home.test"}) {
		t.Errorf("min_msgs: %v", got)
	}
	if got := addrs(SenderQuery{Query: "GITHUB"}); !slices.Equal(got, []string{"notifications@github.com"}) {
		t.Errorf("query by domain: %v", got)
	}
	if got := addrs(SenderQuery{Query: "alice a"}); !slices.Equal(got, []string{"alice@x.example"}) && len(got) != 0 {
		t.Errorf("query by name: %v", got)
	}
	if got := addrs(SenderQuery{Query: "100%"}); len(got) != 0 {
		t.Errorf("LIKE metacharacters must be literal: %v", got)
	}
	if got := addrs(SenderQuery{NeverReplied: true, Kind: KindList}); !slices.Equal(got, []string{"news@lists.example"}) {
		t.Errorf("never replied list: %v", got)
	}
	if got := addrs(SenderQuery{Since: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}); len(got) != 0 {
		t.Errorf("since: %v", got)
	}
	if _, _, err := c.ListSenders(ctx, SenderQuery{Account: "acc", Sort: "nope"}); err == nil {
		t.Error("unknown sort accepted")
	}
	if got := addrs(SenderQuery{Limit: 2}); len(got) != 2 {
		t.Errorf("limit: %v", got)
	}
}
