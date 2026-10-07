package cache

import (
	"context"
	"testing"
	"time"
)

func TestOwnerMatcher(t *testing.T) {
	m := newOwnerMatcher([]string{"Me@Home.test", "@Alias.example", "tagged+x@home.test", "  "})
	for addr, want := range map[string]bool{
		"me@home.test":         true,  // exact
		"ME@HOME.TEST":         true,  // case-insensitive
		"me+news@home.test":    true,  // plus-tag matches the base address
		"me+a+b@home.test":     true,  // only the first + starts the tag
		"anyone@alias.example": true,  // domain pattern
		"x+y@alias.example":    true,  // domain pattern with a tag
		"other@home.test":      false, // same domain, no pattern
		"me@home.example":      false, // different domain
		"me@sub.alias.example": false, // pattern is not a suffix match
		"tagged+x@home.test":   true,  // tagged entry matches itself
		"tagged+y@home.test":   false, // a tagged entry is not its base
		"tagged@home.test":     false,
		"+x@home.test":         false,
		"":                     false,
		"me":                   false,
	} {
		if got := m.match(addr); got != want {
			t.Errorf("match(%q) = %v, want %v", addr, got, want)
		}
	}
	var nilm *ownerMatcher
	if nilm.match("me@home.test") {
		t.Error("nil matcher matched")
	}
}

func TestOwnersFingerprint(t *testing.T) {
	a := ownersFingerprint(map[string][]string{"x": {"a@b.c", "@d.e"}, "y": {"f@g.h"}})
	if b := ownersFingerprint(map[string][]string{"y": {"f@g.h"}, "x": {"@d.e", "a@b.c"}}); a != b {
		t.Error("fingerprint depends on order")
	}
	if b := ownersFingerprint(map[string][]string{"x": {"a@b.c"}, "y": {"f@g.h"}}); a == b {
		t.Error("fingerprint ignores an alias")
	}
	if b := ownersFingerprint(map[string][]string{"x": {"a@b.c", "@d.e", "f@g.h"}, "y": {}}); a == b {
		t.Error("fingerprint ignores which account holds an address")
	}
}

// ownerFixture holds one parent from alice and the owner's reply to her, sent
// as the alias me+list@alias.test from the Sent folder.
func ownerFixture(t *testing.T) *Cache {
	t.Helper()
	c := thrOpen(t)
	t0 := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	sndAdd(t, c, "p1", "p1@x", "Alice <alice@x.example>", "me@home.test", "hello", t0)
	sndAdd(t, c, "r1", "r1@x", "Me <me@alias.test>", "alice@x.example", "Re: hello", t0.Add(time.Hour), "In-Reply-To: <p1@x>")
	for _, q := range []string{
		`INSERT INTO folders (account, folder, uidvalidity, attrs) VALUES ('acc', 'Sent', 1, '')`,
		`INSERT INTO membership (account, stable_id, folder, uid, uidvalidity) VALUES ('acc','r1','Sent',1,1)`,
		`INSERT INTO message_thread (account, stable_id, tid, parent_stable_id, depth, outsider) VALUES ('acc','r1','t1','',0,1)`,
	} {
		if _, err := c.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func ownerRecount(t *testing.T, c *Cache, owners map[string][]string) {
	t.Helper()
	c.SetOwners(owners)
	if err := c.ReconcileOwners(nil); err != nil {
		t.Fatal(err)
	}
}

func recountPending(t *testing.T, c *Cache) bool {
	t.Helper()
	st, err := c.SendersStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return !st.Complete
}

func TestOwnerAliasChangeRecountsOnce(t *testing.T) {
	ctx := context.Background()
	c := ownerFixture(t)
	base := map[string][]string{"acc": {"me@home.test"}}
	withAlias := map[string][]string{"acc": {"me@home.test", "me@alias.test"}}

	// First start with this feature: no fingerprint on record, messages
	// exist, so the data is recounted once under the username alone.
	ownerRecount(t, c, base)
	if !recountPending(t, c) {
		t.Fatal("first start did not schedule the owner recount")
	}
	if err := c.RunSenders(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if n := sndRow(t, c, "alice@x.example").NRepliedByMe; n != 0 {
		t.Fatalf("reply from an unknown alias was credited: %d", n)
	}
	if recountMarker(t, c, ownersDonePrefix+ownersFingerprint(base)) != 1 {
		t.Fatal("done marker missing after the recount")
	}
	var outs int
	_ = c.db.QueryRow(`SELECT outsider FROM message_thread WHERE stable_id = 'r1'`).Scan(&outs)
	if outs != 1 {
		t.Fatal("outsider flag cleared for a non-owner address")
	}

	// Same set again: nothing to do.
	ownerRecount(t, c, base)
	if recountPending(t, c) {
		t.Fatal("unchanged owner set restarted the recount")
	}

	// An alias is added: recount once, and the reply is credited.
	ownerRecount(t, c, withAlias)
	if !recountPending(t, c) {
		t.Fatal("changed owner set did not restart the recount")
	}
	if recountMarker(t, c, ownersDonePrefix+ownersFingerprint(base)) != 0 {
		t.Fatal("stale done marker kept")
	}
	// A second start mid-recount resumes instead of restarting.
	if _, err := c.db.Exec(`UPDATE backfill SET processed = 1 WHERE name = 'senders'`); err != nil {
		t.Fatal(err)
	}
	ownerRecount(t, c, withAlias)
	var processed int
	_ = c.db.QueryRow(`SELECT processed FROM backfill WHERE name = 'senders'`).Scan(&processed)
	if processed != 1 {
		t.Fatalf("resume reset the job: processed = %d", processed)
	}
	if recountMarker(t, c, sendersRecount) != 0 {
		t.Fatal("recount marker present before the recount finished")
	}
	if err := c.RunSenders(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if n := sndRow(t, c, "alice@x.example").NRepliedByMe; n != 1 {
		t.Fatalf("n_replied_by_me = %d after adding the alias, want 1", n)
	}
	if recountMarker(t, c, ownersDonePrefix+ownersFingerprint(withAlias)) != 1 || recountMarker(t, c, sendersRecount) != 1 {
		t.Fatal("markers missing after the recount")
	}
	if recountMarker(t, c, ownersStartedPrefix+ownersFingerprint(withAlias)) != 0 {
		t.Fatal("started marker left behind")
	}
	// (The fixture's outsider flag was cleared when the recount restarted.)
	_ = c.db.QueryRow(`SELECT outsider FROM message_thread WHERE stable_id = 'r1'`).Scan(&outs)
	if outs != 0 {
		t.Error("owner's message still flagged outsider after the alias was added")
	}

	// And unchanged again after the recount: no restart.
	ownerRecount(t, c, withAlias)
	if recountPending(t, c) {
		t.Fatal("unchanged owner set restarted the recount after it finished")
	}

	// A domain pattern is a different set too.
	ownerRecount(t, c, map[string][]string{"acc": {"me@home.test", "@alias.test"}})
	if !recountPending(t, c) {
		t.Fatal("switching to a domain pattern did not restart the recount")
	}
}

func TestOwnerSetOnEmptyCacheJustRecords(t *testing.T) {
	c := thrOpen(t)
	set := map[string][]string{"acc": {"me@home.test"}}
	ownerRecount(t, c, set)
	if recountPending(t, c) {
		t.Fatal("empty cache scheduled a recount")
	}
	if recountMarker(t, c, ownersDonePrefix+ownersFingerprint(set)) != 1 {
		t.Fatal("owner set not recorded")
	}
}

func TestOwnerOutsiderClearResumesAfterInterruptedBatch(t *testing.T) {
	ctx := context.Background()
	c := ownerFixture(t)
	t0 := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	for _, id := range []string{"r2", "r3"} {
		sndAdd(t, c, id, id+"@x", "Me <me@alias.test>", "alice@x.example", "Re: hello", t0)
		if _, err := c.db.Exec(`INSERT INTO message_thread (account, stable_id, tid, parent_stable_id, depth, outsider) VALUES ('acc', ?, 't1', '', 0, 1)`, id); err != nil {
			t.Fatal(err)
		}
	}
	// A stranger's flag must survive.
	sndAdd(t, c, "s1", "s1@x", "Eve <eve@x.example>", "me@home.test", "hi", t0)
	if _, err := c.db.Exec(`INSERT INTO message_thread (account, stable_id, tid, parent_stable_id, depth, outsider) VALUES ('acc', 's1', 't2', '', 0, 1)`); err != nil {
		t.Fatal(err)
	}
	old := ownerOutsiderBatch
	ownerOutsiderBatch = 1
	t.Cleanup(func() { ownerOutsiderBatch = old })

	ownerRecount(t, c, map[string][]string{"acc": {"me@home.test"}})
	ownerRecount(t, c, map[string][]string{"acc": {"me@home.test", "me@alias.test"}})
	flagged := func() int {
		var n int
		_ = c.db.QueryRow(`SELECT COUNT(*) FROM message_thread WHERE outsider = 1`).Scan(&n)
		return n
	}
	if flagged() != 4 {
		t.Fatalf("startup cleared flags itself: %d flagged", flagged())
	}
	// One batch, then the process "dies".
	if more, err := c.ownerOutsiderStep(ctx); err != nil || !more {
		t.Fatalf("first step: more=%v err=%v", more, err)
	}
	if n := flagged(); n != 3 {
		t.Fatalf("after one batch %d flagged, want 3", n)
	}
	var cursor int64
	_ = c.db.QueryRow(`SELECT last_rowid FROM backfill WHERE name = ?`, ownersOutsidersCursor).Scan(&cursor)
	if cursor == 0 {
		t.Fatal("cursor not advanced")
	}
	// A restart with the same set resumes at the cursor.
	ownerRecount(t, c, map[string][]string{"acc": {"me@home.test", "me@alias.test"}})
	var after int64
	_ = c.db.QueryRow(`SELECT last_rowid FROM backfill WHERE name = ?`, ownersOutsidersCursor).Scan(&after)
	if after != cursor {
		t.Fatalf("restart moved the cursor %d -> %d", cursor, after)
	}
	if err := c.RunSenders(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if n := flagged(); n != 1 {
		t.Fatalf("%d flagged after the job, want only the stranger's", n)
	}
	var done int
	_ = c.db.QueryRow(`SELECT done FROM backfill WHERE name = ?`, ownersOutsidersCursor).Scan(&done)
	if done != 1 || recountMarker(t, c, ownersDonePrefix+ownersFingerprint(map[string][]string{"acc": {"me@home.test", "me@alias.test"}})) != 1 {
		t.Fatal("clear or done marker incomplete")
	}
}
