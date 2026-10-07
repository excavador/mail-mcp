package cache

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/excavador/mail-mcp/internal/accounts"
	"github.com/excavador/mail-mcp/internal/imapx"
)

func TestStableIDFor(t *testing.T) {
	hdrA := []byte("Message-Id: <a@x>\r\nSubject: one\r\n\r\n")
	hdrB := []byte("Message-Id: <b@x>\r\nSubject: two\r\n\r\n")
	pm := []byte("X-Pm-Internal-Id: abcDEF123_-==\r\nMessage-Id: <a@x>\r\n\r\n")
	d1, d2 := t0, t0.Add(72*time.Hour)

	for _, tc := range []struct {
		name string
		hdr  []byte
		size int64
		date time.Time
		n    uint64
	}{
		{"same header", hdrA, 10, d1, 18446744073709551615},
		{"other header", hdrB, 10, d1, 18446744073709551615},
		{"other size", hdrA, 9999, d1, 18446744073709551615},
		{"other date", hdrA, 10, d2, 18446744073709551615},
		{"empty header", nil, 0, time.Time{}, 18446744073709551615},
	} {
		got, err := stableIDFor(accounts.Gmail, tc.hdr, tc.size, tc.date, tc.n)
		if err != nil || got != "gm:18446744073709551615" {
			t.Errorf("%s: got %q, %v; want gm:<n> regardless of header, size, date", tc.name, got, err)
		}
	}
	got, _ := stableIDFor(accounts.Gmail, hdrA, 10, d1, 42)
	if got != "gm:42" {
		t.Errorf("gm id = %q", got)
	}

	// Gmail with 0: exactly stableID, which is the mid: scheme.
	want, err := stableID(accounts.Gmail, hdrA, 10, d1)
	if err != nil || !strings.HasPrefix(want, "mid:") {
		t.Fatalf("stableID = %q, %v", want, err)
	}
	if got, err := stableIDFor(accounts.Gmail, hdrA, 10, d1, 0); err != nil || got != want {
		t.Errorf("Gmail 0 = %q, %v; want %q", got, err, want)
	}

	// Proton ignores n.
	wantPm, err := stableID(accounts.Proton, pm, 10, d1)
	if err != nil || !strings.HasPrefix(wantPm, "pm:") {
		t.Fatalf("proton stableID = %q, %v", wantPm, err)
	}
	for _, n := range []uint64{0, 42} {
		if got, err := stableIDFor(accounts.Proton, pm, 10, d1, n); err != nil || got != wantPm {
			t.Errorf("Proton n=%d = %q, %v; want %q", n, got, err, wantPm)
		}
	}
	wantPmMid, _ := stableID(accounts.Proton, hdrA, 10, d1)
	if got, _ := stableIDFor(accounts.Proton, hdrA, 10, d1, 42); got != wantPmMid || strings.HasPrefix(got, "gm:") {
		t.Errorf("Proton without pm id, n=42 = %q; want %q", got, wantPmMid)
	}
}

// gmailRefresh opens a fresh cache, refreshes the fake once and returns both.
func gmailRefresh(t *testing.T, f *gmailFake) (*Cache, Stats) {
	t.Helper()
	dir := t.TempDir()
	a := f.account(t, dir, "g", accounts.Gmail)
	c, err := Open(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := ctxT(t)
	defer cancel()
	cl, err := imapx.Dial(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cl.Close() }()
	st, err := c.Refresh(ctx, a, cl)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	return c, st
}

func ctxT(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 20*time.Second)
}

func count(t *testing.T, c *Cache, q string, args ...any) int {
	t.Helper()
	var n int
	if err := c.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func gmailFolders(inboxMsgID, allMsgID uint64) []gfFolder {
	raw := gfMail("one", "shared message")
	other := gfMail("two", "only in all mail")
	otherID := uint64(222)
	if allMsgID == 0 {
		otherID = 0
	}
	return []gfFolder{
		{Name: "INBOX", Validity: 11, Msgs: []gfMsg{{UID: 3, MsgID: inboxMsgID, ThrID: 900, Raw: raw, Date: t0}}},
		{Name: "[Gmail]/All Mail", Validity: 12, All: true, Msgs: []gfMsg{
			{UID: 77, MsgID: allMsgID, ThrID: 900, Raw: raw, Date: t0},
			{UID: 78, MsgID: otherID, ThrID: 901, Raw: other, Date: t0.Add(time.Hour)},
		}},
	}
}

func TestRefreshGmailWithCapUsesGmMsgID(t *testing.T) {
	f := startGmailFake(t, gfConfig{Folders: gmailFolders(111, 111)})
	c, st := gmailRefresh(t, f)

	if got := count(t, c, `SELECT COUNT(*) FROM messages`); got != 2 {
		t.Errorf("messages = %d, want 2 (same X-GM-MSGID in two folders is one row)", got)
	}
	if got := count(t, c, `SELECT COUNT(*) FROM membership`); got != 3 {
		t.Errorf("membership = %d, want 3", got)
	}
	if got := count(t, c, `SELECT COUNT(*) FROM membership WHERE stable_id = 'gm:111'`); got != 2 {
		t.Errorf("gm:111 membership = %d, want 2 (INBOX and All Mail)", got)
	}
	var thr string
	if err := c.db.QueryRow(`SELECT gm_thread_id FROM messages WHERE stable_id = 'gm:111'`).Scan(&thr); err != nil || thr != "900" {
		t.Errorf("gm_thread_id = %q, %v; want 900", thr, err)
	}
	if err := c.db.QueryRow(`SELECT gm_thread_id FROM messages WHERE stable_id = 'gm:222'`).Scan(&thr); err != nil || thr != "901" {
		t.Errorf("gm:222 gm_thread_id = %q, %v; want 901", thr, err)
	}
	if st.NewIDs != 2 || st.NewBodies != 2 {
		t.Errorf("stats = %+v, want NewIDs 2 NewBodies 2 (one body per gm id)", st)
	}
	raw := f.log.raw()
	if !strings.Contains(raw, "X-GM-MSGID") || !strings.Contains(raw, "X-GM-THRID") {
		t.Errorf("header fetch did not ask for X-GM-MSGID/X-GM-THRID:\n%s", raw)
	}
	for _, v := range f.log.verbs() {
		if v == "STORE" || v == "SELECT" || v == "COPY" || v == "MOVE" || v == "EXPUNGE" {
			t.Errorf("refresh sent %s", v)
		}
	}
}

func TestRefreshGmailWithoutCapUsesMidAndNeverAsksForGm(t *testing.T) {
	f := startGmailFake(t, gfConfig{Caps: gfCapsPlain, Folders: gmailFolders(111, 111)})
	c, _ := gmailRefresh(t, f)

	if got := count(t, c, `SELECT COUNT(*) FROM messages WHERE stable_id LIKE 'mid:%'`); got != 2 {
		t.Errorf("mid: messages = %d, want 2", got)
	}
	if got := count(t, c, `SELECT COUNT(*) FROM messages WHERE stable_id LIKE 'gm:%'`); got != 0 {
		t.Errorf("gm: messages = %d, want 0 without the capability", got)
	}
	if got := count(t, c, `SELECT COUNT(*) FROM messages WHERE gm_thread_id <> ''`); got != 0 {
		t.Errorf("gm_thread_id set without the capability on %d rows", got)
	}
	if raw := f.log.raw(); strings.Contains(raw, "X-GM-MSGID") || strings.Contains(raw, "X-GM-THRID") || strings.Contains(raw, "X-GM-LABELS") {
		t.Errorf("client sent a Gmail attribute to a server without X-GM-EXT-1:\n%s", raw)
	}
	if !strings.Contains(strings.Join(f.log.verbs(), " "), "FETCH") {
		t.Error("no FETCH recorded; the assertion above proved nothing")
	}
}

func TestRefreshGmailZeroMsgIDFallsBackToMid(t *testing.T) {
	f := startGmailFake(t, gfConfig{Folders: gmailFolders(0, 0)})
	c, _ := gmailRefresh(t, f)

	if got := count(t, c, `SELECT COUNT(*) FROM messages WHERE stable_id LIKE 'mid:%'`); got != 2 {
		t.Errorf("mid: messages = %d, want 2 (X-GM-MSGID 0 falls back)", got)
	}
	if got := count(t, c, `SELECT COUNT(*) FROM messages WHERE stable_id LIKE 'gm:%'`); got != 0 {
		t.Errorf("gm:0 ids stored: %d", got)
	}
	if got := count(t, c, `SELECT COUNT(*) FROM membership`); got != 3 {
		t.Errorf("membership = %d, want 3", got)
	}
}

func TestEnsureSchemaVersion(t *testing.T) {
	userVersion := func(c *Cache) int {
		var v int
		if err := c.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	seed := func(c *Cache) {
		t.Helper()
		for _, q := range []string{
			`INSERT INTO messages (account, stable_id, blob_sha256) VALUES ('a', 'mid:x', 'b')`,
			`INSERT INTO membership (account, stable_id, folder, uid, uidvalidity) VALUES ('a', 'mid:x', 'INBOX', 1, 1)`,
			`INSERT INTO folders (account, folder, uidvalidity) VALUES ('a', 'INBOX', 1)`,
			`INSERT INTO refreshes (account, at, ok) VALUES ('a', 1, 1)`,
			`INSERT INTO message_fts2 (rowid, subject, from_addr, to_addr, cc_addr, body_new, body_full, account, stable_id) VALUES (1,'s','f','t','c','b','','a','mid:x')`,
		} {
			if _, err := c.db.Exec(q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
	}
	rows := func(c *Cache) int {
		n := 0
		for _, tb := range []string{"messages", "membership", "folders", "refreshes", "message_fts2"} {
			n += count(t, c, `SELECT COUNT(*) FROM `+tb)
		}
		return n
	}

	dir := filepath.Join(t.TempDir(), "cache")
	c, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if v := userVersion(c); v != 2 {
		t.Fatalf("fresh user_version = %d, want 2", v)
	}
	blob := filepath.Join(dir, "blobs", strings.Repeat("ab", 32))
	if err := os.WriteFile(blob, []byte("mail"), 0o600); err != nil {
		t.Fatal(err)
	}
	seed(c)
	if rows(c) != 5 {
		t.Fatalf("seed rows = %d", rows(c))
	}
	// Force the previous version and reopen: the index is rebuilt empty.
	if _, err := c.db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if v := userVersion(c); v != 2 {
		t.Errorf("user_version after upgrade = %d, want 2", v)
	}
	if n := rows(c); n != 0 {
		t.Errorf("rows after version mismatch = %d, want 0", n)
	}
	if b, err := os.ReadFile(blob); err != nil || string(b) != "mail" {
		t.Errorf("blob lost on upgrade: %v", err)
	}
	// The recreated schema is usable and has the new column.
	seed(c)
	if _, err := c.db.Exec(`UPDATE messages SET gm_thread_id = '1'`); err != nil {
		t.Errorf("gm_thread_id column missing: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopen at the current version keeps the rows.
	c, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if n := rows(c); n != 5 {
		t.Errorf("rows after same-version reopen = %d, want 5", n)
	}
	if v := userVersion(c); v != 2 {
		t.Errorf("user_version = %d", v)
	}
}

func TestEnsureSchemaPreVersionedDatabaseIsRebuilt(t *testing.T) {
	// user_version 0 with existing tables is a pre-versioning file.
	dir := filepath.Join(t.TempDir(), "cache")
	c, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`INSERT INTO messages (account, stable_id, blob_sha256) VALUES ('a', 'mid:x', 'b')`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`PRAGMA user_version = 0`); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	c, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if n := count(t, c, `SELECT COUNT(*) FROM messages`); n != 0 {
		t.Errorf("messages = %d, want 0", n)
	}
}

func TestHitsByUID(t *testing.T) {
	c, err := Open(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx, cancel := ctxT(t)
	defer cancel()

	type row struct {
		id     string
		date   int64
		folder string
		uid    uint32
	}
	rowsIn := []row{
		{"gm:1", 100, "All", 1},
		{"gm:2", 300, "All", 2},
		{"gm:3", 200, "All", 3},
		{"gm:4", 400, "Other", 4}, // same UID space number, different folder
		{"gm:5", 200, "All", 5},   // same date as gm:3: ties by stable_id
	}
	for _, r := range rowsIn {
		if _, err := c.db.Exec(`INSERT INTO messages (account, stable_id, blob_sha256, from_addr, subject, date_unix) VALUES ('a', ?, 'b', 'f', 's-'||?, ?)`, r.id, r.id, r.date); err != nil {
			t.Fatal(err)
		}
		if _, err := c.db.Exec(`INSERT INTO membership (account, stable_id, folder, uid, uidvalidity) VALUES ('a', ?, ?, ?, 1)`, r.id, r.folder, r.uid); err != nil {
			t.Fatal(err)
		}
	}
	// gm:3 is also a member of another folder: Folders lists both.
	if _, err := c.db.Exec(`INSERT INTO membership (account, stable_id, folder, uid, uidvalidity) VALUES ('a', 'gm:3', 'INBOX', 9, 1)`); err != nil {
		t.Fatal(err)
	}
	ids := func(h []SearchHit) string {
		var s []string
		for _, x := range h {
			s = append(s, x.StableID)
		}
		return strings.Join(s, ",")
	}
	u := func(n ...uint32) []imap.UID {
		var out []imap.UID
		for _, x := range n {
			out = append(out, imap.UID(x))
		}
		return out
	}

	// Mixed cached and uncached, newest first, ties broken by stable id.
	hits, trunc, unc, err := c.HitsByUID(ctx, "a", "All", u(1, 2, 3, 5, 100, 101), 0)
	if err != nil || trunc || unc != 2 || ids(hits) != "gm:2,gm:3,gm:5,gm:1" {
		t.Errorf("mixed: ids=%s trunc=%v uncached=%d err=%v", ids(hits), trunc, unc, err)
	}
	// Folders of the message are all its folders, not just the searched one.
	for _, h := range hits {
		if h.StableID == "gm:3" && strings.Join(h.Folders, ",") != "All,INBOX" && strings.Join(h.Folders, ",") != "INBOX,All" {
			t.Errorf("gm:3 folders = %v", h.Folders)
		}
		if h.Snippet != "" || h.Subject != "s-"+h.StableID || h.Date.IsZero() {
			t.Errorf("hit = %+v", h)
		}
	}
	// Limit and truncated.
	hits, trunc, unc, err = c.HitsByUID(ctx, "a", "All", u(1, 2, 3, 5), 2)
	if err != nil || !trunc || unc != 0 || ids(hits) != "gm:2,gm:3" {
		t.Errorf("limit: ids=%s trunc=%v uncached=%d err=%v", ids(hits), trunc, unc, err)
	}
	hits, trunc, _, err = c.HitsByUID(ctx, "a", "All", u(1, 2), 2)
	if err != nil || trunc || len(hits) != 2 {
		t.Errorf("exact limit: %d trunc=%v err=%v", len(hits), trunc, err)
	}
	// A UID that belongs to another folder does not match here.
	hits, _, unc, err = c.HitsByUID(ctx, "a", "All", u(4), 0)
	if err != nil || len(hits) != 0 || unc != 1 {
		t.Errorf("other folder uid: hits=%d uncached=%d err=%v", len(hits), unc, err)
	}
	// Another account sees nothing.
	hits, _, unc, _ = c.HitsByUID(ctx, "b", "All", u(1, 2), 0)
	if len(hits) != 0 || unc != 2 {
		t.Errorf("other account: hits=%d uncached=%d", len(hits), unc)
	}
	// Empty input.
	hits, trunc, unc, err = c.HitsByUID(ctx, "a", "All", nil, 0)
	if err != nil || trunc || unc != 0 || len(hits) != 0 || hits == nil {
		t.Errorf("empty: hits=%v trunc=%v uncached=%d err=%v", hits, trunc, unc, err)
	}
	// More UIDs than one query chunk (500): all resolved, counts right.
	var many []imap.UID
	for i := uint32(1000); i < 1000+1203; i++ {
		many = append(many, imap.UID(i))
	}
	many = append(many, 1, 2)
	hits, trunc, unc, err = c.HitsByUID(ctx, "a", "All", many, 500)
	if err != nil || len(hits) != 2 || trunc || unc != 1203 {
		t.Errorf("chunked: hits=%d trunc=%v uncached=%d err=%v", len(hits), trunc, unc, err)
	}
}
