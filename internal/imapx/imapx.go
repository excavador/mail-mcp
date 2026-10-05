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
	"net"
	"sort"
	"time"

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

// ErrLogin is what errors.Is matches when the server refused the credentials.
var ErrLogin = errors.New("login refused")

type loginError struct{ msg string }

func (e loginError) Error() string        { return e.msg }
func (e loginError) Is(target error) bool { return target == ErrLogin }

// connectTimeout bounds TCP connect, TLS handshake, greeting and login
// together. A server that accepts the connection and then says nothing must
// not be able to hold a caller forever.
const connectTimeout = 30 * time.Second

// Dial connects and logs in. It honours ctx and gives up after connectTimeout.
// Cancelling ctx later also closes the returned client.
//
// The connection is dialled here, not by imapclient.Dial*, because those take
// no context: a deadline on the raw connection covers the handshake and the
// login, and a watcher closes the client if ctx ends mid-way.
func Dial(ctx context.Context, a accounts.Account) (*imapclient.Client, error) {
	opts := &imapclient.Options{TLSConfig: tlsConfig(a)}

	d := &net.Dialer{Timeout: connectTimeout}
	conn, err := d.DialContext(ctx, "tcp", a.Addr())
	if err != nil {
		return nil, fmt.Errorf("%s: connect: %w", a.Name, err)
	}
	_ = conn.SetDeadline(time.Now().Add(connectTimeout))

	var c *imapclient.Client
	switch a.TLS {
	case accounts.Implicit:
		tc := tls.Client(conn, opts.TLSConfig)
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("%s: connect: %w", a.Name, err)
		}
		c = imapclient.New(tc, opts)
	case accounts.StartTLS:
		c, err = imapclient.NewStartTLS(conn, opts)
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("%s: connect: %w", a.Name, err)
		}
	default:
		_ = conn.Close()
		return nil, fmt.Errorf("unknown tls mode %q", a.TLS)
	}

	// The watcher lives as long as the client, not just the login: a ctx
	// that ends mid-session (a listing that overruns its deadline) closes
	// the connection and unblocks whatever command is waiting on it.
	go func() {
		select {
		case <-ctx.Done():
			_ = c.Close()
		case <-c.Closed():
		}
	}()

	if err := c.Login(a.Username, a.Password()).Wait(); err != nil {
		_ = c.Close()
		// Never include the password, and keep the message generic: a wrong
		// password and a disabled app password look identical from here.
		return nil, loginError{fmt.Sprintf("%s: login refused for %s", a.Name, a.Username)}
	}
	// Past login the deadline would kill a healthy long session; from here
	// the caller's context is the bound.
	_ = conn.SetDeadline(time.Time{})
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
		if err := ctx.Err(); err != nil {
			return nil, err
		}
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
