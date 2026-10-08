package cache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/excavador/mail-mcp/internal/accounts"
)

// Two pods of a rolling update share the cache directory. Their writes must
// queue on SQLite's write lock (WAL + busy_timeout), not fail with SQLITE_BUSY.
func TestTwoCachesOnOneDirWriteConcurrentlyWithoutBusy(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX")
	for i := range 20 {
		e.appendMsg("INBOX", mkMsg(fmt.Sprintf("m%d", i), fmt.Sprintf("subject %d", i), "body"), t0.Add(time.Duration(i)*time.Hour))
	}
	e.refresh()

	other, err := Open(filepath.Join(e.dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })

	rows, err := e.cache.db.Query(`SELECT stable_id FROM messages WHERE account = ?`, e.acct.Name)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if len(ids) != 20 {
		t.Fatalf("%d messages cached, want 20", len(ids))
	}

	// More mail for the refresher to ingest while the other writes tags.
	for i := 20; i < 40; i++ {
		e.appendMsg("INBOX", mkMsg(fmt.Sprintf("m%d", i), fmt.Sprintf("subject %d", i), "body"), t0.Add(time.Duration(i)*time.Hour))
	}

	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 256)
	check := func(what string, err error) {
		if err != nil {
			errs <- fmt.Errorf("%s: %w", what, err)
		}
	}
	wg.Add(3)
	go func() { // process 1: tags
		defer wg.Done()
		for i, id := range ids {
			_, err := e.cache.AddTags(ctx, e.acct.Name, fmt.Sprintf("p1/t%d", i%3), []string{id}, "h1")
			check("p1 AddTags", err)
		}
	}()
	go func() { // process 2: tags, sender decisions, removals
		defer wg.Done()
		for i, id := range ids {
			_, err := other.AddTags(ctx, e.acct.Name, fmt.Sprintf("p2/t%d", i%3), []string{id}, "h2")
			check("p2 AddTags", err)
			if i%5 == 0 {
				_, _, err := other.RemoveTags(ctx, e.acct.Name, fmt.Sprintf("p2/t%d", i%3), []string{id}, "")
				check("p2 RemoveTags", err)
			}
			if i%7 == 0 {
				// The sender may not have a row yet; only BUSY would be a failure.
				if _, err := other.SetSenderKind(ctx, e.acct.Name, "alice@example.com", KindHuman); err != nil && !strings.Contains(err.Error(), "not found") {
					check("p2 SetSenderKind", err)
				}
			}
		}
	}()
	go func() { // process 1: a refresh writing messages and senders
		defer wg.Done()
		check("p1 refresh", refreshErr(e))
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if strings.Contains(err.Error(), "SQLITE_BUSY") || strings.Contains(strings.ToLower(err.Error()), "database is locked") {
			t.Errorf("busy surfaced: %v", err)
		} else {
			t.Errorf("unexpected: %v", err)
		}
	}
	if n := e.count(`SELECT COUNT(*) FROM messages WHERE account = ?`, e.acct.Name); n != 40 {
		t.Errorf("%d messages after the refresh, want 40", n)
	}
	// Both handles see the same tags.
	a, err := e.cache.ListTags(ctx, e.acct.Name)
	if err != nil {
		t.Fatal(err)
	}
	b, err := other.ListTags(ctx, e.acct.Name)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(a) != fmt.Sprint(b) || len(a) == 0 {
		t.Errorf("tags differ between handles: %v vs %v", a, b)
	}
}

func refreshErr(e *env) error {
	c, err := imapxDial(e)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	_, err = e.cache.Refresh(e.ctx(), e.acct, c)
	return err
}

func TestSweepTempKeepsFreshAndRemovesStaleTempFiles(t *testing.T) {
	dir := t.TempDir()
	blobs := filepath.Join(dir, "blobs")
	fan := filepath.Join(blobs, "ab")
	if err := os.MkdirAll(fan, 0o700); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(fan, ".tmp-fresh")
	stale := filepath.Join(fan, ".tmp-stale")
	blob := filepath.Join(fan, "abcdef")
	for _, p := range []string{fresh, stale, blob} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-staleTemp - time.Minute)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	// A blob is never touched, however old.
	if err := os.Chtimes(blob, old, old); err != nil {
		t.Fatal(err)
	}
	// Just under the threshold is still in flight.
	young := filepath.Join(fan, ".tmp-young")
	if err := os.WriteFile(young, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ty := time.Now().Add(-staleTemp + 5*time.Minute)
	if err := os.Chtimes(young, ty, ty); err != nil {
		t.Fatal(err)
	}

	c, err := Open(filepath.Join(dir))
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()

	for p, want := range map[string]bool{fresh: true, young: true, blob: true, stale: false} {
		_, err := os.Stat(p)
		if got := err == nil; got != want {
			t.Errorf("%s exists = %v, want %v", filepath.Base(p), got, want)
		}
	}
}
