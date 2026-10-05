package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"math/big"
	"testing"
	"time"
)

func TestAuditTLSCertificateCanSignUnrelatedDomain(t *testing.T) {
	now := time.Now()
	ca, _, err := SelfSignedCert(t.TempDir(), now)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := x509.ParseCertificate(ca.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(123), DNSNames: []string{"unrelated.example"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, ca.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parent)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "unrelated.example", CurrentTime: now}); err != nil {
		t.Fatal(err)
	}
	t.Log("confirmed trusting app certificate as a root allows its key to authenticate unrelated domains")
}
