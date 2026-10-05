package cache

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/excavador/mail-mcp/internal/accounts"
)

// examined returns the lines of the log that EXAMINE a folder.
func examined(l *cmdLog) []string {
	var out []string
	for _, ln := range l.lines() {
		f := strings.Fields(ln)
		if len(f) > 1 && strings.EqualFold(f[1], "EXAMINE") {
			out = append(out, ln)
		}
	}
	return out
}

func verbCount(l *cmdLog, verb string) int {
	n := 0
	for _, v := range l.verbs() {
		if v == verb {
			n++
		}
	}
	return n
}

// threeFolders is an env with INBOX, A and B, two messages in A and B each,
// refreshed once so every folder is complete.
func threeFolders(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, accounts.Gmail, "INBOX", "A", "B")
	e.appendMsg("INBOX", mkMsg("i1", "inbox one", "x"), t0)
	for i := 1; i <= 2; i++ {
		e.appendMsg("A", mkMsg(fmt.Sprintf("a%d", i), fmt.Sprintf("a %d", i), "x"), t0.Add(time.Duration(i)*time.Second))
		e.appendMsg("B", mkMsg(fmt.Sprintf("b%d", i), fmt.Sprintf("b %d", i), "x"), t0.Add(time.Duration(i)*time.Second))
	}
	first := e.refresh()
	if first.FoldersScanned != 3 || first.FoldersSkipped != 0 || first.FoldersTotal != 3 {
		t.Fatalf("first refresh: %+v", first)
	}
	return e
}

// onlyScanned asserts exactly the folder was opened and the others skipped.
func onlyScanned(t *testing.T, e *env, st Stats, folder string) {
	t.Helper()
	if st.FoldersTotal != 3 || st.FoldersScanned != 1 || st.FoldersSkipped != 2 || st.Folders != 3 {
		t.Errorf("stats = %+v, want 1 scanned, 2 skipped of 3", st)
	}
	ex := examined(e.log)
	if len(ex) != 1 || !strings.Contains(ex[0], folder) {
		t.Errorf("EXAMINE lines = %q, want exactly one, of %s", ex, folder)
	}
	if n := verbCount(e.log, "STATUS"); n != 3 {
		t.Errorf("STATUS count = %d, want 3", n)
	}
}

func TestChangedFolderIsScannedOthersSkipped(t *testing.T) {
	t.Run("new mail", func(t *testing.T) {
		e := threeFolders(t)
		e.appendMsg("A", mkMsg("a3", "a 3", "x"), t0.Add(time.Hour))
		e.log.reset()
		st := e.refresh()
		onlyScanned(t, e, st, "A")
		if st.NewUIDs != 1 || st.NewBodies != 1 || st.Removed != 0 {
			t.Errorf("stats = %+v", st)
		}
		if n := e.count(`SELECT COUNT(*) FROM membership WHERE folder = 'A'`); n != 3 {
			t.Errorf("A membership = %d, want 3", n)
		}
	})

	t.Run("removal", func(t *testing.T) {
		e := threeFolders(t)
		e.expungeUID("B", 1)
		e.log.reset()
		st := e.refresh()
		onlyScanned(t, e, st, "B")
		if st.Removed != 1 || st.NewUIDs != 0 {
			t.Errorf("stats = %+v", st)
		}
		if n := e.count(`SELECT COUNT(*) FROM membership WHERE folder = 'B'`); n != 1 {
			t.Errorf("B membership = %d, want 1", n)
		}
	})

	t.Run("uidvalidity change with equal counts", func(t *testing.T) {
		e := threeFolders(t)
		oldIDs := e.count(`SELECT COUNT(*) FROM membership WHERE folder = 'B'`)
		// Delete and re-create B with the same number of different messages:
		// MESSAGES and UIDNEXT come out equal to before, only UIDVALIDITY moves.
		e.deleteFolder("B")
		e.createFolder("B")
		e.appendMsg("B", mkMsg("nb1", "new b 1", "x"), t0.Add(10*time.Second))
		e.appendMsg("B", mkMsg("nb2", "new b 2", "x"), t0.Add(11*time.Second))
		e.log.reset()
		st := e.refresh()
		onlyScanned(t, e, st, "B")
		if st.Removed != oldIDs || st.Removed == 0 {
			t.Errorf("Removed = %d, want %d (old membership dropped)", st.Removed, oldIDs)
		}
		if st.NewUIDs != 2 {
			t.Errorf("NewUIDs = %d, want 2", st.NewUIDs)
		}
		if n := e.count(`SELECT COUNT(*) FROM membership WHERE folder = 'B'`); n != 2 {
			t.Errorf("B membership = %d, want 2", n)
		}
		// The members are the new messages, not the old ones under reused UIDs.
		if n := e.count(`SELECT COUNT(*) FROM membership m JOIN messages x ON x.account = m.account AND x.stable_id = m.stable_id WHERE m.folder = 'B' AND x.subject LIKE 'new b %'`); n != 2 {
			t.Errorf("B members that are the new messages = %d, want 2", n)
		}
	})

	t.Run("remove and add pair", func(t *testing.T) {
		e := threeFolders(t)
		// MESSAGES is 2 before and after; only UIDNEXT moves.
		e.expungeUID("B", 1)
		e.appendMsg("B", mkMsg("b3", "b 3", "x"), t0.Add(time.Hour))
		e.log.reset()
		st := e.refresh()
		onlyScanned(t, e, st, "B")
		if st.Removed != 1 || st.NewUIDs != 1 || st.NewBodies != 1 {
			t.Errorf("stats = %+v", st)
		}
		if n := e.count(`SELECT COUNT(*) FROM membership m JOIN messages x ON x.account = m.account AND x.stable_id = m.stable_id WHERE m.folder = 'B' AND x.subject IN ('b 2', 'b 3')`); n != 2 {
			t.Errorf("B members = %d, want b 2 and b 3", n)
		}
	})

	t.Run("a new folder is scanned", func(t *testing.T) {
		e := threeFolders(t)
		e.createFolder("C")
		e.appendMsg("C", mkMsg("c1", "c 1", "x"), t0)
		e.log.reset()
		st := e.refresh()
		if st.FoldersTotal != 4 || st.FoldersScanned != 1 || st.FoldersSkipped != 3 || st.NewUIDs != 1 {
			t.Errorf("stats = %+v", st)
		}
	})

	t.Run("flag-only change is skipped", func(t *testing.T) {
		// The cache stores no flags, so a flag change alone is not a change.
		e := threeFolders(t)
		e.admin(func(c *imapclient.Client) {
			if _, err := c.Select("A", nil).Wait(); err != nil {
				t.Fatal(err)
			}
			if err := c.Store(imap.UIDSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagSeen}}, nil).Close(); err != nil {
				t.Fatal(err)
			}
		})
		e.log.reset()
		st := e.refresh()
		if st.FoldersSkipped != 3 || st.FoldersScanned != 0 {
			t.Errorf("stats = %+v, want all skipped", st)
		}
	})
}

func folderState1(t *testing.T, e *env, folder string) (uidnext, messages int, scannedAt int64) {
	t.Helper()
	if err := e.cache.db.QueryRow(`SELECT uidnext, messages, scanned_at FROM folders WHERE account = 'acct' AND folder = ?`, folder).Scan(&uidnext, &messages, &scannedAt); err != nil {
		t.Fatal(err)
	}
	return
}

func TestFolderRecordsStatusAfterCompleteScan(t *testing.T) {
	e := threeFolders(t)
	un, msgs, at := folderState1(t, e, "A")
	if un != 3 || msgs != 2 || at == 0 {
		t.Errorf("A state = uidnext %d messages %d scanned_at %d, want 3, 2, nonzero", un, msgs, at)
	}
}

func TestInterruptedScanLeavesFolderIncompleteAndNextRefreshScansIt(t *testing.T) {
	e := threeFolders(t)
	// New mail in B, then kill the connection on the first body fetch.
	e.appendMsg("B", mkMsg("b3", "b 3", "x"), t0.Add(time.Hour))
	e.appendMsg("B", mkMsg("b4", "b 4", "x"), t0.Add(2*time.Hour))
	e.killAfterBodyFetches(1)
	if _, err := e.refreshErr(e.ctx()); err == nil {
		t.Fatal("interrupted refresh returned no error")
	}
	e.clearHook()
	if un, _, _ := folderState1(t, e, "B"); un != 0 {
		t.Errorf("B uidnext after interrupted scan = %d, want 0 (incomplete)", un)
	}
	if un, _, _ := folderState1(t, e, "A"); un == 0 {
		t.Error("A (skipped, untouched) lost its completion record")
	}

	e.log.reset()
	st := e.refresh()
	onlyScanned(t, e, st, "B")
	if un, msgs, _ := folderState1(t, e, "B"); un == 0 || msgs != 4 {
		t.Errorf("B after recovery = uidnext %d messages %d, want nonzero, 4", un, msgs)
	}
	if n := e.count(`SELECT COUNT(*) FROM membership WHERE folder = 'B'`); n != 4 {
		t.Errorf("B membership = %d, want 4", n)
	}
	// And now it is quiet again.
	e.log.reset()
	if st := e.refresh(); st.FoldersSkipped != 3 {
		t.Errorf("after recovery: %+v", st)
	}
}

func TestInterruptedFirstScanIsRescanned(t *testing.T) {
	// Never completed at all: the row exists (setFolder ran) with uidnext 0.
	e := newEnv(t, accounts.Gmail, "INBOX")
	fill(e, "INBOX", 3, "m")
	e.killAfterBodyFetches(1)
	if _, err := e.refreshErr(e.ctx()); err == nil {
		t.Fatal("no error")
	}
	e.clearHook()
	if un, _, _ := folderState1(t, e, "INBOX"); un != 0 {
		t.Errorf("uidnext = %d, want 0", un)
	}
	e.log.reset()
	st := e.refresh()
	if st.FoldersScanned != 1 || st.NewBodies == 0 && e.count(`SELECT COUNT(*) FROM messages`) != 3 {
		t.Errorf("stats = %+v", st)
	}
}

func TestFullScanSafetyNetAt24Hours(t *testing.T) {
	e := threeFolders(t)
	base := time.Now()
	clock := base
	e.cache.now = func() time.Time { return clock }

	clock = base // complete the folders under the fake clock
	if _, err := e.cache.db.Exec(`UPDATE folders SET uidnext = 0`); err != nil {
		t.Fatal(err)
	}
	if st := e.refresh(); st.FoldersScanned != 3 {
		t.Fatalf("seed: %+v", st)
	}

	clock = base.Add(23*time.Hour + 59*time.Minute)
	e.log.reset()
	st := e.refresh()
	if st.FoldersSkipped != 3 || st.FoldersScanned != 0 || len(examined(e.log)) != 0 {
		t.Errorf("+23h59m: %+v, EXAMINE %v; want all skipped", st, examined(e.log))
	}

	clock = base.Add(24 * time.Hour)
	e.log.reset()
	st = e.refresh()
	if st.FoldersScanned != 3 || st.FoldersSkipped != 0 || len(examined(e.log)) != 3 {
		t.Errorf("+24h: %+v, EXAMINE %v; want all scanned", st, examined(e.log))
	}
	if st.NewUIDs != 0 || st.NewBodies != 0 || st.Removed != 0 {
		t.Errorf("+24h full scan of unchanged mailbox changed the cache: %+v", st)
	}

	// The full scan restarted the interval.
	clock = base.Add(24*time.Hour + time.Minute)
	if st := e.refresh(); st.FoldersSkipped != 3 {
		t.Errorf("after the full scan: %+v, want all skipped", st)
	}

	// A clock that went backwards past the record is not trusted.
	clock = base
	if st := e.refresh(); st.FoldersScanned != 3 {
		t.Errorf("clock before scanned_at: %+v, want all scanned", st)
	}
}

func TestPipelinedStatusLatencyDoesNotGrowWithFolders(t *testing.T) {
	const delay = 200 * time.Millisecond
	timeUnchanged := func(folders int) time.Duration {
		names := make([]string, folders)
		for i := range names {
			names[i] = fmt.Sprintf("F%02d", i)
		}
		e := newEnv(t, accounts.Gmail, names...)
		e.appendMsg(names[0], mkMsg("x"+names[0], "s", "b"), t0)
		e.appendMsg(names[1], mkMsg("x"+names[1], "s", "b"), t0)
		e.refresh() // no latency: fill the cache
		e.setDelay(delay)
		e.log.reset()
		start := time.Now()
		st := e.refresh()
		took := time.Since(start)
		if st.FoldersSkipped != folders || st.FoldersScanned != 0 {
			t.Fatalf("%d folders: %+v, want all skipped", folders, st)
		}
		if n := verbCount(e.log, "STATUS"); n != folders {
			t.Fatalf("%d folders: %d STATUS", folders, n)
		}
		return took
	}
	small := timeUnchanged(3)
	large := timeUnchanged(20)
	t.Logf("3 folders %v, 20 folders %v (one-way delay %v)", small, large, delay)
	if small < 2*delay {
		t.Fatalf("3 folders took %v: the delay wrapper is not delaying (delay %v)", small, delay)
	}
	if large >= small+3*delay {
		t.Errorf("20 folders took %v, 3 took %v: not pipelined (limit 3 + 3x%v)", large, small, delay)
	}
}

func TestMigrationAddsFolderColumnsInPlace(t *testing.T) {
	e := threeFolders(t)
	c := e.cache
	db := c.db
	wantMembership := e.count(`SELECT COUNT(*) FROM membership`)
	wantMessages := e.count(`SELECT COUNT(*) FROM messages`)
	if wantMembership == 0 {
		t.Fatal("setup: empty cache")
	}
	// Reduce folders to its original shape, keeping the rows.
	for _, q := range []string{
		`CREATE TABLE folders_old AS SELECT account, folder, uidvalidity FROM folders`,
		`DROP TABLE folders`,
		`CREATE TABLE folders (account TEXT NOT NULL, folder TEXT NOT NULL, uidvalidity INTEGER NOT NULL, PRIMARY KEY (account, folder))`,
		`INSERT INTO folders SELECT account, folder, uidvalidity FROM folders_old`,
		`DROP TABLE folders_old`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var uv int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&uv); err != nil || uv != schemaVersion {
		t.Fatalf("user_version = %d, %v; want %d", uv, err, schemaVersion)
	}
	dir := c.dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	columns := func(c *Cache) string {
		rows, err := c.db.Query(`SELECT name FROM pragma_table_info('folders') ORDER BY cid`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var cols []string
		for rows.Next() {
			var n string
			_ = rows.Scan(&n)
			cols = append(cols, n)
		}
		return strings.Join(cols, ",")
	}
	const wantCols = "account,folder,uidvalidity,uidnext,messages,scanned_at,attrs"

	c2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := columns(c2); got != wantCols {
		t.Errorf("columns after Open = %s, want %s", got, wantCols)
	}
	check := func(c *Cache, label string) {
		t.Helper()
		var n, nz int
		if err := c.db.QueryRow(`SELECT COUNT(*), COUNT(CASE WHEN uidvalidity != 0 THEN 1 END) FROM folders`).Scan(&n, &nz); err != nil || n != 3 || nz != 3 {
			t.Errorf("%s: folders rows = %d (validity set %d), %v; want 3, 3", label, n, nz, err)
		}
		var nx int
		_ = c.db.QueryRow(`SELECT COUNT(*) FROM folders WHERE uidnext = 0 AND messages = 0 AND scanned_at = 0 AND attrs = ''`).Scan(&nx)
		if nx != 3 {
			t.Errorf("%s: migrated rows with default hints = %d, want 3", label, nx)
		}
		var m1, m2, v int
		_ = c.db.QueryRow(`SELECT COUNT(*) FROM membership`).Scan(&m1)
		_ = c.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&m2)
		_ = c.db.QueryRow(`PRAGMA user_version`).Scan(&v)
		if m1 != wantMembership || m2 != wantMessages || v != schemaVersion {
			t.Errorf("%s: membership %d messages %d version %d; want %d, %d, %d (index must survive)", label, m1, m2, v, wantMembership, wantMessages, schemaVersion)
		}
	}
	check(c2, "first Open")
	if err := c2.Close(); err != nil {
		t.Fatal(err)
	}

	// Second Open is a no-op.
	c3, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c3.Close() }()
	if got := columns(c3); got != wantCols {
		t.Errorf("columns after second Open = %s", got)
	}
	check(c3, "second Open")

	// The migrated cache refreshes without refetching anything: every folder
	// is scanned once (hints are 0), no body is fetched, and then it is quiet.
	e.cache = c3
	e.log.reset()
	st := e.refresh()
	if st.FoldersScanned != 3 || st.NewBodies != 0 || st.NewUIDs != 0 || st.Removed != 0 {
		t.Errorf("first refresh after migration: %+v", st)
	}
	e.log.reset()
	if st := e.refresh(); st.FoldersSkipped != 3 {
		t.Errorf("second refresh after migration: %+v", st)
	}
}

func TestAllMailFolderAfterRefresh(t *testing.T) {
	f := startGmailFake(t, gfConfig{Folders: gmailFolders(111, 111)})
	c, _ := gmailRefresh(t, f)
	if got := c.AllMailFolder(tctx(t), "g"); got != "[Gmail]/All Mail" {
		t.Errorf("AllMailFolder = %q, want [Gmail]/All Mail", got)
	}
	if got := c.AllMailFolder(tctx(t), "other"); got != "" {
		t.Errorf("AllMailFolder of an unknown account = %q, want empty", got)
	}
}

func TestAllMailFolderEmptyWhenNoneMarked(t *testing.T) {
	// imapmemserver marks no special-use folder.
	e := newEnv(t, accounts.Gmail, "INBOX", "Archive")
	if got := e.cache.AllMailFolder(e.ctx(), "acct"); got != "" {
		t.Errorf("before refresh: %q", got)
	}
	e.appendMsg("INBOX", mkMsg("a", "one", "x"), t0)
	e.refresh()
	if got := e.cache.AllMailFolder(e.ctx(), "acct"); got != "" {
		t.Errorf("after refresh without \\All: %q, want empty", got)
	}
}

func TestAllMailFolderIsNotConfusedByNames(t *testing.T) {
	// A folder merely named like the attribute is not \All.
	e := newEnv(t, accounts.Gmail, "INBOX", `All`, `\All`)
	e.appendMsg("INBOX", mkMsg("a", "one", "x"), t0)
	e.refresh()
	if got := e.cache.AllMailFolder(e.ctx(), "acct"); got != "" {
		t.Errorf("AllMailFolder = %q, want empty", got)
	}
}

func TestRefreshFoldersAlwaysScansAndLeavesFoldersIncomplete(t *testing.T) {
	e := threeFolders(t)
	c, err := imapxDial(e)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	// Nothing changed, yet RefreshFolders opens the folder.
	e.log.reset()
	st, err := e.cache.RefreshFolders(e.ctx(), c, e.acct, []string{"A"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Folders != 1 {
		t.Errorf("stats = %+v", st)
	}
	if ex := examined(e.log); len(ex) != 1 || !strings.Contains(ex[0], "A") {
		t.Errorf("EXAMINE = %q, want one, of A", ex)
	}
	if verbCount(e.log, "STATUS") != 0 {
		t.Error("RefreshFolders sent STATUS")
	}
	if un, _, _ := folderState1(t, e, "A"); un != 0 {
		t.Errorf("A uidnext = %d after RefreshFolders, want 0 (incomplete)", un)
	}
	if un, _, _ := folderState1(t, e, "B"); un == 0 {
		t.Error("B, not named, lost its record")
	}

	// The next background refresh rescans A and only A, and completes it.
	e.log.reset()
	bg := e.refresh()
	onlyScanned(t, e, bg, "A")
	if un, _, _ := folderState1(t, e, "A"); un == 0 {
		t.Error("A still incomplete after the background refresh")
	}
}
