// Package tlslegacy runs an HTTPS listener that iPhone OS 2/3 can talk to:
// TLS 1.0 with RSA key exchange and AES-CBC suites, and a certificate from a
// bridge-specific CA that users install on the device.
//
// This listener is deliberately weak. It exists only because the clients
// cannot do better, is off by default, runs as its own http.Server with its
// own tls.Config, and serves only the public API handler (never admin or
// metrics).
package tlslegacy

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Options configure certificate issuance.
type Options struct {
	Dir             string   // where CA and leaf live (inside the data volume)
	CommonName      string   // leaf CN; old clients check CN rather than SANs
	Hosts           []string // SANs for the leaf
	NameConstraints bool     // limit the CA to Hosts (non-critical, so clients that don't understand it still work)
	SHA1            bool     // sign with SHA-1 for very old verifiers
}

// Material is the loaded CA and leaf.
type Material struct {
	CA    *x509.Certificate
	CADER []byte
	Leaf  tls.Certificate
	// Warnings lists hostnames the leaf carries but the CA's name
	// constraints do not permit (hosts added after the CA was created).
	// Verifiers that enforce constraints reject the certificate for them.
	Warnings []string
	caKey    *rsa.PrivateKey
}

// Suites enabled on the legacy listener. iPhone OS 3 offers the RSA key
// exchange AES and 3DES suites (and RC4, which is not enabled here).
var Suites = []uint16{
	tls.TLS_RSA_WITH_AES_128_CBC_SHA,
	tls.TLS_RSA_WITH_AES_256_CBC_SHA,
	tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
	tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
	tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA,
}

// Config returns the listener's TLS configuration: TLS 1.0 to 1.2, the
// legacy suite list, and the same certificate for every SNI name (iPhone OS
// 3 does not send SNI).
func (m *Material) Config() *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS10,
		MaxVersion:   tls.VersionTLS12,
		CipherSuites: Suites,
		Certificates: []tls.Certificate{m.Leaf},
		NextProtos:   []string{"http/1.1"},
	}
}

// Load reads existing material from opts.Dir, creating a CA and leaf on
// first use and reissuing the leaf when hosts change or it nears expiry.
func Load(opts Options) (*Material, error) {
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, err
	}
	hosts := normalizeHosts(opts.Hosts, opts.CommonName)
	if len(hosts) == 0 {
		return nil, errors.New("tlslegacy: no hostnames configured")
	}
	caCertPath, caKeyPath := filepath.Join(opts.Dir, "ca.crt"), filepath.Join(opts.Dir, "ca.key")
	leafCertPath, leafKeyPath := filepath.Join(opts.Dir, "leaf.crt"), filepath.Join(opts.Dir, "leaf.key")

	ca, caKey, err := loadPair(caCertPath, caKeyPath)
	if errors.Is(err, os.ErrNotExist) {
		ca, caKey, err = newCA(opts, hosts)
		if err == nil {
			err = savePair(caCertPath, caKeyPath, ca, caKey)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("tlslegacy: CA: %w", err)
	}
	m := &Material{CA: ca, CADER: ca.Raw, caKey: caKey}

	leaf, leafKey, err := loadPair(leafCertPath, leafKeyPath)
	if err != nil || !leafValid(leaf, ca, hosts) {
		leaf, leafKey, err = newLeaf(opts, hosts, ca, caKey)
		if err == nil {
			err = savePair(leafCertPath, leafKeyPath, leaf, leafKey)
		}
		if err != nil {
			return nil, fmt.Errorf("tlslegacy: leaf: %w", err)
		}
	}
	m.Leaf = tls.Certificate{Certificate: [][]byte{leaf.Raw, ca.Raw}, PrivateKey: leafKey, Leaf: leaf}
	m.Warnings = constraintViolations(ca, hosts)
	return m, nil
}

// constraintViolations returns the hosts the CA's name constraints do not
// permit. The constraints are fixed when the CA is created, so hosts added
// later can fall outside them. (Verifiers that enforce constraints then
// reject the whole certificate, not just those names.)
func constraintViolations(ca *x509.Certificate, hosts []string) []string {
	if len(ca.PermittedDNSDomains) == 0 && len(ca.PermittedIPRanges) == 0 {
		return nil
	}
	var bad []string
	for _, h := range hosts {
		ok := false
		if ip := net.ParseIP(h); ip != nil {
			for _, r := range ca.PermittedIPRanges {
				ok = ok || r.Contains(ip)
			}
		} else {
			for _, d := range ca.PermittedDNSDomains {
				d = strings.ToLower(strings.TrimPrefix(d, "."))
				ok = ok || h == d || strings.HasSuffix(h, "."+d)
			}
		}
		if !ok {
			bad = append(bad, h)
		}
	}
	return bad
}

func normalizeHosts(hosts []string, cn string) []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range append([]string{cn}, hosts...) {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	sort.Strings(out[min(1, len(out)):]) // keep CN first
	return out
}

func sigAlg(opts Options) x509.SignatureAlgorithm {
	if opts.SHA1 {
		return x509.SHA1WithRSA
	}
	return x509.SHA256WithRSA
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	return n
}

func newCA(opts Options, hosts []string) (*x509.Certificate, *rsa.PrivateKey, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "mockingbird bridge CA", Organization: []string{"mockingbird"}},
		NotBefore:             now.Add(-24 * time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
		SignatureAlgorithm:    sigAlg(opts),
	}
	if opts.NameConstraints {
		var dns []string
		var ips []*net.IPNet
		for _, h := range hosts {
			if ip := net.ParseIP(h); ip != nil {
				bits := 32
				if ip.To4() == nil {
					bits = 128
				}
				ips = append(ips, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			} else {
				dns = append(dns, h)
			}
		}
		tmpl.PermittedDNSDomains = dns
		tmpl.PermittedIPRanges = ips
		// Non-critical: verifiers that understand constraints enforce them,
		// and very old ones that do not can still use the certificate.
		tmpl.PermittedDNSDomainsCritical = false
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	return cert, key, err
}

func newLeaf(opts Options, hosts []string, ca *x509.Certificate, caKey *rsa.PrivateKey) (*x509.Certificate, *rsa.PrivateKey, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:       serial(),
		Subject:            pkix.Name{CommonName: hosts[0], Organization: []string{"mockingbird"}},
		NotBefore:          now.Add(-24 * time.Hour),
		NotAfter:           now.AddDate(2, 0, 0),
		KeyUsage:           x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:        []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		SignatureAlgorithm: sigAlg(opts),
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	return cert, key, err
}

func leafValid(leaf, ca *x509.Certificate, hosts []string) bool {
	if leaf == nil || time.Until(leaf.NotAfter) < 30*24*time.Hour || leaf.CheckSignatureFrom(ca) != nil {
		return false
	}
	have := map[string]bool{}
	for _, d := range leaf.DNSNames {
		have[d] = true
	}
	for _, ip := range leaf.IPAddresses {
		have[ip.String()] = true
	}
	for _, h := range hosts {
		if !have[h] {
			return false
		}
	}
	return len(have) == len(hosts)
}

func loadPair(certPath, keyPath string) (*x509.Certificate, *rsa.PrivateKey, error) {
	cb, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, err
	}
	kb, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, err
	}
	cp, _ := pem.Decode(cb)
	kp, _ := pem.Decode(kb)
	if cp == nil || kp == nil {
		return nil, nil, errors.New("bad PEM")
	}
	cert, err := x509.ParseCertificate(cp.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, err := x509.ParsePKCS1PrivateKey(kp.Bytes)
	return cert, key, err
}

func savePair(certPath, keyPath string, cert *x509.Certificate, key *rsa.PrivateKey) error {
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o644)
}

// Fingerprint returns the SHA-256 fingerprint of a DER certificate.
func Fingerprint(der []byte) [32]byte { return sha256.Sum256(der) }
