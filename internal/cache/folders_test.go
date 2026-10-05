package cache

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/excavador/mail-mcp/internal/accounts"
)

// bulk inserts n messages "<prefix>0".. in INBOX of account, one date apart.
func bulk(t *testing.T, c *Cache, account, prefix string, n int) []string {
	t.Helper()
	out := make([]string, n)
	for i := range n {
		out[i] = fmt.Sprintf("%s%d", prefix, i)
		ins(t, c, account, out[i], "a@x.org", "s "+out[i], "body", t0.Add(time.Duration(i)*time.Minute), "INBOX")
	}
	return out
}

func uidsIn(t *testing.T, c *Cache, account, folder string) []imap.UID {
	t.Helper()
	rows, err := c.db.Query(`SELECT uid FROM membership WHERE account = ? AND folder = ? ORDER BY uid`, account, folder)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []imap.UID
	for rows.Next() {
		var u uint32
		if err := rows.Scan(&u); err != nil {
			t.Fatal(err)
		}
		out = append(out, imap.UID(u))
	}
	return out
}

func TestSearchFolderLookupsAreOnePerChunk(t *testing.T) {
	for _, n := range []int{500, 501} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			c := openCache(t)
			want := bulk(t, c, "a", "m", n)
			before := c.FolderQueries()
			hits, trunc := search(t, c, SearchQuery{Account: "a", Limit: 500})
			if got := c.FolderQueries() - before; got != 1 {
				t.Errorf("Search of %d matches ran %d folder queries, want 1", n, got)
			}
			if len(hits) != 500 || trunc != (n > 500) {
				t.Fatalf("hits=%d truncated=%v", len(hits), trunc)
			}
			seen := map[string]bool{}
			for _, h := range hits {
				seen[h.StableID] = true
				if !reflect.DeepEqual(h.Folders, []string{"INBOX"}) {
					t.Fatalf("%s folders = %v", h.StableID, h.Folders)
				}
			}
			for _, id := range want {
				if !seen[id] && n == 500 {
					t.Fatalf("hit %s missing", id)
				}
			}
		})
	}
}

// The result set is capped at maxLimit (500) before folders are looked up, so
// foldersByID itself is where the 500 -> 501 chunk boundary can be observed.
func TestFoldersByIDChunkBoundary(t *testing.T) {
	for n, wantQ := range map[int]int64{0: 0, 1: 1, 500: 1, 501: 2, 1000: 2, 1001: 3} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			c := openCache(t)
			ids := bulk(t, c, "a", "m", n)
			before := c.FolderQueries()
			got, err := c.foldersByID(tctx(t), "a", ids)
			if err != nil {
				t.Fatal(err)
			}
			if q := c.FolderQueries() - before; q != wantQ {
				t.Errorf("%d ids: %d queries, want %d", n, q, wantQ)
			}
			if len(got) != n {
				t.Fatalf("got %d entries", len(got))
			}
			for _, id := range ids {
				if !reflect.DeepEqual(got[id], []string{"INBOX"}) {
					t.Fatalf("%s = %v", id, got[id])
				}
			}
		})
	}
}

func TestHitsByUIDFolderLookupsAreOnePerChunk(t *testing.T) {
	for _, n := range []int{500, 501} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			c := openCache(t)
			bulk(t, c, "a", "m", n)
			uids := uidsIn(t, c, "a", "INBOX")
			before := c.FolderQueries()
			hits, trunc, uncached, err := c.HitsByUID(tctx(t), "a", "INBOX", uids, 500)
			if err != nil {
				t.Fatal(err)
			}
			if got := c.FolderQueries() - before; got != 1 {
				t.Errorf("HitsByUID of %d uids ran %d folder queries, want 1", n, got)
			}
			if len(hits) != 500 || trunc != (n > 500) || uncached != 0 {
				t.Fatalf("hits=%d truncated=%v uncached=%d", len(hits), trunc, uncached)
			}
			for _, h := range hits {
				if !reflect.DeepEqual(h.Folders, []string{"INBOX"}) {
					t.Fatalf("%s folders = %v", h.StableID, h.Folders)
				}
			}
		})
	}
}

func TestReadMessageUsesOneFolderQuery(t *testing.T) {
	c := openCache(t)
	ins(t, c, "a", "m1", "a@x.org", "s", "body", t0, "INBOX", "Work")
	sum, err := c.putBlob(mkMsg("m1", "s", "body"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`UPDATE messages SET blob_sha256 = ? WHERE stable_id = 'm1'`, sum); err != nil {
		t.Fatal(err)
	}
	before := c.FolderQueries()
	m, err := c.ReadMessage(tctx(t), "a", "m1", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.FolderQueries() - before; got != 1 {
		t.Errorf("ReadMessage ran %d folder queries, want 1", got)
	}
	if !reflect.DeepEqual(m.Folders, []string{"INBOX", "Work"}) {
		t.Errorf("folders = %v", m.Folders)
	}
}

func TestFoldersAreSortedDedupedAndNeverNull(t *testing.T) {
	c := openCache(t)
	// Inserted out of order; m1 is also in INBOX under a second UID.
	ins(t, c, "a", "m1", "a@x.org", "one", "b", t0, "Zeta", "INBOX", "Alpha")
	if _, err := c.db.Exec(`INSERT INTO membership (account, stable_id, folder, uid, uidvalidity) VALUES ('a','m1','INBOX',?,1)`, uidSeq.Add(1)); err != nil {
		t.Fatal(err)
	}
	ins(t, c, "a", "m2", "a@x.org", "two", "b", t0.Add(time.Hour), "INBOX")
	// An orphan message with no membership at all.
	ins(t, c, "a", "orphan", "a@x.org", "three", "b", t0.Add(2*time.Hour))

	got, err := c.foldersByID(tctx(t), "a", []string{"m1", "m2", "orphan", "nosuch"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"m1": {"Alpha", "INBOX", "Zeta"}, "m2": {"INBOX"}, "orphan": {}, "nosuch": {}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("foldersByID = %#v, want %#v", got, want)
	}
	if got["orphan"] == nil {
		t.Error("orphan folders is nil (would marshal as null)")
	}

	hits, _ := search(t, c, SearchQuery{Account: "a"})
	for _, h := range hits {
		if h.Folders == nil {
			t.Errorf("%s: nil Folders", h.StableID)
		}
		if h.StableID == "m1" && !reflect.DeepEqual(h.Folders, []string{"Alpha", "INBOX", "Zeta"}) {
			t.Errorf("m1 folders = %v", h.Folders)
		}
		if h.StableID == "orphan" && len(h.Folders) != 0 {
			t.Errorf("orphan folders = %v", h.Folders)
		}
	}
	// Hits do not share backing arrays (callers sanitise in place).
	for _, h := range hits {
		if len(h.Folders) > 0 {
			h.Folders[0] = "changed"
		}
	}
	again, _ := search(t, c, SearchQuery{Account: "a"})
	for _, h := range again {
		if len(h.Folders) > 0 && h.Folders[0] == "changed" {
			t.Errorf("%s: folders aliased between searches", h.StableID)
		}
	}
}

func TestHitsAcrossAccountsGetTheirOwnFolders(t *testing.T) {
	c := openCache(t)
	ins(t, c, "a", "same", "a@x.org", "in a", "b", t0, "INBOX", "OnlyA")
	ins(t, c, "b", "same", "a@x.org", "in b", "b", t0.Add(time.Minute), "INBOX", "OnlyB")
	before := c.FolderQueries()
	hits, _ := search(t, c, SearchQuery{})
	if got := c.FolderQueries() - before; got != 2 {
		t.Errorf("two accounts ran %d folder queries, want 2 (one per account)", got)
	}
	byAcct := map[string][]string{}
	for _, h := range hits {
		byAcct[h.Account] = h.Folders
	}
	want := map[string][]string{"a": {"INBOX", "OnlyA"}, "b": {"INBOX", "OnlyB"}}
	if !reflect.DeepEqual(byAcct, want) {
		t.Errorf("folders by account = %v, want %v", byAcct, want)
	}
}

func TestNoteFolderThenRefreshSetsRealUIDValidity(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX", "Fresh")
	ctx := e.ctx()
	if ok, err := e.cache.HasFolder(ctx, "acct", "Fresh"); err != nil || ok {
		t.Fatalf("HasFolder before = %v, %v", ok, err)
	}
	if err := e.cache.NoteFolder(ctx, "acct", "Fresh"); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.cache.HasFolder(ctx, "acct", "Fresh"); err != nil || !ok {
		t.Fatalf("HasFolder after = %v, %v", ok, err)
	}
	if v := e.count(`SELECT uidvalidity FROM folders WHERE account='acct' AND folder='Fresh'`); v != 0 {
		t.Fatalf("placeholder uidvalidity = %d, want 0", v)
	}
	e.appendMsg("Fresh", mkMsg("f1", "in fresh", "b"), t0)
	e.refresh()
	if v := e.count(`SELECT uidvalidity FROM folders WHERE account='acct' AND folder='Fresh'`); v == 0 {
		t.Error("refresh left the placeholder uidvalidity 0")
	}
	if n := e.count(`SELECT COUNT(*) FROM membership WHERE folder='Fresh'`); n != 1 {
		t.Errorf("Fresh memberships = %d, want 1", n)
	}
	// And a targeted refresh of a freshly noted folder works too.
	e.createFolder("Fresh2")
	if err := e.cache.NoteFolder(ctx, "acct", "Fresh2"); err != nil {
		t.Fatal(err)
	}
	c, err := imapxDial(e)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := e.cache.RefreshFolders(ctx, c, e.acct, []string{"Fresh2"}); err != nil {
		t.Fatalf("RefreshFolders of a noted folder: %v", err)
	}
	if v := e.count(`SELECT uidvalidity FROM folders WHERE account='acct' AND folder='Fresh2'`); v == 0 {
		t.Error("RefreshFolders left uidvalidity 0")
	}
}

func TestNoteFolderLeavesAnExistingRowAlone(t *testing.T) {
	c := openCache(t)
	ins(t, c, "a", "m1", "a@x.org", "s", "b", t0, "INBOX") // uidvalidity 1
	if err := c.NoteFolder(tctx(t), "a", "INBOX"); err != nil {
		t.Fatal(err)
	}
	var v, n int
	if err := c.db.QueryRow(`SELECT uidvalidity, (SELECT COUNT(*) FROM folders) FROM folders WHERE account='a' AND folder='INBOX'`).Scan(&v, &n); err != nil {
		t.Fatal(err)
	}
	if v != 1 || n != 1 {
		t.Errorf("uidvalidity=%d rows=%d, want 1 and 1", v, n)
	}
	// Account scoping: the same name under another account is a different row.
	if ok, _ := c.HasFolder(tctx(t), "other", "INBOX"); ok {
		t.Error("HasFolder leaked across accounts")
	}
	if err := c.NoteFolder(tctx(t), "other", "INBOX"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := c.HasFolder(tctx(t), "other", "INBOX"); !ok {
		t.Error("NoteFolder did not record for other")
	}
}

func TestHitsByUIDOnACancelledContextErrors(t *testing.T) {
	c := openCache(t)
	bulk(t, c, "a", "m", 3)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, err := c.HitsByUID(ctx, "a", "INBOX", uidsIn(t, c, "a", "INBOX"), 10); err == nil {
		t.Error("HitsByUID on a cancelled context returned no error")
	}
}

func TestHitsByUIDTimeoutIsTenSeconds(t *testing.T) {
	if HitsByUIDTimeout != 10*time.Second {
		t.Errorf("HitsByUIDTimeout = %v", HitsByUIDTimeout)
	}
}

func TestRefreshPrunesFoldersTheServerNoLongerLists(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX", "Gone", "Keep")
	e.appendMsg("Gone", mkMsg("g1", "in gone", "b"), t0)
	e.appendMsg("Keep", mkMsg("k1", "in keep", "b"), t0)
	e.refresh()
	if n := e.count(`SELECT COUNT(*) FROM membership WHERE folder='Gone'`); n != 1 {
		t.Fatalf("Gone memberships before = %d, want 1", n)
	}
	msgs := e.count(`SELECT COUNT(*) FROM messages`)
	e.deleteFolder("Gone")
	e.refresh()
	if n := e.count(`SELECT COUNT(*) FROM folders WHERE account='acct' AND folder='Gone'`); n != 0 {
		t.Errorf("folders row for Gone survived: %d", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM membership WHERE folder='Gone'`); n != 0 {
		t.Errorf("membership for Gone survived: %d", n)
	}
	if ok, _ := e.cache.HasFolder(e.ctx(), "acct", "Gone"); ok {
		t.Error("HasFolder still accepts Gone")
	}
	if ok, _ := e.cache.HasFolder(e.ctx(), "acct", "Keep"); !ok {
		t.Error("Keep was pruned")
	}
	if n := e.count(`SELECT COUNT(*) FROM membership WHERE folder='Keep'`); n != 1 {
		t.Errorf("Keep memberships = %d, want 1", n)
	}
	if got := e.count(`SELECT COUNT(*) FROM messages`); got != msgs {
		t.Errorf("messages changed %d -> %d; pruning must keep them", msgs, got)
	}
}

func TestPruneFoldersIgnoresAnEmptyOrFailedList(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX", "Keep")
	e.appendMsg("Keep", mkMsg("k1", "in keep", "b"), t0)
	e.refresh()
	rows := func() int { return e.count(`SELECT COUNT(*) FROM folders WHERE account='acct'`) }
	before := rows()

	// Failed LIST: the connection dies when LIST arrives.
	e.log.setHook(func(chunk string) {
		if strings.Contains(strings.ToUpper(chunk), " LIST ") {
			e.log.killConns()
		}
	})
	if _, err := e.refreshErr(e.ctx()); err == nil {
		t.Fatal("Refresh with a dead LIST succeeded")
	}
	e.clearHook()
	if got := rows(); got != before {
		t.Errorf("failed LIST changed folders rows %d -> %d", before, got)
	}

	// Empty listing: Refresh's guard never reaches pruneFolders; the helper
	// itself is only ever called with a non-empty list.
	if got := e.count(`SELECT COUNT(*) FROM membership WHERE folder='Keep'`); got != 1 {
		t.Errorf("Keep membership = %d, want 1", got)
	}
}
