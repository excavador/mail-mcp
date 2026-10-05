package organise

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/excavador/mail-mcp/internal/accounts"
	"github.com/excavador/mail-mcp/internal/cache"
)

func newOrg(t *testing.T) *Organiser {
	t.Helper()
	c, err := cache.Open(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	o, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func acct(p accounts.Provider) accounts.Account { return accounts.Account{Name: "acct", Provider: p} }

func mv(src, dst string) Intent {
	return Intent{Criterion: Criterion{Folder: src, From: "a@example.com"}, Target: dst, Action: ActionMove}
}

func lbl(src, dst string) Intent {
	i := mv(src, dst)
	i.Action = ActionLabel
	return i
}

func wantErr(t *testing.T, err error, contains string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want an error containing %q, got nil", contains)
	}
	if !IsSafe(err) {
		t.Errorf("error %q is not a SafeError, so it would not reach the client", err)
	}
	if !strings.Contains(err.Error(), contains) {
		t.Errorf("error %q lacks %q", err, contains)
	}
}

// ---- 4: criterion validation ----

func TestValidateRefusesFolderOnlyCriterion(t *testing.T) {
	for name, c := range map[string]Criterion{
		"folder only":        {Folder: "INBOX"},
		"dates only":         {Folder: "INBOX", Since: time.Unix(1, 0), Before: time.Unix(99, 0)},
		"whitespace-less ok": {Folder: "INBOX", From: "a@b"}, // control: this one passes
	} {
		err := Intent{Criterion: c, Target: "X", Action: ActionMove}.Validate(accounts.Gmail)
		if name == "whitespace-less ok" {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if !errors.Is(err, ErrNoMatcher) {
			t.Errorf("%s: err = %v, want ErrNoMatcher", name, err)
		}
	}
}

func TestValidateRefusesControlCharsAndLongValues(t *testing.T) {
	bad := map[string]string{
		"NUL":      "a\x00b",
		"newline":  "a\nb",
		"CR":       "a\rb",
		"tab":      "a\tb",
		"DEL":      "a\x7fb",
		"C1":       "a\u0085b",
		"long 257": strings.Repeat("x", 257),
		"bad utf8": "a\xffb",
	}
	for name, v := range bad {
		for field, set := range map[string]func(*Criterion){
			"from":    func(c *Criterion) { c.From = v },
			"to":      func(c *Criterion) { c.To = v },
			"subject": func(c *Criterion) { c.SubjectContains = v },
			"list_id": func(c *Criterion) { c.ListID = v },
			"gh":      func(c *Criterion) { c.GitHubReason = v },
		} {
			in := mv("INBOX", "X")
			in.Criterion.From = ""
			set(&in.Criterion)
			wantErr(t, in.Validate(accounts.Gmail), "criterion values")
			_ = name
			_ = field
		}
	}
	// The boundary itself is fine.
	in := mv("INBOX", "X")
	in.Criterion.From = strings.Repeat("x", 256)
	if err := in.Validate(accounts.Gmail); err != nil {
		t.Errorf("256-byte value refused: %v", err)
	}
	// A control character in the source folder name is refused as well.
	in = mv("IN\x00BOX", "X")
	wantErr(t, in.Validate(accounts.Gmail), "source folder")
}

// ---- 6 and 5 (rules part): provider rules ----

func TestProviderRules(t *testing.T) {
	type tc struct {
		p         accounts.Provider
		in        Intent
		wantError string // "" = allowed
	}
	cases := []tc{
		// Proton
		{accounts.Proton, mv("INBOX", "Labels/x"), "move needs a Folders/"},
		{accounts.Proton, lbl("INBOX", "Folders/x"), "label needs a Labels/"},
		{accounts.Proton, mv("INBOX", "Folders/x"), ""},
		{accounts.Proton, mv("Folders/y", "INBOX"), ""},
		{accounts.Proton, mv("INBOX", "Archive"), ""},
		{accounts.Proton, lbl("INBOX", "Labels/x"), ""},
		{accounts.Proton, mv("INBOX", "Trash"), "not valid targets"},
		{accounts.Proton, mv("INBOX", "Spam"), "not valid targets"},
		{accounts.Proton, mv("INBOX", "All Mail"), "not valid targets"},
		{accounts.Proton, lbl("INBOX", "Trash"), "not valid targets"},
		{accounts.Proton, lbl("INBOX", "All Mail"), "not valid targets"},
		{accounts.Proton, mv("INBOX", "spam"), "not valid targets"},
		{accounts.Proton, lbl("INBOX", "INBOX2"), "label needs a Labels/"},
		{accounts.Proton, lbl("INBOX", "Archive"), "label needs a Labels/"},
		// Gmail
		{accounts.Gmail, mv("[Gmail]/All Mail", "Work"), "cannot move out of [Gmail]/All Mail"},
		{accounts.Gmail, mv("[gmail]/all mail", "Work"), "cannot move out of [Gmail]/All Mail"},
		{accounts.Gmail, lbl("[Gmail]/All Mail", "Work"), ""},
		{accounts.Gmail, mv("INBOX", "[Gmail]/Trash"), "under [Gmail]/"},
		{accounts.Gmail, mv("INBOX", "[Gmail]/Spam"), "under [Gmail]/"},
		{accounts.Gmail, lbl("INBOX", "[Gmail]/Starred"), "under [Gmail]/"},
		{accounts.Gmail, lbl("INBOX", "[GMAIL]/Anything"), "under [Gmail]/"},
		{accounts.Gmail, mv("INBOX", "Work"), ""},
		{accounts.Gmail, lbl("INBOX", "Work/Sub"), ""},
		// Both
		{accounts.Gmail, mv("INBOX", "INBOX"), "same folder"},
		{accounts.Proton, mv("INBOX", "INBOX"), "same folder"},
		{accounts.Gmail, Intent{Criterion: Criterion{Folder: "INBOX", From: "a"}, Target: "X", Action: "delete"}, "action must be"},
		{accounts.Gmail, Intent{Criterion: Criterion{Folder: "INBOX", From: "a"}, Target: "", Action: ActionMove}, "target"},
		{accounts.Gmail, Intent{Criterion: Criterion{Folder: "INBOX", From: "a"}, Target: "a/../b", Action: ActionMove}, "target"},
	}
	for _, c := range cases {
		name := string(c.p) + " " + c.in.Action + " " + c.in.Criterion.Folder + "->" + c.in.Target
		t.Run(name, func(t *testing.T) {
			err := c.in.Validate(c.p)
			if c.wantError == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			wantErr(t, err, c.wantError)
		})
	}
}

// ---- 15: folder name validation ----

func TestValidateNewFolder(t *testing.T) {
	bad := map[string]string{
		"star":           "a*b",
		"percent":        "a%b",
		"NUL":            "a\x00b",
		"newline":        "a\nb",
		"tab":            "a\tb",
		"bidi override":  "a‮b",
		"zero width":     "a​b",
		"201 bytes":      strings.Repeat("x", 201),
		"leading slash":  "/a",
		"trailing slash": "a/",
		"empty segment":  "a//b",
		"dotdot":         "a/../b",
		"dotdot only":    "..",
		"dot segment":    "a/./b",
		"dot only":       ".",
		"empty":          "",
		"bad utf8":       "a\xffb",
		"trailing dots":  "a/b/..",
	}
	for n, name := range bad {
		if err := ValidateNewFolder(accounts.Gmail, name); err == nil {
			t.Errorf("%s (%q) accepted", n, name)
		} else if !IsSafe(err) {
			t.Errorf("%s: %v is not a SafeError", n, err)
		}
	}
	for _, name := range []string{"Work", "Work/Sub", strings.Repeat("x", 200), "Né/ü", "a.b", "a..b", "...x"} {
		if err := ValidateNewFolder(accounts.Gmail, name); err != nil {
			t.Errorf("%q refused: %v", name, err)
		}
	}
	// Proton prefix rule.
	for _, name := range []string{"Work", "folders/x", "Archive", "Labels", "Folders"} {
		wantErr(t, ValidateNewFolder(accounts.Proton, name), "Folders/ or Labels/")
	}
	for _, name := range []string{"Folders/x", "Labels/y", "Folders/a/b"} {
		if err := ValidateNewFolder(accounts.Proton, name); err != nil {
			t.Errorf("%q refused on proton: %v", name, err)
		}
	}
	// Gmail reserves [Gmail].
	wantErr(t, ValidateNewFolder(accounts.Gmail, "[Gmail]/Mine"), "reserved")
	wantErr(t, ValidateNewFolder(accounts.Gmail, "[gmail]"), "reserved")
}

// ---- 2: tokens ----

func TestTokenShapeAndTTL(t *testing.T) {
	o := newOrg(t)
	p, err := o.PreviewIntent(context.Background(), acct(accounts.Gmail), mv("INBOX", "X"), KindApply, time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	exp, mac, ok := strings.Cut(p.Token, ".")
	if !ok || mac == "" {
		t.Fatalf("token %q is not <expiry>.<mac>", p.Token)
	}
	if n, err := strconv.ParseInt(exp, 10, 64); err != nil || n != p.Expires.Unix() {
		t.Fatalf("token expiry %q != Expires %v", exp, p.Expires)
	}
	if d := time.Until(p.Expires); d > PreviewTTL || d < PreviewTTL-5*time.Second {
		t.Errorf("ttl = %v, want about %v", d, PreviewTTL)
	}
	if PreviewTTL != 15*time.Minute {
		t.Errorf("PreviewTTL = %v, want 15m", PreviewTTL)
	}
	if got, err := o.Lookup(p.Token); err != nil || got != p {
		t.Fatalf("Lookup of a fresh token = %v, %v", got, err)
	}
}

func TestLookupRefusesUnknownTamperedExpiredAndForeignTokens(t *testing.T) {
	o, o2 := newOrg(t), newOrg(t)
	ctx := context.Background()
	p, err := o.PreviewIntent(ctx, acct(accounts.Gmail), mv("INBOX", "X"), KindApply, time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	exp, mac, _ := strings.Cut(p.Token, ".")
	flip := func(s string) string {
		b := []byte(s)
		if b[len(b)-1] == 'A' {
			b[len(b)-1] = 'B'
		} else {
			b[len(b)-1] = 'A'
		}
		return string(b)
	}
	n, _ := strconv.ParseInt(exp, 10, 64)

	t.Run("unknown", func(t *testing.T) {
		for _, tok := range []string{"", "x", ".", "1.2", "nonsense"} {
			if _, err := o.Lookup(tok); !errors.Is(err, ErrExpired) {
				t.Errorf("Lookup(%q) = %v, want ErrExpired", tok, err)
			}
		}
	})
	t.Run("one char of the mac changed", func(t *testing.T) {
		if _, err := o.Lookup(exp + "." + flip(mac)); !errors.Is(err, ErrExpired) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("expiry prefix changed", func(t *testing.T) {
		if _, err := o.Lookup(strconv.FormatInt(n+3600, 10) + "." + mac); !errors.Is(err, ErrExpired) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("another organiser", func(t *testing.T) {
		if _, err := o2.Lookup(p.Token); !errors.Is(err, ErrExpired) {
			t.Errorf("err = %v", err)
		}
	})
	// The three below put a forged entry in the map, so what refuses them is
	// the MAC check, not the "unknown token" branch.
	t.Run("mac is checked, not just the map", func(t *testing.T) {
		forged := exp + "." + flip(mac)
		o.mu.Lock()
		o.previews[forged] = p
		o.mu.Unlock()
		if _, err := o.Lookup(forged); !errors.Is(err, ErrExpired) {
			t.Errorf("forged token under a stored preview accepted: %v", err)
		}
		forged2 := strconv.FormatInt(n+3600, 10) + "." + mac
		o.mu.Lock()
		o.previews[forged2] = p
		o.mu.Unlock()
		if _, err := o.Lookup(forged2); !errors.Is(err, ErrExpired) {
			t.Errorf("token with edited expiry accepted: %v", err)
		}
	})
	t.Run("signed by another key", func(t *testing.T) {
		o2.mu.Lock()
		o2.previews[p.Token] = p
		o2.mu.Unlock()
		if _, err := o2.Lookup(p.Token); !errors.Is(err, ErrExpired) {
			t.Errorf("a token signed by another organiser's key was accepted: %v", err)
		}
	})
	t.Run("expired", func(t *testing.T) {
		q, err := o.PreviewIntent(ctx, acct(accounts.Gmail), mv("INBOX", "Y"), KindApply, time.Time{}, "")
		if err != nil {
			t.Fatal(err)
		}
		q.Expires = time.Now().Add(-time.Second)
		if _, err := o.Lookup(q.Token); !errors.Is(err, ErrExpired) {
			t.Errorf("expired token accepted: %v", err)
		}
		if ErrExpired.Error() != "preview expired or unknown; preview again" {
			t.Errorf("ErrExpired text = %q", ErrExpired)
		}
	})
	t.Run("preview mutated after issue", func(t *testing.T) {
		q, err := o.PreviewIntent(ctx, acct(accounts.Gmail), mv("INBOX", "Z"), KindApply, time.Time{}, "")
		if err != nil {
			t.Fatal(err)
		}
		q.IDs = append(q.IDs, "mid:smuggled")
		if _, err := o.Lookup(q.Token); !errors.Is(err, ErrExpired) {
			t.Errorf("a preview whose ids changed after signing was accepted: %v", err)
		}
		q2, _ := o.PreviewIntent(ctx, acct(accounts.Gmail), mv("INBOX", "Z2"), KindApply, time.Time{}, "")
		q2.Intent.Target = "Elsewhere"
		if _, err := o.Lookup(q2.Token); !errors.Is(err, ErrExpired) {
			t.Errorf("a preview whose target changed after signing was accepted: %v", err)
		}
	})
}

func TestConsumeForgetsTheTokenAndSucceedsOnlyOnce(t *testing.T) {
	o := newOrg(t)
	p, err := o.PreviewIntent(context.Background(), acct(accounts.Gmail), mv("INBOX", "X"), KindApply, time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !o.Consume(p.Token) {
		t.Fatal("first Consume of a live token = false")
	}
	if _, err := o.Lookup(p.Token); !errors.Is(err, ErrExpired) {
		t.Fatalf("consumed token still valid: %v", err)
	}
	if o.Consume(p.Token) {
		t.Error("second Consume = true: a token approved two applies")
	}
	if o.Consume("never issued") {
		t.Error("Consume of an unknown token = true")
	}
	q, _ := o.PreviewIntent(context.Background(), acct(accounts.Gmail), mv("INBOX", "X"), KindApply, time.Time{}, "")
	q.Expires = time.Now().Add(-time.Second)
	if o.Consume(q.Token) {
		t.Error("Consume of an expired token = true")
	}
}

func TestRacingConsumesYieldExactlyOneWinner(t *testing.T) {
	o := newOrg(t)
	p, _ := o.PreviewIntent(context.Background(), acct(accounts.Gmail), mv("INBOX", "X"), KindApply, time.Time{}, "")
	wins := make(chan bool, 16)
	for range 16 {
		go func() { wins <- o.Consume(p.Token) }()
	}
	n := 0
	for range 16 {
		select {
		case w := <-wins:
			if w {
				n++
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out")
		}
	}
	if n != 1 {
		t.Errorf("%d winners, want 1", n)
	}
}

func TestIdenticalPreviewsGetDistinctTokens(t *testing.T) {
	o := newOrg(t)
	a, _ := o.PreviewIntent(context.Background(), acct(accounts.Gmail), mv("INBOX", "X"), KindApply, time.Time{}, "")
	b, _ := o.PreviewIntent(context.Background(), acct(accounts.Gmail), mv("INBOX", "X"), KindApply, time.Time{}, "")
	if a.Token == b.Token || a.Nonce == b.Nonce {
		t.Fatalf("identical previews share a token: %s", a.Token)
	}
	o.Consume(a.Token)
	if _, err := o.Lookup(b.Token); err != nil {
		t.Errorf("consuming one preview killed its twin: %v", err)
	}
}

func TestNamesAndTargetsStartingWithBracketAreRefused(t *testing.T) {
	for _, p := range []accounts.Provider{accounts.Gmail, accounts.Proton} {
		for _, name := range []string{"[Gmail]/Trash", "[Google Mail]/Bin", "[x", "["} {
			wantErr(t, ValidateNewFolder(p, name), "[")
			for _, act := range []string{ActionMove, ActionLabel} {
				in := mv("INBOX", name)
				in.Action = act
				if err := in.Validate(p); err == nil {
					t.Errorf("%s %s target %q accepted", p, act, name)
				}
			}
		}
	}
	// INBOX stays a valid Gmail target; a name with [ later on is fine.
	if err := mv("Work", "INBOX").Validate(accounts.Gmail); err != nil {
		t.Errorf("gmail INBOX target refused: %v", err)
	}
	if err := ValidateNewFolder(accounts.Gmail, "a[1]"); err != nil {
		t.Errorf("a[1] refused: %v", err)
	}
}

func TestOnly32PreviewsAreHeldOldestEvicted(t *testing.T) {
	o := newOrg(t)
	var toks []string
	for i := range 33 {
		p, err := o.PreviewIntent(context.Background(), acct(accounts.Gmail), mv("INBOX", "T"+strconv.Itoa(i)), KindApply, time.Time{}, "")
		if err != nil {
			t.Fatal(err)
		}
		toks = append(toks, p.Token)
	}
	if _, err := o.Lookup(toks[0]); !errors.Is(err, ErrExpired) {
		t.Errorf("33rd preview did not evict the oldest: %v", err)
	}
	for i, tok := range toks[1:] {
		if _, err := o.Lookup(tok); err != nil {
			t.Errorf("preview %d evicted too early: %v", i+1, err)
		}
	}
	o.mu.Lock()
	n, m := len(o.previews), len(o.order)
	o.mu.Unlock()
	if n != 32 || m != 32 {
		t.Errorf("held %d previews / %d order entries, want 32", n, m)
	}
}

func TestPreviewIDsChecksTheMoveRules(t *testing.T) {
	o := newOrg(t)
	ctx := context.Background()
	// An undo needs no matcher...
	in := Intent{Criterion: Criterion{Folder: "Folders/x"}, Target: "INBOX", Action: ActionMove}
	p, err := o.PreviewIDs(ctx, acct(accounts.Proton), in, []string{"pm:1", "pm:2"}, []string{"pm:3"}, 1, "rec1")
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != KindUndo || !p.IDsOnly || p.Undoes != "rec1" || p.Missing != 1 || p.Matched != 3 || len(p.CopyBack) != 1 {
		t.Errorf("preview = %+v", p)
	}
	// ...but the provider rules still hold.
	_, err = o.PreviewIDs(ctx, acct(accounts.Proton), Intent{Criterion: Criterion{Folder: "Folders/x"}, Target: "Labels/y", Action: ActionMove}, []string{"pm:1"}, nil, 0, "r")
	wantErr(t, err, "move needs a Folders/")
	// PreviewIntent refuses a folder-only criterion; PreviewIDs does not need one.
	_, err = o.PreviewIntent(ctx, acct(accounts.Gmail), Intent{Criterion: Criterion{Folder: "INBOX"}, Target: "X", Action: ActionMove}, KindApply, time.Time{}, "")
	if !errors.Is(err, ErrNoMatcher) {
		t.Errorf("err = %v", err)
	}
}

func TestAcquireIsPerAccountAndDoesNotWait(t *testing.T) {
	o := newOrg(t)
	rel, err := o.Acquire("a")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := o.Acquire("a"); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrBusy) || err.Error() != "another write is in progress" {
			t.Errorf("second Acquire = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second Acquire blocked instead of refusing")
	}
	relB, err := o.Acquire("b")
	if err != nil {
		t.Fatalf("another account was blocked: %v", err)
	}
	relB()
	rel()
	rel2, err := o.Acquire("a")
	if err != nil {
		t.Fatalf("slot not freed by release: %v", err)
	}
	rel2()
}
