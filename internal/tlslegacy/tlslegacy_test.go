package tlslegacy

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIssueAndHandshakeTLS10(t *testing.T) {
	dir := t.TempDir()
	opts := Options{Dir: dir, CommonName: "bird.example.net", Hosts: []string{"api.twitter.com", "twitter.com", "search.twitter.com"}, NameConstraints: true}
	m, err := Load(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !m.CA.IsCA || len(m.CA.PermittedDNSDomains) != 4 || m.CA.PermittedDNSDomainsCritical {
		t.Fatalf("CA constraints wrong: %+v", m.CA.PermittedDNSDomains)
	}
	if m.Leaf.Leaf.Subject.CommonName != "bird.example.net" {
		t.Fatalf("CN %q", m.Leaf.Leaf.Subject.CommonName)
	}
	// Reload reuses the same CA (users installed it).
	m2, err := Load(opts)
	if err != nil || string(m2.CADER) != string(m.CADER) {
		t.Fatal("CA changed on reload")
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	srv.TLS = m.Config()
	srv.StartTLS()
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(m.CA)
	// An iPhone OS 3 style client: TLS 1.0, RSA key exchange, AES-128-CBC,
	// no SNI (ServerName only used for verification here).
	for _, host := range []string{"api.twitter.com", "bird.example.net"} {
		cfg := &tls.Config{RootCAs: pool, ServerName: host, MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS10,
			CipherSuites: []uint16{tls.TLS_RSA_WITH_AES_128_CBC_SHA}}
		conn, err := tls.Dial("tcp", srv.Listener.Addr().String(), cfg)
		if err != nil {
			t.Fatalf("%s: TLS 1.0 handshake: %v", host, err)
		}
		st := conn.ConnectionState()
		if st.Version != tls.VersionTLS10 || st.CipherSuite != tls.TLS_RSA_WITH_AES_128_CBC_SHA {
			t.Fatalf("negotiated %x %x", st.Version, st.CipherSuite)
		}
		conn.Close()
	}
	// The name constraints must stop the CA vouching for other sites.
	if _, err := m.Leaf.Leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "www.google.com"}); err == nil {
		t.Fatal("leaf verified for an unrelated host")
	}
	_ = net.IPv4len
}

func TestLeafReissuedWhenHostsChange(t *testing.T) {
	dir := t.TempDir()
	m1, err := Load(Options{Dir: dir, CommonName: "a.example", Hosts: []string{"twitter.com"}})
	if err != nil {
		t.Fatal(err)
	}
	m2, err := Load(Options{Dir: dir, CommonName: "a.example", Hosts: []string{"twitter.com", "api.twitter.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if m1.Leaf.Leaf.SerialNumber.Cmp(m2.Leaf.Leaf.SerialNumber) == 0 {
		t.Fatal("leaf not reissued")
	}
	if string(m1.CADER) != string(m2.CADER) {
		t.Fatal("CA must survive host changes")
	}
}
