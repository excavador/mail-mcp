// Package imapx opens IMAP connections the way mail-mcp needs them: one
// account at a time, TLS verified or pinned, nothing ever written to the
// mailbox unless a caller asks.
package imapx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/excavador/mail-mcp/internal/accounts"
)

// tlsConfig builds the TLS configuration for an account.
//
// With a pin, chain validation is replaced -- not disabled -- by an exact
// match on the leaf certificate's digest. InsecureSkipVerify is set only so
// the standard library hands the raw certificates to VerifyPeerCertificate;
// that callback then refuses everything except the one pinned certificate.
// Without a pin, verification is the standard library's own, against the
// system roots, for the configured host.
func tlsConfig(a accounts.Account) *tls.Config {
	pin := a.Pin()
	if pin == nil {
		return &tls.Config{ServerName: a.Host, MinVersion: tls.VersionTLS12}
	}
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // see above: the callback below is the verification
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("server presented no certificate")
			}
			got := sha256.Sum256(raw[0])
			if !bytes.Equal(got[:], pin) {
				return fmt.Errorf("certificate pin mismatch: got %s", hex.EncodeToString(got[:]))
			}
			return nil
		},
	}
}

// Dial connects and logs in.
func Dial(ctx context.Context, a accounts.Account) (*imapclient.Client, error) {
	opts := &imapclient.Options{TLSConfig: tlsConfig(a)}

	var (
		c   *imapclient.Client
		err error
	)
	switch a.TLS {
	case accounts.Implicit:
		c, err = imapclient.DialTLS(a.Addr(), opts)
	case accounts.StartTLS:
		c, err = imapclient.DialStartTLS(a.Addr(), opts)
	default:
		return nil, fmt.Errorf("unknown tls mode %q", a.TLS)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: connect: %w", a.Name, err)
	}

	if err := c.Login(a.Username, a.Password()).Wait(); err != nil {
		_ = c.Close()
		// Never include the password, and keep the message generic: a wrong
		// password and a disabled app password look identical from here.
		return nil, fmt.Errorf("%s: login refused for %s", a.Name, a.Username)
	}
	return c, nil
}

// Folder is one mailbox and how many messages it holds.
type Folder struct {
	Name     string   `json:"name"`
	Messages uint32   `json:"messages"`
	Attrs    []string `json:"attributes,omitempty"`
}

// ListFolders returns every selectable folder with its message count.
//
// LIST then one STATUS per folder, rather than LIST-STATUS: Bridge does not
// advertise LIST-STATUS (its capability line is AUTH=PLAIN ID IDLE IMAP4rev1
// MOVE STARTTLS UIDPLUS UNSELECT), and one code path for both providers is
// worth a round trip per folder.
func ListFolders(ctx context.Context, c *imapclient.Client) ([]Folder, error) {
	list, err := c.List("", "*", nil).Collect()
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	var out []Folder
	for _, m := range list {
		f := Folder{Name: m.Mailbox}
		noSelect := false
		for _, at := range m.Attrs {
			f.Attrs = append(f.Attrs, string(at))
			if at == imap.MailboxAttrNoSelect || at == imap.MailboxAttrNonExistent {
				noSelect = true
			}
		}
		if noSelect {
			continue
		}
		st, err := c.Status(m.Mailbox, &imap.StatusOptions{NumMessages: true}).Wait()
		if err == nil && st.NumMessages != nil {
			f.Messages = *st.NumMessages
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
