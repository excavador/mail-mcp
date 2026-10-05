package cache

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/excavador/mail-mcp/internal/accounts"
)

func subjOf(t *testing.T, e *env, ms []Member) []string {
	t.Helper()
	ids := make([]string, len(ms))
	for i, m := range ms {
		ids[i] = m.StableID
	}
	hits, err := e.cache.Summaries(e.ctx(), "acct", ids)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, h := range hits {
		out = append(out, h.Subject)
	}
	sort.Strings(out)
	return out
}

func resolve(t *testing.T, e *env, q MemberQuery) []string {
	t.Helper()
	q.Folder = "INBOX"
	ms, err := e.cache.ResolveMembers(e.ctx(), "acct", q, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return subjOf(t, e, ms)
}

func add(e *env, id, from, subject string, extra ...string) {
	raw := []byte(strings.Join(append([]string{
		"From: " + from, "To: Bob <bob@example.com>", "Subject: " + subject,
		"Date: Mon, 02 Jan 2006 15:04:05 +0000", "Message-Id: <" + id + "@t>",
	}, extra...), "\r\n") + "\r\n\r\nbody " + id + "\r\n")
	e.appendMsg("INBOX", raw, t0)
}

func same(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	sort.Strings(want)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("%s = %q, want %q", what, got, want)
	}
}

func TestBareAddr(t *testing.T) {
	for in, want := range map[string]string{
		"Alice <Alice@Example.COM>": "alice@example.com",
		"  bob@x.org ":              "bob@x.org",
		"\"A, B\" <ab@x.org>":       "ab@x.org",
		"<c@x.org>":                 "c@x.org",
		"":                          "",
		"Name <broken":              "name <broken",
		"Name < spaced@x.org >":     "spaced@x.org",
	} {
		if got := BareAddr(in); got != want {
			t.Errorf("BareAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveFromIsExactBareAndCaseInsensitive(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX")
	add(e, "1", "Alice <alice@example.com>", "named")
	add(e, "2", "BOB@Example.com", "bare upper")
	add(e, "3", "Mallory <malice@example.com>", "lookalike")
	add(e, "4", "alice@example.com.evil.org", "suffix")
	e.refresh()
	for _, q := range []string{"alice@example.com", "ALICE@EXAMPLE.COM", "Alice <alice@example.com>", "Whoever <Alice@Example.Com>", "  alice@example.com "} {
		same(t, "from "+q, resolve(t, e, MemberQuery{From: q}), "named")
	}
	same(t, "bare upper", resolve(t, e, MemberQuery{From: "bob@example.com"}), "bare upper")
	same(t, "substring is not a match", resolve(t, e, MemberQuery{From: "lice@example.com"}))
	same(t, "prefix is not a match", resolve(t, e, MemberQuery{From: "alice@example.co"}))
	same(t, "domain only", resolve(t, e, MemberQuery{From: "example.com"}))
}

func TestResolveListIDWithAndWithoutBrackets(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX")
	add(e, "1", "a@x.org", "named", "List-Id: Dev List <dev.example.com>")
	add(e, "2", "a@x.org", "plain", "List-Id: plain.example.com")
	add(e, "3", "a@x.org", "other", "List-Id: <dev.example.com.evil>")
	add(e, "4", "a@x.org", "none")
	e.refresh()
	for _, q := range []string{"dev.example.com", "<dev.example.com>", " DEV.Example.COM ", "<DEV.example.com>"} {
		same(t, "list "+q, resolve(t, e, MemberQuery{ListID: q}), "named")
	}
	for _, q := range []string{"plain.example.com", "<plain.example.com>"} {
		same(t, "list "+q, resolve(t, e, MemberQuery{ListID: q}), "plain")
	}
	same(t, "partial list id", resolve(t, e, MemberQuery{ListID: "example.com"}))
}

func TestResolveGitHubReasonExactCaseInsensitive(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX")
	add(e, "1", "a@x.org", "review", "X-GitHub-Reason: review_requested")
	add(e, "2", "a@x.org", "mention", "X-GitHub-Reason: mention")
	add(e, "3", "a@x.org", "none")
	e.refresh()
	same(t, "reason", resolve(t, e, MemberQuery{GitHubReason: "Review_Requested"}), "review")
	same(t, "partial", resolve(t, e, MemberQuery{GitHubReason: "review"}))
	same(t, "wildcard", resolve(t, e, MemberQuery{GitHubReason: "%"}))
}

func TestResolveToAndSubjectAreLiteralSubstrings(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX")
	add(e, "1", "a@x.org", "100% done")
	add(e, "2", "a@x.org", "a_b")
	add(e, "3", "a@x.org", "axb")
	add(e, "4", "a@x.org", "1000 done")
	add(e, "5", "a@x.org", "Quoted 'x' and \"y\" ; DROP")
	e.refresh()
	same(t, "percent", resolve(t, e, MemberQuery{SubjectContains: "%"}), "100% done")
	same(t, "100%", resolve(t, e, MemberQuery{SubjectContains: "100%"}), "100% done")
	same(t, "underscore", resolve(t, e, MemberQuery{SubjectContains: "_"}), "a_b")
	same(t, "a_b not a?b", resolve(t, e, MemberQuery{SubjectContains: "a_b"}), "a_b")
	same(t, "case-insensitive", resolve(t, e, MemberQuery{SubjectContains: "DONE"}), "100% done", "1000 done")
	same(t, "sql quote", resolve(t, e, MemberQuery{SubjectContains: "'x' and \"y\" ; DROP"}), "Quoted 'x' and \"y\" ; DROP")
	same(t, "to substring", resolve(t, e, MemberQuery{To: "bob@"}), "100% done", "a_b", "axb", "1000 done", "Quoted 'x' and \"y\" ; DROP")
	same(t, "to percent", resolve(t, e, MemberQuery{To: "%"}))
	same(t, "to underscore", resolve(t, e, MemberQuery{To: "b_b"}))
	// Fields are ANDed.
	same(t, "and", resolve(t, e, MemberQuery{SubjectContains: "done", From: "a@x.org", To: "BOB"}), "100% done", "1000 done")
	same(t, "and none", resolve(t, e, MemberQuery{SubjectContains: "done", From: "z@x.org"}))
}

func TestResolveDatesReceivedAfterAndTooMany(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX")
	for i := range 5 {
		raw := []byte(fmt.Sprintf("From: a@x.org\r\nSubject: m%d\r\nDate: %s\r\nMessage-Id: <d%d@t>\r\n\r\nb\r\n", i, t0.Add(time.Duration(i)*24*time.Hour).Format(time.RFC1123Z), i))
		e.appendMsg("INBOX", raw, t0.Add(time.Duration(i)*24*time.Hour))
	}
	e.refresh()
	same(t, "since", resolve(t, e, MemberQuery{From: "a@x.org", Since: t0.Add(48 * time.Hour)}), "m2", "m3", "m4")
	same(t, "before", resolve(t, e, MemberQuery{From: "a@x.org", Before: t0.Add(48 * time.Hour)}), "m0", "m1")
	same(t, "received after", resolve(t, e, MemberQuery{From: "a@x.org", ReceivedAfter: t0.Add(72 * time.Hour)}), "m4")
	ms, err := e.cache.ResolveMembers(e.ctx(), "acct", MemberQuery{Folder: "INBOX", From: "a@x.org"}, 5)
	if err != nil || len(ms) != 5 {
		t.Fatalf("exactly max: %d, %v", len(ms), err)
	}
	if _, err := e.cache.ResolveMembers(e.ctx(), "acct", MemberQuery{Folder: "INBOX", From: "a@x.org"}, 4); !errors.Is(err, ErrTooMany) {
		t.Fatalf("over max: err = %v, want ErrTooMany (never a truncated set)", err)
	}
	// Newest first, and folder-scoped.
	ms, _ = e.cache.ResolveMembers(e.ctx(), "acct", MemberQuery{Folder: "INBOX", From: "a@x.org"}, 10)
	if got := subjOf(t, e, ms[:1]); got[0] != "m4" {
		t.Errorf("first = %v, want newest m4", got)
	}
	if ms, _ := e.cache.ResolveMembers(e.ctx(), "acct", MemberQuery{Folder: "Nowhere", From: "a@x.org"}, 10); len(ms) != 0 {
		t.Errorf("other folder resolved %d", len(ms))
	}
	if ms, _ := e.cache.ResolveMembers(e.ctx(), "other", MemberQuery{Folder: "INBOX", From: "a@x.org"}, 10); len(ms) != 0 {
		t.Errorf("other account resolved %d", len(ms))
	}
}

func TestMembersByIDAndSummaries(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX")
	add(e, "1", "a@x.org", "one")
	add(e, "2", "a@x.org", "two")
	e.refresh()
	all, _ := e.cache.ResolveMembers(e.ctx(), "acct", MemberQuery{Folder: "INBOX", From: "a@x.org"}, 10)
	ids := []string{}
	for i := range 1200 { // more than one chunk of 500
		ids = append(ids, fmt.Sprintf("mid:missing%d", i))
	}
	ids = append(ids, all[0].StableID, all[1].StableID)
	got, err := e.cache.MembersByID(e.ctx(), "acct", "INBOX", ids)
	if err != nil || len(got) != 2 {
		t.Fatalf("MembersByID = %d, %v", len(got), err)
	}
	if got[0].UID == 0 || got[0].UIDValidity == 0 || got[0].Folder != "INBOX" {
		t.Errorf("member = %+v", got[0])
	}
	if got, _ := e.cache.MembersByID(e.ctx(), "acct", "Other", ids); len(got) != 0 {
		t.Errorf("other folder returned %d", len(got))
	}
	if hs, err := e.cache.Summaries(e.ctx(), "acct", nil); err != nil || hs != nil {
		t.Errorf("Summaries(nil) = %v, %v", hs, err)
	}
	hs, err := e.cache.Summaries(e.ctx(), "acct", []string{all[0].StableID, "mid:unknown"})
	if err != nil || len(hs) != 1 || hs[0].StableID != all[0].StableID || hs[0].Subject == "" || hs[0].From == "" {
		t.Errorf("Summaries = %+v, %v", hs, err)
	}
}

func TestRefreshFoldersRefreshesOnlyTheNamedOnes(t *testing.T) {
	e := newEnv(t, accounts.Gmail, "INBOX", "Other")
	add(e, "1", "a@x.org", "in inbox")
	e.refresh()
	add(e, "2", "a@x.org", "second inbox")
	e.appendMsg("Other", mkMsg("o1", "in other", "b"), t0)
	c, err := imapxDial(e)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := e.cache.RefreshFolders(e.ctx(), c, e.acct, []string{"INBOX"}); err != nil {
		t.Fatal(err)
	}
	same(t, "inbox", resolve(t, e, MemberQuery{From: "a@x.org"}), "in inbox", "second inbox")
	if n := e.count(`SELECT COUNT(*) FROM membership WHERE folder = 'Other'`); n != 0 {
		t.Errorf("Other was refreshed too: %d memberships", n)
	}
	if _, err := e.cache.RefreshFolders(e.ctx(), c, e.acct, []string{"Other"}); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT COUNT(*) FROM membership WHERE folder = 'Other'`); n != 1 {
		t.Errorf("Other memberships = %d, want 1", n)
	}
	if _, err := e.cache.RefreshFolders(e.ctx(), c, e.acct, []string{"NoSuchFolder"}); err == nil {
		t.Error("refreshing a missing folder reported no error")
	}
}
