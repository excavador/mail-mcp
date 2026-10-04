// Package accounts loads and validates the mailboxes mail-mcp may reach.
//
// Two rules are enforced here rather than trusted to the deployment:
//
//   - A password never appears in the configuration file. It is read from a
//     file of its own (a mounted Secret), so the configuration can be shown,
//     logged and diffed without leaking a credential.
//
//   - TLS is either verified normally or PINNED to one certificate. There is
//     no "skip verification" setting at all. Proton Bridge presents a
//     self-signed certificate, and the honest answer to that is to pin it,
//     not to switch verification off and hope nothing else answers on the
//     port.
package accounts

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Provider names the mail service behind an account. It decides how folders
// behave: Gmail has labels (many per message), Proton has folders (one per
// message) and labels.
type Provider string

const (
	Gmail  Provider = "gmail"
	Proton Provider = "proton"
)

// TLSMode is how the IMAP connection is secured.
type TLSMode string

const (
	// Implicit is TLS from the first byte, IMAPS on 993. Gmail.
	Implicit TLSMode = "implicit"
	// StartTLS upgrades a plaintext connection. Proton Bridge on 143.
	StartTLS TLSMode = "starttls"
)

// Account is one mailbox.
type Account struct {
	Name         string   `yaml:"name"`
	Provider     Provider `yaml:"provider"`
	Host         string   `yaml:"host"`
	Port         int      `yaml:"port"`
	TLS          TLSMode  `yaml:"tls"`
	Username     string   `yaml:"username"`
	PasswordFile string   `yaml:"passwordFile"`
	// PinnedCertSHA256 is the SHA-256 of the server's leaf certificate in
	// DER form, hex. When set, the certificate chain is not validated
	// against a CA; the leaf must match this digest exactly. When empty, the
	// certificate is verified normally against the system roots.
	PinnedCertSHA256 string `yaml:"pinnedCertSHA256,omitempty"`

	password string
}

// Password returns the credential read from PasswordFile at load time.
func (a Account) Password() string { return a.password }

// Pin returns the decoded pin, or nil when the account uses normal
// verification.
func (a Account) Pin() []byte {
	if a.PinnedCertSHA256 == "" {
		return nil
	}
	b, _ := hex.DecodeString(a.PinnedCertSHA256) // validated at load
	return b
}

// Addr is host:port.
func (a Account) Addr() string { return fmt.Sprintf("%s:%d", a.Host, a.Port) }

type file struct {
	Accounts []Account `yaml:"accounts"`
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Load reads the accounts file and each account's password file.
func Load(path string) ([]Account, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read accounts file: %w", err)
	}
	var f file
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true) // an unknown key is a typo, not an option
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parse accounts file: %w", err)
	}
	if len(f.Accounts) == 0 {
		return nil, errors.New("accounts file lists no accounts")
	}

	seen := map[string]bool{}
	for i := range f.Accounts {
		a := &f.Accounts[i]
		if err := a.validate(); err != nil {
			return nil, fmt.Errorf("account %d (%q): %w", i, a.Name, err)
		}
		if seen[a.Name] {
			return nil, fmt.Errorf("account %q is listed twice", a.Name)
		}
		seen[a.Name] = true

		pw, err := os.ReadFile(a.PasswordFile)
		if err != nil {
			return nil, fmt.Errorf("account %q: read password file: %w", a.Name, err)
		}
		a.password = strings.TrimRight(string(pw), "\r\n")
		if a.password == "" {
			return nil, fmt.Errorf("account %q: password file is empty", a.Name)
		}
	}
	return f.Accounts, nil
}

func (a Account) validate() error {
	switch {
	case !nameRE.MatchString(a.Name):
		return errors.New("name must be lowercase letters, digits and hyphens")
	case a.Provider != Gmail && a.Provider != Proton:
		return fmt.Errorf("provider must be %q or %q", Gmail, Proton)
	case a.Host == "":
		return errors.New("host is required")
	case a.Port <= 0 || a.Port > 65535:
		return errors.New("port must be 1-65535")
	case a.TLS != Implicit && a.TLS != StartTLS:
		return fmt.Errorf("tls must be %q or %q", Implicit, StartTLS)
	case a.Username == "":
		return errors.New("username is required")
	case a.PasswordFile == "":
		return errors.New("passwordFile is required; a password is never written in this file")
	}
	if a.PinnedCertSHA256 != "" {
		b, err := hex.DecodeString(a.PinnedCertSHA256)
		if err != nil || len(b) != sha256.Size {
			return errors.New("pinnedCertSHA256 must be 64 hex characters")
		}
	}
	return nil
}
