package server

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Headers and the first byte of a body, then silence. The server must not
// hold the connection and its goroutine for as long as the client likes.
func TestAttackRequestBodyThatNeverArrives(t *testing.T) {
	old := bodyTimeout
	bodyTimeout = 300 * time.Millisecond
	t.Cleanup(func() { bodyTimeout = old })
	h := newHarness(t, Options{}, false)
	u, err := url.Parse(h.url)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST /api/login HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: 1024\r\n\r\n{", u.Host)
	start := time.Now()
	conn.SetReadDeadline(start.Add(3 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer within 3 s: %v", err)
	}
	if resp.StatusCode != http.StatusRequestTimeout {
		t.Errorf("status %d, want %d", resp.StatusCode, http.StatusRequestTimeout)
	}
	if since := time.Since(start); since > 2*time.Second {
		t.Errorf("the answer took %v", since)
	}
}

// The server's certificate vouches for this server only. Trusting it on a
// device must not let whoever holds its key vouch for other sites.
func TestAttackCertificateCannotVouchForOtherSites(t *testing.T) {
	now := time.Now()
	cert, _, err := SelfSignedCert(t.TempDir(), now)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if parent.IsCA || parent.KeyUsage&x509.KeyUsageCertSign != 0 {
		t.Errorf("the certificate is an authority (IsCA %v, key usage %b)", parent.IsCA, parent.KeyUsage)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(123), DNSNames: []string{"unrelated.example"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, cert.PrivateKey)
	if err != nil {
		return // it cannot even sign one
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parent)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "unrelated.example", CurrentTime: now}); err == nil {
		t.Error("a certificate for another site, signed with this server's key, was accepted by a device trusting this server")
	}
}

// A certificate made by an earlier version, marked as an authority, is
// replaced the next time the program starts.
func TestAnOldAuthorityCertificateIsReplaced(t *testing.T) {
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Reflecting Pool"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(certLifetime),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, IsCA: true, BasicConstraintsValid: true, DNSNames: []string{"localhost"}}
	der, err := x509.CreateCertificate(rand.Reader, old, old, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	os.WriteFile(filepath.Join(dir, "cert.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	os.WriteFile(filepath.Join(dir, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)

	cert, print, err := SelfSignedCert(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if leaf.IsCA || print == fingerprint(der) {
		t.Errorf("the old authority certificate was kept (IsCA %v)", leaf.IsCA)
	}
	// And the new one is kept on the next start.
	_, again, err := SelfSignedCert(dir, now.Add(time.Hour))
	if err != nil || again != print {
		t.Errorf("second start: %v, fingerprint changed %v", err, again != print)
	}
}
