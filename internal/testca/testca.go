// Package testca issues throwaway certificate chains for tests. It mirrors what
// scripts/certgen.sh produces, but in memory, so tests never depend on ../certs
// having been generated. Only _test.go files import it, which is why depending
// on "testing" here is harmless.
package testca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// CA is a local certificate authority that signs leaf certificates carrying
// explicit SANs, so tests exercise real chain building and hostname
// verification rather than httptest's self-trusting shortcut.
type CA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
	Pool *x509.CertPool
}

func New(t *testing.T) *CA {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber:          serial(t),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(cert)

	return &CA{Cert: cert, Key: key, Pool: pool}
}

// Leaf issues a server certificate valid for the given SANs.
func (ca *CA) Leaf(t *testing.T, commonName string, dnsNames []string, ips []net.IP) tls.Certificate {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber:          serial(t),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           ips,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		t.Fatalf("sign leaf cert: %v", err)
	}

	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// WriteCertFile writes the CA certificate as PEM and returns its path. The
// proxy is configured with file paths, so its tests need one on disk.
func (ca *CA) WriteCertFile(t *testing.T, dir string) string {
	t.Helper()

	path := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Cert.Raw}), 0o644); err != nil {
		t.Fatalf("write CA cert: %v", err)
	}
	return path
}

// WriteLeafFiles writes a leaf certificate and its key as PEM, returning both
// paths, for code that loads a keypair from disk.
func (ca *CA) WriteLeafFiles(t *testing.T, dir, name string, dnsNames []string, ips []net.IP) (certFile, keyFile string) {
	t.Helper()

	pair := ca.Leaf(t, name, dnsNames, ips)

	keyDER, err := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}

	certFile = filepath.Join(dir, name+".crt")
	keyFile = filepath.Join(dir, name+".key")

	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.Certificate[0]}), 0o644); err != nil {
		t.Fatalf("write leaf cert: %v", err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write leaf key: %v", err)
	}
	return certFile, keyFile
}

func serial(t *testing.T) *big.Int {
	t.Helper()

	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	return n
}
