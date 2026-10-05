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
	"log/slog"
	"net"
	"sort"
	"sync/atomic"
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
// no context: a watcher closes the raw connection the moment ctx ends, from
// the TCP connect on, so the TLS handshake, the STARTTLS exchange, the login
// and every later command are all unblocked by ctx. (go-imap clears the read
// deadline between responses, so a server that goes quiet is bounded by ctx
// and by nothing else.)
func Dial(ctx context.Context, a accounts.Account) (*imapclient.Client, error) {
	opts := &imapclient.Options{TLSConfig: tlsConfig(a)}

	SetPhase(ctx, "connect")
	d := &net.Dialer{Timeout: connectTimeout}
	conn, err := d.DialContext(ctx, "tcp", a.Addr())
	if err != nil {
		return nil, fmt.Errorf("%s: connect: %w", a.Name, err)
	}
	_ = conn.SetDeadline(time.Now().Add(connectTimeout))

	// Stage one of the watcher: until Dial returns, ctx ending closes conn.
	handoff := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-handoff:
		}
	}()

	var c *imapclient.Client
	switch a.TLS {
	case accounts.Implicit:
		SetPhase(ctx, "tls")
		tc := tls.Client(conn, opts.TLSConfig)
		if err := tc.HandshakeContext(ctx); err != nil {
			close(handoff)
			_ = conn.Close()
			return nil, fmt.Errorf("%s: connect: %w", a.Name, err)
		}
		c = imapclient.New(tc, opts)
	case accounts.StartTLS:
		SetPhase(ctx, "starttls")
		c, err = imapclient.NewStartTLS(conn, opts)
		if err != nil {
			close(handoff)
			_ = conn.Close()
			return nil, fmt.Errorf("%s: connect: %w", a.Name, err)
		}
	default:
		close(handoff)
		_ = conn.Close()
		return nil, fmt.Errorf("unknown tls mode %q", a.TLS)
	}

	// Stage two lives as long as the client, not just the login: a ctx that
	// ends mid-session closes the raw connection, which fails the read loop
	// and completes every pending command with an error. The raw connection
	// is closed (not c.Close): that call waits for the read loop to finish.
	close(handoff)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-c.Closed():
		}
	}()

	SetPhase(ctx, "login")
	if err := c.Login(a.Username, a.Password()).Wait(); err != nil {
		_ = conn.Close()
		go func() { _ = c.Close() }()
		// Never include the password, and keep the message generic: a wrong
		// password and a disabled app password look identical from here.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%s: login: %w", a.Name, ctx.Err())
		}
		return nil, loginError{fmt.Sprintf("%s: login refused for %s", a.Name, a.Username)}
	}
	// Past login the deadline would kill a healthy long session; from here
	// the caller's context is the bound.
	_ = conn.SetDeadline(time.Time{})
	return c, nil
}

// ErrTimeout is what errors.Is matches when Do's budget ran out before the
// mail server answered.
var ErrTimeout = errors.New("mail server did not answer in time")

// Phase records how far a session got, for the log of one that did not finish.
type Phase struct{ v atomic.Value }

// Get returns the last phase reported ("" before any).
func (p *Phase) Get() string {
	s, _ := p.v.Load().(string)
	return s
}

type phaseKey struct{}

// WithPhase returns a context whose SetPhase calls land in the returned Phase.
func WithPhase(ctx context.Context) (context.Context, *Phase) {
	p := &Phase{}
	return context.WithValue(ctx, phaseKey{}, p), p
}

// SetPhase reports the step about to run (connect, tls, login, list,
// examine, search, ...). It does nothing on a context without WithPhase.
func SetPhase(ctx context.Context, name string) {
	if p, ok := ctx.Value(phaseKey{}).(*Phase); ok {
		p.v.Store(name)
	}
}

// graceAfterTimeout is how long Do waits, once the budget is spent and the
// connection closed, for fn to come back and report what it had done.
const graceAfterTimeout = 3 * time.Second

// Do runs fn on a logged-in client under a hard budget: Do returns within
// budget plus graceAfterTimeout whatever the server does. The dial and fn run
// in their own goroutine; when the budget ends the connection is closed (which
// unblocks every pending command) and Do returns ErrTimeout without waiting
// longer than the grace period, so a caller holding a slot always gets it back.
//
// fn must not touch state the caller reads after a timeout without its own
// lock: it may still be winding down. Every call logs account, tool, the phase
// reached, the duration and the outcome.
func Do(ctx context.Context, a accounts.Account, tool string, budget time.Duration, fn func(ctx context.Context, c *imapclient.Client) error) error {
	start := time.Now()
	pctx, ph := WithPhase(ctx)
	cctx, cancel := context.WithTimeout(pctx, budget)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("imap session panic: %v", r)
			}
		}()
		c, err := Dial(cctx, a)
		if err != nil {
			done <- err
			return
		}
		err = fn(cctx, c)
		done <- err
		// Off the caller's path: say goodbye, then drop the connection.
		_ = c.Logout()
		cancel()
		_ = c.Close()
	}()

	var err error
	select {
	case err = <-done:
	case <-cctx.Done():
		// The watcher has closed (or is closing) the connection; give fn a
		// moment to return so work it finished is reported.
		select {
		case err = <-done:
		case <-time.After(graceAfterTimeout):
			err = cctx.Err()
		}
	}
	outcome := "ok"
	switch {
	case err == nil:
	case errors.Is(cctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		outcome = "timeout"
		err = fmt.Errorf("%w (phase %s, after %s): %v", ErrTimeout, ph.Get(), time.Since(start).Round(time.Millisecond), err)
	case ctx.Err() != nil:
		outcome = "canceled"
	default:
		outcome = "error"
	}
	attrs := []any{"account", a.Name, "tool", tool, "phase", ph.Get(), "outcome", outcome, "duration", time.Since(start).Round(time.Millisecond).String()}
	if outcome == "ok" {
		slog.Info("imap session", attrs...)
	} else {
		slog.Warn("imap session", append(attrs, "err", err)...)
	}
	return err
}

// Folder is one mailbox and how many messages it holds.
type Folder struct {
	Name     string   `json:"name"`
	Messages uint32   `json:"messages"`
	Attrs    []string `json:"attributes,omitempty"`
}

// ListFolders returns every selectable folder with its message count.
//
// LIST then one pipelined STATUS batch, rather than LIST-STATUS: Bridge does not
// advertise LIST-STATUS (its capability line is AUTH=PLAIN ID IDLE IMAP4rev1
// MOVE STARTTLS UIDPLUS UNSELECT), and one code path for both providers is
// worth a round trip per folder.
func ListFolders(ctx context.Context, c *imapclient.Client) ([]Folder, error) {
	SetPhase(ctx, "list")
	list, err := c.List("", "*", nil).Collect()
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	SetPhase(ctx, "status")
	// Every STATUS is sent before any is waited for: the server's latency is
	// paid once, not once per folder.
	type item struct {
		f   Folder
		cmd *imapclient.StatusCommand
	}
	var items []item
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
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		items = append(items, item{f: f, cmd: c.Status(m.Mailbox, &imap.StatusOptions{NumMessages: true})})
	}
	var out []Folder
	for _, it := range items {
		st, err := it.cmd.Wait()
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return nil, fmt.Errorf("status: %w", cerr) // the session ended, not a folder that refuses STATUS
			}
		} else if st.NumMessages != nil {
			it.f.Messages = *st.NumMessages
		}
		out = append(out, it.f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ErrNoGmailExt means the server does not advertise X-GM-EXT-1.
var ErrNoGmailExt = errors.New("server does not support the Gmail extensions (X-GM-EXT-1)")

// gmailAllMailFallback is the folder searched when LIST marks none \All.
const gmailAllMailFallback = "[Gmail]/All Mail"

// GmailAllMail returns the name of the folder holding every message: the one
// LIST marks with the \All special-use attribute, else "[Gmail]/All Mail".
func GmailAllMail(c *imapclient.Client) (string, error) {
	list, err := c.List("", "*", nil).Collect()
	if err != nil {
		return "", fmt.Errorf("list: %w", err)
	}
	for _, m := range list {
		for _, at := range m.Attrs {
			if at == imap.MailboxAttrAll {
				return m.Mailbox, nil
			}
		}
	}
	return gmailAllMailFallback, nil
}

// GmailRawSearch is GmailRawSearchIn with no known All Mail name (it LISTs).
func GmailRawSearch(ctx context.Context, c *imapclient.Client, query string) (string, []imap.UID, error) {
	return GmailRawSearchIn(ctx, c, query, "")
}

// GmailRawSearchIn runs query verbatim as X-GM-RAW (Gmail's own search syntax)
// with UID SEARCH in the All Mail folder, opened read-only (EXAMINE). It
// returns the folder name and the matching UIDs, ascending. c must be logged in.
//
// allMail is the folder to search when the caller already knows it (the cache
// records the \All folder). With it, the session needs no LIST, and EXAMINE
// and UID SEARCH are sent back to back and answered in one round trip, which
// matters on an account where every command costs seconds. If EXAMINE refuses
// the hint (a renamed folder), it falls back to LIST. With "" it LISTs first.
func GmailRawSearchIn(ctx context.Context, c *imapclient.Client, query, allMail string) (string, []imap.UID, error) {
	SetPhase(ctx, "caps")
	if !c.Caps().Has(imap.CapGmailExt1) {
		if err := ctx.Err(); err != nil {
			return "", nil, err // the connection was closed under us, not a missing extension
		}
		return "", nil, ErrNoGmailExt
	}
	folder := allMail
	if folder == "" {
		var err error
		if folder, err = listAllMail(ctx, c); err != nil {
			return "", nil, err
		}
	}
	uids, err := examineAndSearch(ctx, c, folder, query)
	if err != nil && allMail != "" && ctx.Err() == nil && errors.Is(err, errExamine) {
		if folder, err = listAllMail(ctx, c); err != nil {
			return "", nil, err
		}
		uids, err = examineAndSearch(ctx, c, folder, query)
	}
	if err != nil {
		return "", nil, err
	}
	return folder, uids, nil
}

func listAllMail(ctx context.Context, c *imapclient.Client) (string, error) {
	SetPhase(ctx, "list")
	folder, err := GmailAllMail(c)
	if err != nil {
		return "", err
	}
	return folder, ctx.Err()
}

var errExamine = errors.New("examine")

// examineAndSearch sends EXAMINE and UID SEARCH X-GM-RAW without waiting in
// between: one round trip for both.
func examineAndSearch(ctx context.Context, c *imapclient.Client, folder, query string) ([]imap.UID, error) {
	SetPhase(ctx, "examine")
	selCmd := c.Select(folder, &imap.SelectOptions{ReadOnly: true})
	searchCmd := c.UIDSearch(&imap.SearchCriteria{GmailRaw: query}, nil)
	if _, err := selCmd.Wait(); err != nil {
		_, _ = searchCmd.Wait() // answered with an error too; drain it
		return nil, fmt.Errorf("examine: %w: %w", errExamine, err)
	}
	SetPhase(ctx, "search")
	data, err := searchCmd.Wait()
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	uids := data.AllUIDs()
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	return uids, nil
}
