package calendar

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// Login runs the one-time consent flow for `mail-mcp calendar-login`: a
// loopback OAuth flow (127.0.0.1, random port) with PKCE, offline access and
// a forced consent screen, so Google returns a refresh token. The token is
// written to --out (0600) and never printed; only "saved" is.
func Login(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("calendar-login", flag.ContinueOnError)
	fs.SetOutput(stderr)
	idFile := fs.String("client-id-file", "", "file holding the OAuth client id")
	secretFile := fs.String("client-secret-file", "", "file holding the OAuth client secret")
	out := fs.String("out", "", "file to write the refresh token to (mode 0600)")
	write := fs.Bool("write", false, "also ask for calendar.events.owned (needed for preview_event / create_event); default: calendar.readonly only")
	timeout := fs.Duration("timeout", 5*time.Minute, "how long to wait for the browser")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *idFile == "" || *secretFile == "" || *out == "" {
		return errors.New("--client-id-file, --client-secret-file and --out are required")
	}
	id, err := readTrim(*idFile)
	if err != nil {
		return fmt.Errorf("--client-id-file: %w", err)
	}
	secret, err := readTrim(*secretFile)
	if err != nil {
		return fmt.Errorf("--client-secret-file: %w", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("cannot listen on loopback: %w", err)
	}
	defer ln.Close()
	scopes := []string{ScopeReadonly}
	if *write {
		scopes = append(scopes, ScopeEvents)
	}
	conf := &oauth2.Config{
		ClientID: id, ClientSecret: secret, Endpoint: google.Endpoint, Scopes: scopes,
		RedirectURL: "http://" + ln.Addr().String() + "/",
	}
	return login(ctx, conf, ln, *out, *timeout, stderr, stdout)
}

func readTrim(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("cannot be read")
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", errors.New("is empty")
	}
	return v, nil
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

type callback struct{ code, errCode string }

func login(ctx context.Context, conf *oauth2.Config, ln net.Listener, outPath string, timeout time.Duration, stderr, stdout io.Writer) error {
	state, err := randHex(16)
	if err != nil {
		return err
	}
	verifier := oauth2.GenerateVerifier()
	authURL := conf.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("prompt", "consent"))

	got := make(chan callback, 1)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") == "" && q.Get("code") == "" && q.Get("error") == "" {
			http.NotFound(w, r) // favicon and the like
			return
		}
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			return
		}
		select {
		case got <- callback{code: q.Get("code"), errCode: q.Get("error")}:
		default:
		}
		fmt.Fprintln(w, "mail-mcp: you can close this tab and return to the terminal.")
	})}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	fmt.Fprintf(stderr, "Open this URL in a browser signed in to the Google account, and approve:\n\n%s\n\n", authURL)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var cb callback
	select {
	case cb = <-got:
	case <-ctx.Done():
		return errors.New("timed out waiting for the browser")
	}
	if cb.errCode != "" {
		return fmt.Errorf("Google refused: %s", sanitizeCode(cb.errCode))
	}
	if cb.code == "" {
		return errors.New("no authorization code in the callback")
	}
	tok, err := conf.Exchange(ctx, cb.code, oauth2.VerifierOption(verifier))
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.ErrorCode != "" {
			return fmt.Errorf("token exchange failed: %s", sanitizeCode(re.ErrorCode))
		}
		return errors.New("token exchange failed")
	}
	granted := strings.Fields(fmt.Sprint(tok.Extra("scope")))
	for _, want := range conf.Scopes {
		found := false
		for _, g := range granted {
			found = found || g == want
		}
		if !found {
			return fmt.Errorf("Google did not grant the scope %s (granted: %s); nothing was saved. Approve every requested permission and run again", want, sanitizeScopes(granted))
		}
	}
	if tok.RefreshToken == "" {
		return errors.New("Google returned no refresh token; remove this app's access at myaccount.google.com/permissions and run again")
	}
	if err := writeTokenFile(outPath, tok.RefreshToken+"\n"); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "saved")
	return nil
}

func sanitizeScopes(g []string) string {
	if len(g) == 0 {
		return "none reported"
	}
	return sanitizeURLs(strings.Join(g, " "))
}

func sanitizeURLs(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r > ' ' && r < 0x7f {
			b.WriteRune(r)
		} else if r == ' ' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// writeTokenFile replaces outPath atomically: the token goes to an O_EXCL
// 0600 temporary file beside it, is fsynced, and is renamed over outPath.
// A symlink at outPath is refused, so the token cannot be steered elsewhere.
func writeTokenFile(outPath, content string) error {
	if fi, err := os.Lstat(outPath); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return errors.New("--out is a symbolic link; refusing to write through it")
	}
	dir := filepath.Dir(outPath)
	rnd, err := randHex(8)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+filepath.Base(outPath)+".tmp-"+rnd)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("cannot create a temporary file beside --out")
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return errors.New("cannot set the file to mode 0600")
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return errors.New("cannot write --out")
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return errors.New("cannot write --out")
	}
	if err := f.Close(); err != nil {
		return errors.New("cannot write --out")
	}
	if err := os.Rename(tmp, outPath); err != nil {
		return errors.New("cannot replace --out")
	}
	ok = true
	return nil
}

// sanitizeCode keeps an OAuth error code (invalid_grant, access_denied)
// readable and nothing else.
func sanitizeCode(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	if b.Len() > 64 {
		return b.String()[:64]
	}
	return b.String()
}
