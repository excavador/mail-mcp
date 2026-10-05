package server

// A small scripted IMAP server that speaks just enough of Gmail's dialect
// (X-GM-EXT-1) for the Gmail paths, which imapmemserver cannot serve. It is
// TLS with a self-signed certificate the account pins, like the other
// harnesses. This file is identical in internal/cache and internal/server
// except for the package clause; keep the two in step.

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/excavador/mail-mcp/internal/accounts"
)

const gfCapsExt = "IMAP4rev1 UIDPLUS X-GM-EXT-1"
const gfCapsPlain = "IMAP4rev1 UIDPLUS"

type gfMsg struct {
	UID   uint32
	MsgID uint64 // X-GM-MSGID, 0 is sent as 0
	ThrID uint64
	Raw   []byte
	Date  time.Time
	NoGM  bool // do not send X-GM-* at all even when asked
}

type gfFolder struct {
	Name     string
	Validity uint32
	All      bool // LIST marks it \All
	Msgs     []gfMsg
}

type gfConfig struct {
	Caps    string // CAPABILITY text; default gfCapsExt
	Folders []gfFolder
	Search  []uint32 // UIDs answered to UID SEARCH
	// Hold, if non-nil, blocks every UID SEARCH until closed (slot-busy tests).
	Hold chan struct{}
	// Entered is closed (once) when the first UID SEARCH arrives.
	Entered chan struct{}
}

type gfArrival struct {
	at time.Time
	s  string
}

type gfLog struct {
	mu       sync.Mutex
	buf      strings.Builder
	arrivals []gfArrival
}

func (l *gfLog) write(p []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.Write(p)
	l.arrivals = append(l.arrivals, gfArrival{at: time.Now(), s: string(p)})
}

// firstArrival is when the first chunk containing sub reached the server
// after the first n bytes, and false if none has.
func (l *gfLog) firstArrival(sub string, after int) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	off := 0
	for _, a := range l.arrivals {
		if off >= after && strings.Contains(a.s, sub) {
			return a.at, true
		}
		off += len(a.s)
	}
	return time.Time{}, false
}

// raw is every byte the client sent.
func (l *gfLog) raw() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// verbs is the command name of each client line that starts with a tag
// ("UID FETCH" reports as FETCH). Literal payload lines are skipped by tag
// shape: tags are "T<digits>" in go-imap.
var gfTag = regexp.MustCompile(`^[A-Za-z]+\d+ `)

func (l *gfLog) verbs() []string {
	var out []string
	for _, ln := range strings.Split(l.raw(), "\r\n") {
		if !gfTag.MatchString(ln) {
			continue
		}
		f := strings.Fields(ln)
		v := strings.ToUpper(f[1])
		if v == "UID" && len(f) > 2 {
			v = strings.ToUpper(f[2])
		}
		out = append(out, v)
	}
	return out
}

type gfConn struct {
	net.Conn
	log *gfLog
}

func (c gfConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.log.write(p[:n])
	}
	return n, err
}

type gmailFake struct {
	t    *testing.T
	cfg  *gfConfig
	log  *gfLog
	host string
	port int
	pin  string
	ln   net.Listener
	once sync.Once
	// delay is the one-way server-to-client latency of new connections.
	delay atomic.Int64
}

func (f *gmailFake) setDelay(d time.Duration) { f.delay.Store(int64(d)) }

func startGmailFake(t *testing.T, cfg gfConfig) *gmailFake {
	t.Helper()
	if cfg.Caps == "" {
		cfg.Caps = gfCapsExt
	}
	cert, pin := selfSigned(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(p)
	f := &gmailFake{t: t, cfg: &cfg, log: &gfLog{}, host: h, port: port, pin: pin, ln: ln}
	var wg sync.WaitGroup
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = c.SetDeadline(time.Now().Add(60 * time.Second)) // bound every wait
				dc := maybeDelay(c, time.Duration(f.delay.Load()))
				defer dc.Close() // delivers queued answers, then closes c
				f.serve(gfConn{Conn: dc, log: f.log})
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		if cfg.Hold != nil {
			f.release()
		}
	})
	return f
}

func (f *gmailFake) release() { f.once.Do(func() { close(f.cfg.Hold) }) }

// account builds an Account through the real loader, pointing at the fake.
func (f *gmailFake) account(t *testing.T, dir, name string, p accounts.Provider) accounts.Account {
	t.Helper()
	pw := filepath.Join(dir, name+".pw")
	if err := os.WriteFile(pw, []byte("pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, name+".yaml")
	y := fmt.Sprintf("accounts:\n  - name: %s\n    provider: %s\n    host: %s\n    port: %d\n    tls: implicit\n    username: u\n    passwordFile: %s\n    pinnedCertSHA256: %s\n",
		name, p, f.host, f.port, pw, f.pin)
	if err := os.WriteFile(cfg, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	as, err := accounts.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return as[0]
}

var (
	gfPeek = regexp.MustCompile(`BODY\.PEEK\[([^\]]*)\]`)
	gfLit  = regexp.MustCompile(`\{(\d+)\+?\}$`)
)

// readCommand returns one logical command with its literals inlined.
func gfReadCommand(br *bufio.Reader, w io.Writer) (string, error) {
	var sb strings.Builder
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return "", err
		}
		line = strings.TrimRight(line, "\r\n")
		m := gfLit.FindStringSubmatch(line)
		if m == nil {
			sb.WriteString(line)
			return sb.String(), nil
		}
		n, _ := strconv.Atoi(m[1])
		sb.WriteString(strings.TrimSuffix(line, m[0]))
		if !strings.HasSuffix(m[0], "+}") {
			_, _ = io.WriteString(w, "+ ready\r\n")
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(br, buf); err != nil {
			return "", err
		}
		sb.Write(buf)
	}
}

func (f *gmailFake) folder(name string) *gfFolder {
	for i := range f.cfg.Folders {
		if f.cfg.Folders[i].Name == name {
			return &f.cfg.Folders[i]
		}
	}
	return nil
}

func (f *gmailFake) serve(c net.Conn) {
	br := bufio.NewReader(c)
	say := func(s string) { _, _ = io.WriteString(c, s+"\r\n") }
	say("* OK [CAPABILITY " + f.cfg.Caps + "] fake ready")
	var cur *gfFolder
	for {
		cmd, err := gfReadCommand(br, c)
		if err != nil {
			return
		}
		sp := strings.SplitN(cmd, " ", 3)
		if len(sp) < 2 {
			say("* BAD empty")
			continue
		}
		tag, verb := sp[0], strings.ToUpper(sp[1])
		rest := ""
		if len(sp) > 2 {
			rest = sp[2]
		}
		switch verb {
		case "CAPABILITY":
			say("* CAPABILITY " + f.cfg.Caps)
			say(tag + " OK done")
		case "LOGIN":
			say(tag + " OK [CAPABILITY " + f.cfg.Caps + "] logged in")
		case "NOOP":
			say(tag + " OK done")
		case "LIST":
			for _, fo := range f.cfg.Folders {
				attrs := `\HasNoChildren`
				if fo.All {
					attrs += ` \All`
				}
				say(fmt.Sprintf(`* LIST (%s) "/" "%s"`, attrs, fo.Name))
			}
			say(tag + " OK done")
		case "STATUS":
			name := gfMailbox(rest)
			fo := f.folder(name)
			if fo == nil {
				say(tag + " NO no such mailbox")
				continue
			}
			say(fmt.Sprintf(`* STATUS "%s" (MESSAGES %d)`, name, len(fo.Msgs)))
			say(tag + " OK done")
		case "EXAMINE", "SELECT":
			name := gfMailbox(rest)
			fo := f.folder(name)
			if fo == nil {
				say(tag + " NO no such mailbox")
				continue
			}
			cur = fo
			say(fmt.Sprintf("* %d EXISTS", len(fo.Msgs)))
			say("* 0 RECENT")
			say(fmt.Sprintf("* OK [UIDVALIDITY %d] ok", fo.Validity))
			say("* OK [UIDNEXT 1000] ok")
			if verb == "EXAMINE" {
				say(tag + " OK [READ-ONLY] done")
			} else {
				say(tag + " OK [READ-WRITE] done")
			}
		case "UID":
			sub := strings.SplitN(rest, " ", 2)
			arg := ""
			if len(sub) > 1 {
				arg = sub[1]
			}
			switch strings.ToUpper(sub[0]) {
			case "FETCH":
				if cur == nil {
					say(tag + " BAD no mailbox")
					continue
				}
				f.fetch(c, cur, arg)
				say(tag + " OK done")
			case "SEARCH":
				if f.cfg.Entered != nil {
					select {
					case <-f.cfg.Entered:
					default:
						close(f.cfg.Entered)
					}
				}
				if f.cfg.Hold != nil {
					select {
					case <-f.cfg.Hold:
					case <-time.After(30 * time.Second):
					}
				}
				var ids []string
				for _, u := range f.cfg.Search {
					ids = append(ids, strconv.FormatUint(uint64(u), 10))
				}
				if len(ids) == 0 {
					say("* SEARCH")
				} else {
					say("* SEARCH " + strings.Join(ids, " "))
				}
				say(tag + " OK done")
			default:
				say(tag + " BAD unsupported")
			}
		case "LOGOUT":
			say("* BYE bye")
			say(tag + " OK done")
			return
		default:
			say(tag + " BAD unsupported")
		}
	}
}

// fetch answers UID FETCH <set> (<attrs>) for the selected folder.
func (f *gmailFake) fetch(c net.Conn, fo *gfFolder, arg string) {
	set, attrs, _ := strings.Cut(arg, " ")
	want := func(a string) bool { return strings.Contains(strings.ToUpper(attrs), a) }
	for i, m := range fo.Msgs {
		if !gfInSet(set, m.UID) {
			continue
		}
		parts := []string{fmt.Sprintf("UID %d", m.UID)}
		if want("RFC822.SIZE") {
			parts = append(parts, fmt.Sprintf("RFC822.SIZE %d", len(m.Raw)))
		}
		if want("INTERNALDATE") {
			parts = append(parts, `INTERNALDATE "`+m.Date.UTC().Format("02-Jan-2006 15:04:05 -0700")+`"`)
		}
		if !m.NoGM && want("X-GM-MSGID") {
			parts = append(parts, fmt.Sprintf("X-GM-MSGID %d", m.MsgID))
		}
		if !m.NoGM && want("X-GM-THRID") {
			parts = append(parts, fmt.Sprintf("X-GM-THRID %d", m.ThrID))
		}
		var lits []string // section, payload
		for _, sm := range gfPeek.FindAllStringSubmatch(attrs, -1) {
			sec := sm[1]
			payload := m.Raw
			if strings.HasPrefix(strings.ToUpper(sec), "HEADER") {
				hdr, _, _ := strings.Cut(string(m.Raw), "\r\n\r\n")
				payload = []byte(hdr + "\r\n\r\n")
			}
			lits = append(lits, sec, string(payload))
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "* %d FETCH (%s", i+1, strings.Join(parts, " "))
		for j := 0; j < len(lits); j += 2 {
			fmt.Fprintf(&sb, " BODY[%s] {%d}\r\n%s", lits[j], len(lits[j+1]), lits[j+1])
		}
		sb.WriteString(")\r\n")
		_, _ = io.WriteString(c, sb.String())
	}
}

func gfInSet(set string, uid uint32) bool {
	for _, part := range strings.Split(set, ",") {
		lo, hi, isRange := strings.Cut(part, ":")
		a := gfNum(lo, uid)
		b := a
		if isRange {
			b = gfNum(hi, uid)
		}
		if a > b {
			a, b = b, a
		}
		if uid >= a && uid <= b {
			return true
		}
	}
	return false
}

func gfNum(s string, star uint32) uint32 {
	if s == "*" {
		return ^uint32(0)
	}
	n, _ := strconv.ParseUint(s, 10, 32)
	return uint32(n)
}

// gfMail builds a message with a unique Message-Id.
func gfMail(id, subject string) []byte {
	return gfMailAt(id, subject, time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC))
}

// gfMailAt is gfMail with a chosen Date header.
func gfMailAt(id, subject string, date time.Time) []byte {
	return []byte(strings.Join([]string{
		"From: Alice <alice@example.com>",
		"To: Bob <bob@example.com>",
		"Subject: " + subject,
		"Date: " + date.Format(time.RFC1123Z),
		"Message-Id: <" + id + "@test>",
	}, "\r\n") + "\r\n\r\nbody of " + subject + "\r\n")
}

// gfMailbox reads the first astring (quoted or atom) of a command's arguments.
func gfMailbox(rest string) string {
	if strings.HasPrefix(rest, `"`) {
		if i := strings.Index(rest[1:], `"`); i >= 0 {
			return rest[1 : i+1]
		}
	}
	name, _, _ := strings.Cut(rest, " ")
	return name
}
