package cache

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
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
		{"forged gh reason is ignored off github.com", KindInputs{Addr: "bob@corp.example", NMsgs: 1, NGH: 1}, KindHuman},
		{"gh reason at github.com", KindInputs{Addr: "octocat@github.com", NMsgs: 1, NGH: 1}, KindNotification},
		{"owner replied beats notifier domain", KindInputs{Addr: "jane@atlassian.com", NMsgs: 4, NAuto: 4, NReplied: 1}, KindHuman},
		{"github addresses stay notification even if replied", KindInputs{Addr: "noreply@github.com", NReplied: 1}, KindNotification},
		{"notifier domain needs automated majority", KindInputs{Addr: "bob@cloudflare.com", NMsgs: 3, NAuto: 1}, KindHuman},
		{"notifier domain automated majority", KindInputs{Addr: "alerts@cloudflare.com", NMsgs: 3, NAuto: 3}, KindNotification},
		{"private suffix is not the shop", KindInputs{Addr: "x@ups.github.io"}, KindHuman},
		{"private suffix ebay", KindInputs{Addr: "x@ebay.vercel.app"}, KindHuman},
		{"piano.reply is not noreply", KindInputs{Addr: "piano.reply@x.example", NTxn: 1, NList: 0, NUnsub: 1}, KindHuman},
		{"no.reply separated", KindInputs{Addr: "no.reply@x.example", NTxn: 1}, KindTransactional},
		{"linear", KindInputs{Addr: "notifications@linear.app", NMsgs: 2, NAuto: 2}, KindNotification},
		{"replied beats noreply+list", KindInputs{Addr: "noreply@x.example", NList: 3, NReplied: 1}, KindHuman},
		{"github notifications beats replied", KindInputs{Addr: "notifications@github.com", NReplied: 2}, KindNotification},
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

func TestClassifyMarketing(t *testing.T) {
	cases := []struct {
		name string
		in   KindInputs
		want string
	}{
		// Marketing that read as transactional (shop domain) or human.
		{"shop promotion@, unsub", KindInputs{Addr: "promotion@aliexpress.com", NMsgs: 60, NUnsub: 60}, KindList},
		{"shop campaign address, unsub majority", KindInputs{Addr: "ae-market.ae3@deals.aliexpress.com", NMsgs: 17, NUnsub: 17}, KindList},
		{"shop store-news@ with unsub majority", KindInputs{Addr: "store-news@amazon.nl", NMsgs: 247, NUnsub: 240}, KindList},
		{"shop offers@ any tld", KindInputs{Addr: "amazon-offers@amazon.co.uk", NMsgs: 21, NList: 21}, KindList},
		{"shop promotionN@", KindInputs{Addr: "promotion5@amazon.de", NMsgs: 12, NUnsub: 12}, KindList},
		{"shop newsletter@ with list id", KindInputs{Addr: "newsletter@update.thuisbezorgd.nl", NMsgs: 48, NList: 48, NUnsub: 48}, KindList},
		{"shop email.campaign@", KindInputs{Addr: "email.campaign@sg.booking.com", NMsgs: 89, NUnsub: 80}, KindList},
		{"newsletters-noreply@", KindInputs{Addr: "newsletters-noreply@social.example", NMsgs: 133, NUnsub: 133, NTxn: 2}, KindList},
		{"unsub majority, plain address", KindInputs{Addr: "info@mail.shop.example", NMsgs: 65, NUnsub: 60, NTxn: 3}, KindList},
		{"newsletter@ with unsub", KindInputs{Addr: "newsletter@gamerant.example", NMsgs: 115, NUnsub: 115}, KindList},
		{"news@ with unsub", KindInputs{Addr: "news@studio.example", NMsgs: 185, NUnsub: 185}, KindList},
		{"hello@ with unsub majority", KindInputs{Addr: "hello@app.example", NMsgs: 74, NUnsub: 74}, KindList},
		{"digest sender with unsub", KindInputs{Addr: "digest@quora.example", NMsgs: 476, NUnsub: 476}, KindList},
		// Must not change.
		{"order confirmation at shop", KindInputs{Addr: "auto-bevestiging@amazon.nl", NMsgs: 234, NTxn: 230}, KindTransactional},
		{"shipping at shop, no headers", KindInputs{Addr: "verzending-volgen@amazon.nl", NMsgs: 556, NTxn: 540}, KindTransactional},
		{"shop order mail with stray unsub", KindInputs{Addr: "automail@bol.com", NMsgs: 272, NTxn: 250, NUnsub: 272}, KindTransactional},
		{"carrier", KindInputs{Addr: "noreply@dhlparcel.nl", NMsgs: 98, NTxn: 90}, KindTransactional},
		{"carrier with list-id and parcel subjects", KindInputs{Addr: "notificatie@edm.postnl.nl", NMsgs: 49, NList: 49, NUnsub: 49, NTxn: 40}, KindTransactional},
		{"payment processor", KindInputs{Addr: "service@paypal.com", NMsgs: 1046, NTxn: 900}, KindTransactional},
		{"shop transaction@ no unsub", KindInputs{Addr: "transaction@notice.aliexpress.com", NMsgs: 46, NTxn: 20}, KindTransactional},
		{"noreply with unsub stays notification", KindInputs{Addr: "notifications-noreply@social.example", NMsgs: 300, NUnsub: 300}, KindNotification},
		{"noreply with order subjects", KindInputs{Addr: "noreply@brand.example", NMsgs: 10, NTxn: 8, NUnsub: 10}, KindTransactional},
		{"newsletter-like address without bulk evidence at unknown domain", KindInputs{Addr: "news@home.example", NMsgs: 3}, KindHuman},
		{"a person with an occasional unsub", KindInputs{Addr: "alice@home.example", NMsgs: 20, NUnsub: 2}, KindHuman},
		{"newsletter@ but mostly invoices", KindInputs{Addr: "newsletter@shop.example", NMsgs: 10, NUnsub: 10, NTxn: 8}, KindHuman},
		{"owner replied beats marketing", KindInputs{Addr: "newsletter@shop.example", NMsgs: 10, NUnsub: 10, NReplied: 1}, KindHuman},
		{"shop campaign local part without bulk evidence", KindInputs{Addr: "store-news@amazon.nl", NMsgs: 20}, KindList},
		{"shop campaign local part, mostly order subjects", KindInputs{Addr: "store-news@amazon.nl", NMsgs: 20, NTxn: 15}, KindTransactional},
		{"shop market stream, no headers", KindInputs{Addr: "ae-market.ae6@mail.aliexpress.com", NMsgs: 22}, KindList},
		{"booking confirmed with unsub at a shop domain", KindInputs{Addr: "customer.service@booking.com", NMsgs: 12, NTxn: 12, NUnsub: 12}, KindTransactional},
		{"shop unsub majority with a few order subjects, no list-id", KindInputs{Addr: "service@mail.shop.example", NMsgs: 5, NTxn: 1, NUnsub: 5}, KindList},
		{"carrier unsub majority with an order subject", KindInputs{Addr: "info@dhl.com", NMsgs: 5, NTxn: 1, NUnsub: 5}, KindTransactional},
		{"two messages, one unsub, as before", KindInputs{Addr: "info@brand.example", NMsgs: 2, NUnsub: 1}, KindHuman},
		{"two messages, both unsub, as before", KindInputs{Addr: "info@brand.example", NMsgs: 2, NUnsub: 2}, KindHuman},
		{"two messages at a shop, both unsub", KindInputs{Addr: "info@amazon.nl", NMsgs: 2, NUnsub: 2}, KindTransactional},
		{"mailer-daemon is not marketing", KindInputs{Addr: "mailer-daemon@brand.example", NMsgs: 2, NUnsub: 1}, KindHuman},
		{"mailer-daemon with bulk evidence", KindInputs{Addr: "mailer-daemon@brand.example", NMsgs: 1, NList: 1}, KindList}, // List-Id rule, not marketing
		{"renewsletter is not the word newsletter", KindInputs{Addr: "renewsletter@x.example", NMsgs: 3, NUnsub: 1}, KindHuman},
	}
	for _, c := range cases {
		if got := ClassifySender(c.in); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestIsTransactionalSubject(t *testing.T) {
	for subj, want := range map[string]bool{
		"Your order has shipped":                   true,
		"Order #1234567 confirmed":                 true,
		"Booking confirmation":                     true,
		"Your tickets for Saturday":                true,
		"Uw bestelling is bezorgd":                 true,
		"Bestätigung Ihrer Bestellung":             true,
		"Payment received":                         true,
		"Free shipping on your order":              false,
		"Delivery deals this week":                 false,
		"Order now and save 20% off":               false,
		"Free delivery, order 171-1234567-1234567": true,
		"Get US $8.00 off your order":              false,
		"Weekly digest":                            false,
	} {
		if got := IsTransactionalSubject(subj); got != want {
			t.Errorf("%q: got %v, want %v", subj, got, want)
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
	// A forged owner From in INBOX answering an attacker's mail: not a reply.
	sndAdd(t, c, "m8", "mal1@x", "Mallory <mallory@evil.example>", "me@home.test", "hi", t0)
	sndAdd(t, c, "m9", "forged@x", "Me <me@home.test>", "mallory@evil.example", "Re: hi", t0.Add(time.Hour), "In-Reply-To: <mal1@x>")
	return c
}

// sndSent puts the owner's genuine replies (m3, m7) in the Sent folder; m9 stays in INBOX.
func sndSent(t *testing.T, c *Cache) {
	t.Helper()
	for _, q := range []string{
		`INSERT OR REPLACE INTO folders (account, folder, uidvalidity, attrs) VALUES ('acc', 'Sent Items', 1, '\Sent')`,
		`INSERT OR REPLACE INTO folders (account, folder, uidvalidity, attrs) VALUES ('acc', 'INBOX', 1, '')`,
		`INSERT OR REPLACE INTO membership (account, stable_id, folder, uid, uidvalidity) VALUES ('acc', 'm3', 'Sent Items', 1, 1), ('acc', 'm7', 'Sent Items', 2, 1), ('acc', 'm9', 'INBOX', 1, 1)`,
	} {
		if _, err := c.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
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
	if me := sndRow(t, c, "me@home.test"); me.NMsgs != 3 || me.NRepliedByMe != 0 {
		t.Errorf("me: %+v", me)
	}
	if m := sndRow(t, c, "mallory@evil.example"); m.NRepliedByMe != 0 || m.Kind != KindHuman {
		t.Errorf("spoofed owner From in INBOX credited a reply: %+v", m)
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
	sndSent(t, c)
	restartSenders(t, c)
	if err := c.RunSenders(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	checkFixture(t, c)
	var nulls, total int
	_ = c.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE list_unsub IS NULL`).Scan(&nulls)
	_ = c.db.QueryRow(`SELECT SUM(n_msgs) FROM senders`).Scan(&total)
	if nulls != 0 || total != 9 {
		t.Errorf("null list_unsub = %d, summed n_msgs = %d (want 0, 9)", nulls, total)
	}
	// Running again changes nothing (done).
	if err := c.RunSenders(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	_ = c.db.QueryRow(`SELECT SUM(n_msgs) FROM senders`).Scan(&total)
	if total != 9 {
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
	sndSent(t, c)
	if err := c.creditSentReplies(context.Background(), "acc"); err != nil {
		t.Fatal(err)
	}
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
	sndSent(t, c)
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
	sb.add("acc", "News <news@lists.example>", "me@home.test", "weekly 2", 0, time.Now().Unix(), "<weekly.lists.example>", false, false)
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
	if got := addrs(SenderQuery{MinMsgs: 2}); !slices.Equal(got, []string{"me@home.test", "alice@x.example"}) {
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

func TestParentLookupRefusesAmbiguousAndLateParents(t *testing.T) {
	c := thrOpen(t)
	c.SetOwners(map[string][]string{"acc": {"me@home.test"}})
	t0 := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	// Two different senders reuse one Message-ID: ambiguous, nobody credited.
	sndAdd(t, c, "p1", "dup@x", "A <a@x.example>", "me@home.test", "s", t0)
	sndAdd(t, c, "p2", "dup@x", "B <b@x.example>", "me@home.test", "s", t0)
	// A parent that arrives after the reply.
	sndAdd(t, c, "late", "late@x", "L <l@x.example>", "me@home.test", "s", t0.Add(5*time.Hour))
	// A clean parent.
	sndAdd(t, c, "ok", "ok@x", "O <o@x.example>", "me@home.test", "s", t0)
	sndAdd(t, c, "r1", "r1@x", "Me <me@home.test>", "a@x.example", "Re: s", t0.Add(time.Hour), "In-Reply-To: <dup@x>")
	sndAdd(t, c, "r2", "r2@x", "Me <me@home.test>", "l@x.example", "Re: s", t0.Add(time.Hour), "In-Reply-To: <late@x>")
	sndAdd(t, c, "r3", "r3@x", "Me <me@home.test>", "o@x.example", "Re: s", t0.Add(time.Hour), "In-Reply-To: <ok@x>")
	for _, q := range []string{
		`INSERT INTO folders (account, folder, uidvalidity, attrs) VALUES ('acc', 'Sent', 1, '')`,
		`INSERT INTO membership (account, stable_id, folder, uid, uidvalidity) VALUES ('acc','r1','Sent',1,1),('acc','r2','Sent',2,1),('acc','r3','Sent',3,1)`,
	} {
		if _, err := c.db.Exec(q); err != nil { // no \Sent attribute: the name "Sent" is the fallback
			t.Fatal(err)
		}
	}
	restartSenders(t, c)
	if err := c.RunSenders(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	for addr, want := range map[string]int{"a@x.example": 0, "b@x.example": 0, "l@x.example": 0, "o@x.example": 1} {
		if got := sndRow(t, c, addr).NRepliedByMe; got != want {
			t.Errorf("%s n_replied_by_me = %d, want %d", addr, got, want)
		}
	}
}

func TestToMeIsExactAndAddressesAreValidated(t *testing.T) {
	b := thrOpen(t).newSenderBatch()
	b.owners["acc"] = []string{"me@home.test"}
	b.add("acc", "A <a@x.example>", "Bob <notme@home.test>, c@x.example", "s", 0, 1, "", false, false)
	b.add("acc", "B <b@x.example>", "Me <ME@home.test>", "s", 0, 1, "", false, false)
	if d := b.deltas[senderKey{"acc", "a@x.example"}]; d.toMe != 0 {
		t.Errorf("substring address matched: %+v", d)
	}
	if d := b.deltas[senderKey{"acc", "b@x.example"}]; d.toMe != 1 {
		t.Errorf("exact address missed: %+v", d)
	}
	long := strings.Repeat("a", 330) + "@x.example"
	for _, bad := range []string{long, "a\x01b@x.example", "a\u202eb@x.example", "a b@x.example"} {
		b.add("acc", bad, "", "s", 0, 1, "", false, false)
	}
	if len(b.deltas) != 2 {
		t.Errorf("invalid addresses were stored: %d keys", len(b.deltas))
	}
}

// recountFixture is a cache whose senders table was counted by older rules: a
// promo sender whose subjects were all counted as order mail, plus an owner row
// and an llm row, with the recount marker absent.
func recountFixture(t *testing.T, dir string) *Cache {
	t.Helper()
	c, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	t0 := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 2*sendersBatch+30; i++ {
		sndAdd(t, c, fmt.Sprintf("p%d", i), fmt.Sprintf("p%d@x", i), "Shop <promotion@ebay.de>", "me@home.test", "Get US $8.00 off your order", t0.Add(time.Duration(i)*time.Minute), "List-Unsubscribe: <mailto:u@x.example>")
		sndAdd(t, c, fmt.Sprintf("q%d", i), fmt.Sprintf("q%d@x", i), "Store <store-news@ikea.nl>", "me@home.test", "Deals for you", t0.Add(time.Duration(i)*time.Minute))
	}
	sndAdd(t, c, "o1", "o1@x", "Own <owned@x.example>", "me@home.test", "hi", t0)
	sndAdd(t, c, "l1", "l1@x", "Llm <llm@x.example>", "me@home.test", "hi", t0)
	// Stale rows, as the old rules left them.
	for _, q := range []string{
		`DELETE FROM senders`,
		`INSERT INTO senders (account, addr, domain, n_msgs, n_unsub, n_txn_subj, kind, kind_source) VALUES ('acc', 'promotion@ebay.de', 'ebay.de', 260, 260, 260, 'transactional', 'rule')`,
		`INSERT INTO senders (account, addr, domain, n_msgs, kind, kind_source) VALUES ('acc', 'owned@x.example', 'x.example', 1, 'list', 'owner')`,
		`INSERT INTO senders (account, addr, domain, n_msgs, kind, kind_source) VALUES ('acc', 'llm@x.example', 'x.example', 1, 'notification', 'llm')`,
		`DELETE FROM backfill WHERE name IN ('senders_recount_3', 'senders_recount_3_started')`,
		`UPDATE backfill SET done = 1 WHERE name = 'senders'`,
	} {
		if _, err := c.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func recountMarker(t *testing.T, c *Cache, name string) int {
	t.Helper()
	var n int
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM backfill WHERE name = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRecountFixesStaleCountsAndKeepsOwnerAndLLM(t *testing.T) {
	c := recountFixture(t, t.TempDir())
	if err := c.initSenders(); err != nil {
		t.Fatal(err)
	}
	// Until the job has run: restarted, not marked, search tables untouched.
	if st, _ := c.SendersStatus(context.Background()); st.Complete {
		t.Error("recount not pending after the rules changed")
	}
	if recountMarker(t, c, sendersRecount) != 0 {
		t.Fatal("marker written before the recount")
	}
	if err := c.RunSenders(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	p := sndRow(t, c, "promotion@ebay.de")
	if p.Kind != KindList || p.NMsgs != 2*sendersBatch+30 {
		t.Errorf("promo sender: %+v", p)
	}
	var txn int
	if err := c.db.QueryRow(`SELECT n_txn_subj FROM senders WHERE addr = 'promotion@ebay.de'`).Scan(&txn); err != nil || txn != 0 {
		t.Errorf("n_txn_subj = %d (%v), want 0 with the new subject rule", txn, err)
	}
	if r := sndRow(t, c, "store-news@ikea.nl"); r.Kind != KindList {
		t.Errorf("store-news: %+v", r)
	}
	if r := sndRow(t, c, "owned@x.example"); r.Kind != KindList || r.KindSource != SourceOwner {
		t.Errorf("owner row lost: %+v", r)
	}
	if r := sndRow(t, c, "llm@x.example"); r.Kind != KindNotification || r.KindSource != SourceLLM {
		t.Errorf("llm row lost: %+v", r)
	}
	if recountMarker(t, c, sendersRecount) != 1 {
		t.Error("marker missing after success")
	}
	if func() bool {
		var n int
		_ = c.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'senders_prev_kind'`).Scan(&n)
		return n != 0
	}() {
		t.Error("snapshot table left behind")
	}
	// Idempotent: a later start does not recount again.
	if err := c.initSenders(); err != nil {
		t.Fatal(err)
	}
	if st, _ := c.SendersStatus(context.Background()); !st.Complete {
		t.Error("recounted again after the marker was written")
	}
}

func TestRecountResumesAfterRestartAndMarksOnlyAtTheEnd(t *testing.T) {
	dir := t.TempDir()
	c := recountFixture(t, dir)
	if err := c.initSenders(); err != nil {
		t.Fatal(err)
	}
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
	if err := c.RunSenders(ctx, nil); err == nil {
		t.Fatal("RunSenders was not interrupted")
	}
	c.now = real
	if recountMarker(t, c, sendersRecount) != 0 {
		t.Fatal("marker written by an interrupted recount")
	}
	var processed int
	if err := c.db.QueryRow(`SELECT processed FROM backfill WHERE name = 'senders'`).Scan(&processed); err != nil || processed == 0 {
		t.Fatalf("no progress to resume: %d %v", processed, err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c2.Close() })
	var after int
	if err := c2.db.QueryRow(`SELECT processed FROM backfill WHERE name = 'senders'`).Scan(&after); err != nil || after != processed {
		t.Fatalf("restart did not resume: processed %d -> %d (%v)", processed, after, err)
	}
	if err := c2.RunSenders(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if p := sndRow(t, c2, "promotion@ebay.de"); p.Kind != KindList || p.NMsgs != 2*sendersBatch+30 {
		t.Errorf("after resume: %+v", p)
	}
	if recountMarker(t, c2, sendersRecount) != 1 {
		t.Error("marker missing after the resumed recount")
	}
}
