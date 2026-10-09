package accounts

import (
	"strings"
	"testing"
)

const (
	calID     = "CID-leakcheck-111.apps.example"
	calSecret = "CSECRET-leakcheck-222"
	calToken  = "RTOKEN-leakcheck-333"
)

// calBlock renders a calendar section; a key mapped to "<omit>" is left out,
// other entries replace the defaults or add keys.
func calBlock(dir string, t *testing.T, over map[string]string) string {
	t.Helper()
	fields := map[string]string{
		"provider":         "google",
		"clientIdFile":     writeFile(t, dir, "cid", calID+"\n"),
		"clientSecretFile": writeFile(t, dir, "csecret", "  "+calSecret+" \r\n"),
		"refreshTokenFile": writeFile(t, dir, "rtoken", "\n"+calToken+"\n\n"),
	}
	for k, v := range over {
		fields[k] = v
	}
	var b strings.Builder
	b.WriteString("    calendar:\n")
	for _, k := range []string{"provider", "clientIdFile", "clientSecretFile", "refreshTokenFile", "write"} {
		if v, ok := fields[k]; ok && v != "<omit>" {
			b.WriteString("      " + k + ": " + v + "\n")
			delete(fields, k)
		}
	}
	for k, v := range fields {
		if v != "<omit>" {
			b.WriteString("      " + k + ": " + v + "\n")
		}
	}
	return b.String()
}

func TestLoadReadsAndTrimsTheCalendarCredentials(t *testing.T) {
	dir := t.TempDir()
	pw := writeFile(t, dir, "pw", "secret\n")
	cfg := writeFile(t, dir, "accounts.yaml", "accounts:\n"+acct(pw, nil)+calBlock(dir, t, map[string]string{"write": "true"})+
		acct(pw, map[string]string{"name": "plain"}))
	got, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c := got[0].Calendar
	if c == nil || c.Provider != GoogleCalendar || !c.Write {
		t.Fatalf("calendar = %+v", c)
	}
	if c.ClientID() != calID || c.ClientSecret() != calSecret || c.RefreshToken() != calToken {
		t.Errorf("credentials = %q %q %q (want trimmed values)", c.ClientID(), c.ClientSecret(), c.RefreshToken())
	}
	if got[1].Calendar != nil {
		t.Error("an account without a calendar section has one")
	}

	// write defaults to false.
	cfg = writeFile(t, dir, "accounts2.yaml", "accounts:\n"+acct(pw, nil)+calBlock(dir, t, nil))
	got, err = Load(cfg)
	if err != nil || got[0].Calendar == nil || got[0].Calendar.Write {
		t.Fatalf("default write: %+v, %v", got, err)
	}
}

func TestLoadRefusesABadCalendarWithoutLeakingValues(t *testing.T) {
	dir := t.TempDir()
	pw := writeFile(t, dir, "pw", "secret\n")
	empty := writeFile(t, dir, "empty", " \n\t\n")
	absent := dir + "/absent-file"
	for name, tc := range map[string]struct {
		over map[string]string
		want string
	}{
		"proton provider":   {map[string]string{"provider": "proton"}, "calendar.provider must be"},
		"no provider":       {map[string]string{"provider": "<omit>"}, "calendar.provider must be"},
		"caps provider":     {map[string]string{"provider": "Google"}, "calendar.provider must be"},
		"missing id key":    {map[string]string{"clientIdFile": "<omit>"}, "needs clientIdFile, clientSecretFile and refreshTokenFile"},
		"missing secret":    {map[string]string{"clientSecretFile": "<omit>"}, "needs clientIdFile"},
		"missing token":     {map[string]string{"refreshTokenFile": "<omit>"}, "needs clientIdFile"},
		"id file absent":    {map[string]string{"clientIdFile": absent}, "calendar.clientIdFile cannot be read"},
		"secret absent":     {map[string]string{"clientSecretFile": absent}, "calendar.clientSecretFile cannot be read"},
		"token absent":      {map[string]string{"refreshTokenFile": absent}, "calendar.refreshTokenFile cannot be read"},
		"id file empty":     {map[string]string{"clientIdFile": empty}, "calendar.clientIdFile is empty"},
		"secret file empty": {map[string]string{"clientSecretFile": empty}, "calendar.clientSecretFile is empty"},
		"token file empty":  {map[string]string{"refreshTokenFile": empty}, "calendar.refreshTokenFile is empty"},
		"unknown key":       {map[string]string{"refreshToken": calToken}, "refreshToken"},
		"inline secret":     {map[string]string{"clientSecret": calSecret}, "clientSecret"},
		"typo":              {map[string]string{"writes": "true"}, "writes"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := writeFile(t, dir, "a.yaml", "accounts:\n"+acct(pw, nil)+calBlock(dir, t, tc.over))
			got, err := Load(cfg)
			if err == nil {
				t.Fatalf("accepted: %+v", got)
			}
			if got != nil {
				t.Errorf("accounts returned with an error: %+v", got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q lacks %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), `account "work"`) && !strings.Contains(err.Error(), "refreshToken") && !strings.Contains(err.Error(), "clientSecret") && !strings.Contains(err.Error(), "writes") {
				t.Errorf("error does not name the account: %q", err)
			}
			for _, s := range []string{calID, calSecret, calToken, "secret\n"} {
				if strings.Contains(err.Error(), strings.TrimSpace(s)) && strings.TrimSpace(s) != "" && s != "secret\n" {
					t.Errorf("error leaks %q: %v", s, err)
				}
			}
			// An unreadable file's path may be named by the file system error; the file's contents never are.
			if strings.Contains(err.Error(), absent) {
				t.Errorf("error leaks the path/os error: %v", err)
			}
		})
	}
}

func TestCalendarForTestCarriesCredentials(t *testing.T) {
	c := NewCalendarForTest("i", "s", "r", true)
	if c.Provider != GoogleCalendar || !c.Write || c.ClientID() != "i" || c.ClientSecret() != "s" || c.RefreshToken() != "r" {
		t.Errorf("%+v", c)
	}
}
