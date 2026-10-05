package cache

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/excavador/mail-mcp/internal/accounts"
	"github.com/excavador/mail-mcp/internal/imapx"
)

const (
	testUser = "u"
	testPass = "pw"
)

// cmdLog records the plaintext bytes the CLIENT sent to the server (reads on
// the server side of the connection), so assertions are about commands and
// never about server replies.
type cmdLog struct {
	mu    sync.Mutex
	buf   strings.Builder
	conns []net.Conn
	// hook, when set, sees each chunk the client sent (called with mu held).
	hook func(chunk string)
}

func (l *cmdLog) write(p []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.Write(p)
	if l.hook != nil {
		l.hook(string(p))
	}
}

// killConns closes every accepted connection (call with mu held or from hook).
func (l *cmdLog) killConns() {
	for _, c := range l.conns {
		_ = c.Close()
	}
}

func (l *cmdLog) setHook(h func(string)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hook = h
}

func (l *cmdLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.Reset()
}

func (l *cmdLog) lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, ln := range strings.Split(l.buf.String(), "\r\n") {
		if ln != "" {
			out = append(out, ln)
		}
	}
	return out
}

// verbs returns the IMAP command name of every logged line ("UID FETCH" is
// reported as "FETCH").
func (l *cmdLog) verbs() []string {
	var out []string
	for _, ln := range l.lines() {
		f := strings.Fields(ln)
		if len(f) < 2 {
			continue
		}
		v := strings.ToUpper(f[1])
		if v == "UID" && len(f) > 2 {
			v = strings.ToUpper(f[2])
		}
		out = append(out, v)
	}
	return out
}

type recConn struct {
	net.Conn
	log *cmdLog
}

func (c recConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.log.write(p[:n])
	}
	return n, err
}

type recListener struct {
	net.Listener
	log *cmdLog
	// delay is the one-way server-to-client latency of new connections.
	delay *atomic.Int64
}

func (l recListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.log.mu.Lock()
	l.log.conns = append(l.log.conns, c)
	l.log.mu.Unlock()
	var rc net.Conn = recConn{Conn: c, log: l.log}
	if l.delay != nil {
		rc = maybeDelay(rc, time.Duration(l.delay.Load()))
	}
	return rc, nil
}

func selfSigned(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, hex.EncodeToString(sum[:])
}

type env struct {
	t     *testing.T
	log   *cmdLog
	host  string
	port  int
	pin   string
	acct  accounts.Account
	cache *Cache
	dir   string
	delay *atomic.Int64
}

// setDelay sets the one-way server-to-client latency of connections made from
// now on.
func (e *env) setDelay(d time.Duration) { e.delay.Store(int64(d)) }

// newEnv starts an in-process IMAP server (TLS, self-signed, pinned) with the
// given folders and opens a fresh cache.
func newEnv(t *testing.T, provider accounts.Provider, folders ...string) *env {
	t.Helper()
	cert, pin := selfSigned(t)
	mem := imapmemserver.New()
	user := imapmemserver.NewUser(testUser, testPass)
	mem.AddUser(user)
	for _, f := range folders {
		if err := user.Create(f, nil); err != nil {
			t.Fatal(err)
		}
	}
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
	})
	raw, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	log := &cmdLog{}
	delay := &atomic.Int64{}
	go func() { _ = srv.Serve(recListener{Listener: raw, log: log, delay: delay}) }()
	t.Cleanup(func() { _ = srv.Close() })

	h, p, _ := net.SplitHostPort(raw.Addr().String())
	port, _ := strconv.Atoi(p)
	e := &env{t: t, log: log, host: h, port: port, pin: pin, dir: t.TempDir(), delay: delay}
	e.acct = e.account("acct", provider, testPass)

	c, err := Open(filepath.Join(e.dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	e.cache = c
	return e
}

// account builds an Account through the real loader (the password field is
// unexported), pointing at this env's server.
func (e *env) account(name string, p accounts.Provider, password string) accounts.Account {
	e.t.Helper()
	pw := filepath.Join(e.dir, name+".pw")
	if err := os.WriteFile(pw, []byte(password+"\n"), 0o600); err != nil {
		e.t.Fatal(err)
	}
	cfg := filepath.Join(e.dir, name+".yaml")
	y := fmt.Sprintf("accounts:\n  - name: %s\n    provider: %s\n    host: %s\n    port: %d\n    tls: implicit\n    username: %s\n    passwordFile: %s\n    pinnedCertSHA256: %s\n",
		name, p, e.host, e.port, testUser, pw, e.pin)
	if err := os.WriteFile(cfg, []byte(y), 0o600); err != nil {
		e.t.Fatal(err)
	}
	as, err := accounts.Load(cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	return as[0]
}

func (e *env) ctx() context.Context {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	e.t.Cleanup(cancel)
	return ctx
}

// admin runs f on a short-lived authenticated connection.
func (e *env) admin(f func(c *imapclient.Client)) {
	e.t.Helper()
	c, err := imapx.Dial(e.ctx(), e.acct)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	f(c)
}

func (e *env) appendMsg(folder string, raw []byte, date time.Time) {
	e.t.Helper()
	e.admin(func(c *imapclient.Client) {
		cmd := c.Append(folder, int64(len(raw)), &imap.AppendOptions{Time: date})
		if _, err := cmd.Write(raw); err != nil {
			e.t.Fatal(err)
		}
		if err := cmd.Close(); err != nil {
			e.t.Fatal(err)
		}
		if _, err := cmd.Wait(); err != nil {
			e.t.Fatal(err)
		}
	})
}

// expungeAll deletes every message in folder.
func (e *env) expungeAll(folder string) {
	e.t.Helper()
	e.admin(func(c *imapclient.Client) {
		if _, err := c.Select(folder, nil).Wait(); err != nil {
			e.t.Fatal(err)
		}
		var all imap.UIDSet
		all.AddRange(1, 0)
		if err := c.Store(all, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); err != nil {
			e.t.Fatal(err)
		}
		if err := c.Expunge().Close(); err != nil {
			e.t.Fatal(err)
		}
	})
}

func (e *env) expungeUID(folder string, uid imap.UID) {
	e.t.Helper()
	e.admin(func(c *imapclient.Client) {
		if _, err := c.Select(folder, nil).Wait(); err != nil {
			e.t.Fatal(err)
		}
		if err := c.Store(imap.UIDSetNum(uid), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); err != nil {
			e.t.Fatal(err)
		}
		if err := c.Expunge().Close(); err != nil {
			e.t.Fatal(err)
		}
	})
}

func (e *env) deleteFolder(name string) {
	e.t.Helper()
	e.admin(func(c *imapclient.Client) {
		if err := c.Delete(name).Wait(); err != nil {
			e.t.Fatal(err)
		}
	})
}

func (e *env) createFolder(name string) {
	e.t.Helper()
	e.admin(func(c *imapclient.Client) {
		if err := c.Create(name, nil).Wait(); err != nil {
			e.t.Fatal(err)
		}
	})
}

// refresh dials and runs one Refresh on the env's cache.
func (e *env) refresh() Stats {
	e.t.Helper()
	c, err := imapx.Dial(e.ctx(), e.acct)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	st, err := e.cache.Refresh(e.ctx(), e.acct, c)
	if err != nil {
		e.t.Fatalf("Refresh: %v", err)
	}
	return st
}

func (e *env) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.cache.db.QueryRow(q, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
	return n
}

// mkMsg builds a CRLF RFC 822 message with a unique Message-ID.
func mkMsg(id, subject, body string, extra ...string) []byte {
	h := []string{
		"From: Alice <alice@example.com>",
		"To: Bob <bob@example.com>",
		"Subject: " + subject,
		"Date: Mon, 02 Jan 2006 15:04:05 +0000",
		"Message-Id: <" + id + "@test>",
	}
	h = append(h, extra...)
	return []byte(strings.Join(h, "\r\n") + "\r\n\r\n" + body + "\r\n")
}

var t0 = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

func imapxDial(e *env) (*imapclient.Client, error) { return imapx.Dial(e.ctx(), e.acct) }
