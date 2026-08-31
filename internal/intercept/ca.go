// Package intercept lets daiyaku answer the harness at the vendor's own address
// instead of asking the harness to point somewhere else. The harness is given
// nothing: it resolves api.anthropic.com to this machine, is handed a
// certificate for that name signed by a CA the machine trusts, and behaves
// exactly as it would in production. That matters because a harness can and does
// change behaviour when it notices a non-standard base URL.
package intercept

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CACommonName identifies daiyaku's CA in a trust store, so --revert can find
// and remove exactly the certificate this tool installed.
const CACommonName = "daiyaku local test CA"

const (
	caValidity   = 2 * 365 * 24 * time.Hour
	leafValidity = 90 * 24 * time.Hour
	// renewBefore keeps a CA from expiring mid-engagement: one that is close to
	// the end of its life is replaced on the next run rather than failing later.
	renewBefore = 30 * 24 * time.Hour
)

// CA is daiyaku's signing authority plus the leaf certificates minted from it.
// The key never leaves this machine and is written owner-only: anything holding
// it can impersonate any site to a machine that trusts the CA.
type CA struct {
	Cert     *x509.Certificate
	CertPEM  []byte
	CertPath string
	key      *ecdsa.PrivateKey

	mu    sync.Mutex
	leafs map[string]*tls.Certificate
}

// LoadOrCreateCA returns the CA stored in dir, generating one if it is missing,
// unreadable, or near expiry. Reusing it across runs is what keeps the operator
// from having to trust a new certificate every session.
func LoadOrCreateCA(dir string) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")

	if ca, err := loadCA(certPath, keyPath); err == nil {
		if time.Until(ca.Cert.NotAfter) > renewBefore {
			return ca, nil
		}
	}
	return createCA(certPath, keyPath)
}

func loadCA(certPath, keyPath string) (*CA, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	certBlock, _ := pem.Decode(certPEM)
	keyBlock, _ := pem.Decode(keyPEM)
	if certBlock == nil || keyBlock == nil {
		return nil, fmt.Errorf("ca files are not valid PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, CertPEM: certPEM, CertPath: certPath, key: key,
		leafs: map[string]*tls.Certificate{}}, nil
}

func createCA(certPath, keyPath string) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   CACommonName,
			Organization: []string{"daiyaku harness testing"},
		},
		NotBefore:             now.Add(-time.Hour), // tolerate small clock skew
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	// The certificate is public; the key is not. 0600 on the key is the only
	// thing standing between this file and impersonating any site to a machine
	// that trusts the CA.
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, err
	}
	return &CA{Cert: cert, CertPEM: certPEM, CertPath: certPath, key: key,
		leafs: map[string]*tls.Certificate{}}, nil
}

// TLSConfig mints a certificate per requested server name on demand, so one CA
// covers whichever vendor hostnames the run intercepts.
func (ca *CA) TLSConfig(fallbackHost string) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			name := hello.ServerName
			if name == "" {
				// A client that connected by IP sends no SNI. Serve the host this
				// run is intercepting so the handshake still completes.
				name = fallbackHost
			}
			return ca.leafFor(name)
		},
	}
}

func (ca *CA) leafFor(host string) (*tls.Certificate, error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if c, ok := ca.leafs[host]; ok {
		return c, nil
	}
	c, err := ca.mintLeaf(host)
	if err != nil {
		return nil, err
	}
	ca.leafs[host] = c
	return c, nil
}

func (ca *CA) mintLeaf(host string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		// Modern clients ignore CommonName entirely and require a SAN.
		DNSNames:    []string{host},
		NotBefore:   now.Add(-time.Hour),
		NotAfter:    now.Add(leafValidity),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{
		Certificate: [][]byte{der, ca.Cert.Raw},
		PrivateKey:  key,
		Leaf:        tmpl,
	}, nil
}

// Pool is a root pool trusting only this CA, used by the self-test to prove the
// chain the harness will be offered actually verifies.
func (ca *CA) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	return pool
}

func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}
