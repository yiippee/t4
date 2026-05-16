package e2e_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ca is the in-memory authority used to sign leaf certs. Held by tests that
// need to mint additional certs (e.g. a "wrong CA" or a client cert) at
// runtime without re-deriving the bundle from PEM files.
type ca struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	dir  string // temp directory for any files written via writePEM*
}

// newCA generates a self-signed CA and writes its cert + key to dir.
func newCA(t *testing.T, dir string, cn string) *ca {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ca key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          bigSerial(),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("sign ca: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}
	return &ca{cert: cert, key: key, dir: dir}
}

// writeCACert writes the CA cert PEM to disk and returns the path.
func (c *ca) writeCACert(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(c.dir, name)
	writePEMCert(t, path, c.cert.Raw)
	return path
}

// mintServer issues a server cert with the requested SANs (DNS names and/or
// IPs). Includes both server and client EKUs so the same cert can be used
// for peer mTLS (where each node is both server and client).
func (c *ca) mintServer(t *testing.T, cn string, dnsSANs []string, ipSANs []net.IP) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate server key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: bigSerial(),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     dnsSANs,
		IPAddresses:  ipSANs,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		t.Fatalf("sign server cert: %v", err)
	}
	base := filepath.Join(c.dir, cn)
	certPath = base + ".crt"
	keyPath = base + ".key"
	writePEMCert(t, certPath, der)
	writePEMKey(t, keyPath, key)
	return certPath, keyPath
}

// mintClient issues a client cert (clientAuth EKU only) for mTLS clients.
func (c *ca) mintClient(t *testing.T, cn string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: bigSerial(),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		t.Fatalf("sign client cert: %v", err)
	}
	base := filepath.Join(c.dir, "client-"+cn)
	certPath = base + ".crt"
	keyPath = base + ".key"
	writePEMCert(t, certPath, der)
	writePEMKey(t, keyPath, key)
	return certPath, keyPath
}

func writePEMCert(t *testing.T, path string, der []byte) {
	t.Helper()
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func writePEMKey(t *testing.T, path string, key *ecdsa.PrivateKey) {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal ec key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func bigSerial() *big.Int {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		panic(fmt.Sprintf("generate serial: %v", err))
	}
	return n
}
