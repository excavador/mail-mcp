package accounts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodPin = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

// writeFile writes content into dir and returns the path.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// acct renders one YAML account entry; overrides replace whole lines by key.
func acct(pwFile string, over map[string]string) string {
	fields := map[string]string{
		"name":         "work",
		"provider":     "gmail",
		"host":         "imap.gmail.com",
		"port":         "993",
		"tls":          "implicit",
		"username":     "me@example.com",
		"passwordFile": pwFile,
	}
	for k, v := range over {
		fields[k] = v
	}
	order := []string{"name", "provider", "host", "port", "tls", "username", "passwordFile", "pinnedCertSHA256"}
	var b strings.Builder
	first := true
	for _, k := range order {
		v, ok := fields[k]
		if !ok || v == "<omit>" {
			continue
		}
		prefix := "    "
		if first {
			prefix = "  - "
			first = false
		}
		b.WriteString(prefix + k + ": " + v + "\n")
	}
	// extra unknown keys
	for k, v := range over {
		known := false
		for _, o := range order {
			if o == k {
				known = true
			}
		}
		if !known {
			b.WriteString("    " + k + ": " + v + "\n")
		}
	}
	return b.String()
}

func TestLoadRefuses(t *testing.T) {
	tests := []struct {
		name    string
		build   func(t *testing.T, dir, pw string) string // returns full YAML
		wantErr string
	}{
		{"empty list", func(*testing.T, string, string) string { return "accounts: []\n" }, "no accounts"},
		{"no accounts key", func(*testing.T, string, string) string { return "{}\n" }, "no accounts"},
		{"duplicate names", func(_ *testing.T, _, pw string) string {
			return "accounts:\n" + acct(pw, nil) + acct(pw, nil)
		}, "listed twice"},
		{"uppercase name", func(_ *testing.T, _, pw string) string {
			return "accounts:\n" + acct(pw, map[string]string{"name": "Work"})
		}, "name must be"},
		{"leading hyphen", func(_ *testing.T, _, pw string) string {
			return "accounts:\n" + acct(pw, map[string]string{"name": "-work"})
		}, "name must be"},
		{"name too long", func(_ *testing.T, _, pw string) string {
			return "accounts:\n" + acct(pw, map[string]string{"name": strings.Repeat("a", 64)})
		}, "name must be"},
		{"empty name", func(_ *testing.T, _, pw string) string {
			return "accounts:\n" + acct(pw, map[string]string{"name": `""`})
		}, "name must be"},
		{"unknown provider", func(_ *testing.T, _, pw string) string {
			return "accounts:\n" + acct(pw, map[string]string{"provider": "yahoo"})
		}, "provider must be"},
		{"missing host", func(_ *testing.T, _, pw string) string {
			return "accounts:\n" + acct(pw, map[string]string{"host": "<omit>"})
		}, "host is required"},
		{"port zero", func(_ *testing.T, _, pw string) string {
			return "accounts:\n" + acct(pw, map[string]string{"port": "0"})
		}, "port must be"},
		{"port too big", func(_ *testing.T, _, pw string) string {
			return "accounts:\n" + acct(pw, map[string]string{"port": "70000"})
		}, "port must be"},
		{"negative port", func(_ *testing.T, _, pw string) string {
			return "accounts:\n" + acct(pw, map[string]string{"port": "-1"})
		}, "port must be"},
		{"unknown tls", func(_ *testing.T, _, pw string) string {
			return "accounts:\n" + acct(pw, map[string]string{"tls": "none"})
		}, "tls must be"},
		{"missing tls", func(_ *testing.T, _, pw string) string {
			return "accounts:\n" + acct(pw, map[string]string{"tls": "<omit>"})
		}, "tls must be"},
		{"missing username", func(_ *testing.T, _, pw string) string {
			return "accounts:\n" + acct(pw, map[string]string{"username": "<omit>"})
		}, "username is required"},
		{"missing passwordFile", func(_ *testing.T, _, pw string) string {
			return "accounts:\n" + acct(pw, map[string]string{"passwordFile": "<omit>"})
		}, "passwordFile is required"},
		{"typo key", func(_ *testing.T, _, pw string) string {
			return "accounts:\n" + acct(pw, map[string]string{"pasword": "hunter2"})
		}, "pasword"},
		{"inline password key", func(_ *testing.T, _, pw string) string {
			return "accounts:\n" + acct(pw, map[string]string{"password": "hunter2"})
		}, "password"},
		{"insecure skip verify key", func(_ *testing.T, _, pw string) string {
			return "accounts:\n" + acct(pw, map[string]string{"insecureSkipVerify": "true"})
		}, "insecureSkipVerify"},
		{"typo top-level key", func(_ *testing.T, _, pw string) string {
			return "acounts:\n" + acct(pw, nil)
		}, "acounts"},
		{"pin too short", func(_ *testing.T, _, pw string) string {
			return "accounts:\n" + acct(pw, map[string]string{"pinnedCertSHA256": goodPin[:62]})
		}, "64 hex"},
		{"pin too long", func(_ *testing.T, _, pw string) string {
			return "accounts:\n" + acct(pw, map[string]string{"pinnedCertSHA256": goodPin + "00"})
		}, "64 hex"},
		{"pin non-hex", func(_ *testing.T, _, pw string) string {
			return "accounts:\n" + acct(pw, map[string]string{"pinnedCertSHA256": strings.Repeat("zz", 32)})
		}, "64 hex"},
		{"password file missing", func(_ *testing.T, dir, _ string) string {
			return "accounts:\n" + acct(filepath.Join(dir, "nope"), nil)
		}, "read password file"},
		{"password file empty", func(t *testing.T, dir, _ string) string {
			return "accounts:\n" + acct(writeFile(t, dir, "empty", ""), nil)
		}, "empty"},
		{"password file only newline", func(t *testing.T, dir, _ string) string {
			return "accounts:\n" + acct(writeFile(t, dir, "nl", "\r\n"), nil)
		}, "empty"},
		{"malformed yaml", func(*testing.T, string, string) string { return "accounts: [\n" }, "parse"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			pw := writeFile(t, dir, "pw", "secret\n")
			cfg := writeFile(t, dir, "accounts.yaml", tc.build(t, dir, pw))
			got, err := Load(cfg)
			if err == nil {
				t.Fatalf("expected refusal, got accounts %+v", got)
			}
			if got != nil {
				t.Errorf("expected nil accounts on error, got %+v", got)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadMissingAccountsFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("expected error for missing accounts file")
	}
}

func TestLoadAccepts(t *testing.T) {
	dir := t.TempDir()
	gpw := writeFile(t, dir, "gmail-pw", "app-password\n")
	ppw := writeFile(t, dir, "proton-pw", "bridge-pass")
	cfg := writeFile(t, dir, "accounts.yaml", "accounts:\n"+
		acct(gpw, nil)+
		acct(ppw, map[string]string{
			"name": "proton-1", "provider": "proton", "host": "bridge.local", "port": "143",
			"tls": "starttls", "username": "me@proton.me", "pinnedCertSHA256": goodPin,
		}))
	got, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 accounts, got %d", len(got))
	}

	g := got[0]
	if g.Name != "work" || g.Provider != Gmail || g.TLS != Implicit {
		t.Errorf("gmail account fields wrong: %+v", g)
	}
	if g.Password() != "app-password" {
		t.Errorf("Password() = %q, want trailing newline trimmed", g.Password())
	}
	if g.Pin() != nil {
		t.Errorf("Pin() = %x, want nil without a pin", g.Pin())
	}
	if g.Addr() != "imap.gmail.com:993" {
		t.Errorf("Addr() = %q", g.Addr())
	}

	p := got[1]
	if p.Provider != Proton || p.TLS != StartTLS {
		t.Errorf("proton account fields wrong: %+v", p)
	}
	if p.Password() != "bridge-pass" {
		t.Errorf("Password() = %q", p.Password())
	}
	pin := p.Pin()
	if len(pin) != 32 || pin[0] != 0x00 || pin[1] != 0x11 || pin[31] != 0xff {
		t.Errorf("Pin() = %x, want decoded %s", pin, goodPin)
	}
	if p.Addr() != "bridge.local:143" {
		t.Errorf("Addr() = %q", p.Addr())
	}
}

func TestPasswordTrimming(t *testing.T) {
	tests := []struct{ name, content, want string }{
		{"lf", "pw\n", "pw"},
		{"crlf", "pw\r\n", "pw"},
		{"none", "pw", "pw"},
		{"inner space kept", " p w \n", " p w "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			pw := writeFile(t, dir, "pw", tc.content)
			cfg := writeFile(t, dir, "a.yaml", "accounts:\n"+acct(pw, nil))
			got, err := Load(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if got[0].Password() != tc.want {
				t.Errorf("Password() = %q, want %q", got[0].Password(), tc.want)
			}
		})
	}
}

func TestNameBoundary(t *testing.T) {
	// 63 characters is the longest accepted name.
	dir := t.TempDir()
	pw := writeFile(t, dir, "pw", "x")
	cfg := writeFile(t, dir, "a.yaml", "accounts:\n"+acct(pw, map[string]string{"name": strings.Repeat("a", 63)}))
	if _, err := Load(cfg); err != nil {
		t.Fatalf("63-char name should be accepted: %v", err)
	}
}

func loadAliases(t *testing.T, aliasYAML string) ([]Account, error) {
	t.Helper()
	dir := t.TempDir()
	pw := writeFile(t, dir, "pw", "secret")
	cfg := writeFile(t, dir, "accounts.yaml", "accounts:\n"+acct(pw, map[string]string{"aliases": aliasYAML}))
	return Load(cfg)
}

func TestAliasesAccepted(t *testing.T) {
	got, err := loadAliases(t, "[\" Me.Alias@Example.COM \", \"@Example.org\", \"me+x@example.com\", \"me.alias@example.com\"]")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"me@example.com", "me.alias@example.com", "@example.org", "me+x@example.com"}
	if o := got[0].OwnerAddrs(); strings.Join(o, ",") != strings.Join(want, ",") {
		t.Errorf("OwnerAddrs() = %v, want %v", o, want)
	}
}

func TestAliasesOptional(t *testing.T) {
	dir := t.TempDir()
	pw := writeFile(t, dir, "pw", "secret")
	got, err := Load(writeFile(t, dir, "accounts.yaml", "accounts:\n"+acct(pw, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if o := got[0].OwnerAddrs(); len(o) != 1 || o[0] != "me@example.com" {
		t.Errorf("OwnerAddrs() = %v, want the username only", o)
	}
}

func TestAliasesRefused(t *testing.T) {
	for name, y := range map[string]string{
		"no at sign":      `["me.example.com"]`,
		"two at signs":    `["a@b@example.com"]`,
		"wildcard":        `["*@example.com"]`,
		"star domain":     `["@*.example.com"]`,
		"space":           `["me @example.com"]`,
		"empty":           `[""]`,
		"bare domain":     `["@localhost"]`,
		"empty domain":    `["me@"]`,
		"display name":    `["Me <me@example.com>"]`,
		"not a list":      `me@example.com`,
		"leading dot":     `["@.example.com"]`,
		"trailing hyphen": `["@example-.com"]`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadAliases(t, y)
			if err == nil {
				t.Fatalf("aliases %s accepted", y)
			}
			if name != "not a list" && !strings.Contains(err.Error(), "alias") {
				t.Errorf("error does not name the alias problem: %v", err)
			}
		})
	}
}

func TestOwns(t *testing.T) {
	got, err := loadAliases(t, `["me.alias@example.com", "@example.org"]`)
	if err != nil {
		t.Fatal(err)
	}
	a := got[0]
	for addr, want := range map[string]bool{
		"me@example.com":          true,
		"ME@Example.com":          true,
		" me@example.com ":        true,
		"me+news@example.com":     true, // plus-tagged username
		"me.alias@example.com":    true,
		"anyone@example.org":      true, // domain alias
		"x+y@example.org":         true,
		"anyone@sub.example.org":  false, // exactly that domain
		"other@example.com":       false,
		"me.alias+t@example.com":  true,
		"someone@example.net":     false,
		"":                        false,
		"example.com":             false,
		"@example.org":            false,
		"me@":                     false,
		"+x@example.org.evil.com": false,
	} {
		if g := a.Owns(addr); g != want {
			t.Errorf("Owns(%q) = %v, want %v", addr, g, want)
		}
	}
}

func TestDraftsSwitchAndDisplayName(t *testing.T) {
	load := func(over map[string]string) ([]Account, error) {
		dir := t.TempDir()
		pw := writeFile(t, dir, "pw", "secret")
		return Load(writeFile(t, dir, "accounts.yaml", "accounts:\n"+acct(pw, over)))
	}
	got, err := load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].DraftsEnabled() || got[0].DisplayName != "" {
		t.Errorf("defaults: drafts enabled = %v, display name %q", got[0].DraftsEnabled(), got[0].DisplayName)
	}
	got, err = load(map[string]string{"drafts": "false", "displayName": `"Oleg Tsarev"`})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].DraftsEnabled() || got[0].DisplayName != "Oleg Tsarev" {
		t.Errorf("drafts enabled = %v, display name %q", got[0].DraftsEnabled(), got[0].DisplayName)
	}
	if _, err := load(map[string]string{"displayName": `"a\nb"`}); err == nil {
		t.Error("a display name with a newline was accepted")
	}
}
