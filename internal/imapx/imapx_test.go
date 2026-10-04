package imapx

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"math/big"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/excavador/mail-mcp/internal/accounts"
)

// selfSigned returns a TLS certificate for 127.0.0.1 and the SHA-256 of its DER.
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

// serve starts a loopback TLS listener that completes handshakes and returns
// its address. The handshake is forced server-side so client failures surface.
func serve(t *testing.T, cert tls.Certificate) (host string, port int) {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.(*tls.Conn).Handshake()
				buf := make([]byte, 1)
				_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
				_, _ = c.Read(buf)
			}()
		}
	}()
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	pn, _ := strconv.Atoi(p)
	return h, pn
}

func dial(t *testing.T, a accounts.Account) error {
	t.Helper()
	d := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := tls.DialWithDialer(d, "tcp", a.Addr(), tlsConfig(a))
	if err == nil {
		_ = conn.Close()
	}
	return err
}

func account(host string, port int, pin string) accounts.Account {
	return accounts.Account{
		Name: "t", Provider: accounts.Proton, Host: host, Port: port,
		TLS: accounts.Implicit, Username: "u", PasswordFile: "unused",
		PinnedCertSHA256: pin,
	}
}

func TestPinAcceptsMatchingCertificate(t *testing.T) {
	cert, pin := selfSigned(t)
	host, port := serve(t, cert)
	if err := dial(t, account(host, port, pin)); err != nil {
		t.Fatalf("handshake with correct pin failed: %v", err)
	}
}

func TestPinRefusesWrongCertificate(t *testing.T) {
	cert, _ := selfSigned(t)
	host, port := serve(t, cert)
	wrong := strings.Repeat("ab", 32)
	err := dial(t, account(host, port, wrong))
	if err == nil {
		t.Fatal("handshake with wrong pin must fail")
	}
	if !strings.Contains(err.Error(), "pin") {
		t.Errorf("error should mention the pin, got: %v", err)
	}
}

func TestPinRefusesDifferentCertWithValidOtherPin(t *testing.T) {
	// Pin belongs to one self-signed cert; the server presents another.
	_, pinA := selfSigned(t)
	certB, _ := selfSigned(t)
	host, port := serve(t, certB)
	err := dial(t, account(host, port, pinA))
	if err == nil || !strings.Contains(err.Error(), "pin") {
		t.Fatalf("want pin mismatch, got %v", err)
	}
}

func TestNoPinRefusesSelfSigned(t *testing.T) {
	cert, _ := selfSigned(t)
	host, port := serve(t, cert)
	if err := dial(t, account(host, port, "")); err == nil {
		t.Fatal("self-signed certificate without a pin must be refused by normal verification")
	}
}

func TestNoPinKeepsVerificationOn(t *testing.T) {
	cfg := tlsConfig(account("imap.example.com", 993, ""))
	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify must be false without a pin")
	}
	if cfg.VerifyPeerCertificate != nil {
		t.Error("no custom verifier expected without a pin")
	}
	if cfg.ServerName != "imap.example.com" {
		t.Errorf("ServerName = %q, want the account host", cfg.ServerName)
	}
	if cfg.MinVersion < tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want at least TLS 1.2", cfg.MinVersion)
	}
}

func TestPinnedVerifierRefusesEmptyChain(t *testing.T) {
	cfg := tlsConfig(account("h", 1, strings.Repeat("ab", 32)))
	if cfg.VerifyPeerCertificate == nil {
		t.Fatal("pinned config must carry a verifier")
	}
	if err := cfg.VerifyPeerCertificate(nil, nil); err == nil {
		t.Error("empty certificate list must be refused")
	}
}
