package api

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackgilbert/mockingbird/internal/config"
)

// RFC 5849 section 1.2 / OAuth Core 1.0 Appendix A example.
func TestHMACSHA1RFCExample(t *testing.T) {
	params := [][2]string{
		{"file", "vacation.jpg"}, {"size", "original"},
		{"oauth_consumer_key", "dpf43f3p2l4k3l03"}, {"oauth_token", "nnch734d00sl2jdk"},
		{"oauth_signature_method", "HMAC-SHA1"}, {"oauth_timestamp", "1191242096"},
		{"oauth_nonce", "kllo9940pd9333jh"}, {"oauth_version", "1.0"},
	}
	base := signatureBase("GET", "http://photos.example.net/photos", params)
	want := "GET&http%3A%2F%2Fphotos.example.net%2Fphotos&file%3Dvacation.jpg%26oauth_consumer_key%3Ddpf43f3p2l4k3l03%26oauth_nonce%3Dkllo9940pd9333jh%26oauth_signature_method%3DHMAC-SHA1%26oauth_timestamp%3D1191242096%26oauth_token%3Dnnch734d00sl2jdk%26oauth_version%3D1.0%26size%3Doriginal"
	if base != want {
		t.Fatalf("base string:\n got %s\nwant %s", base, want)
	}
	if sig := hmacSHA1(base, "kd94hf93k423kf44", "pfkkdhi9sl3r4s00"); sig != "tR3+Ty81lMeYAr/Fid0kMTYa/WM=" {
		t.Fatalf("signature %s", sig)
	}
}

// Twitter's "Creating a signature" documentation example.
func TestHMACSHA1TwitterExample(t *testing.T) {
	params := [][2]string{
		{"status", "Hello Ladies + Gentlemen, a signed OAuth request!"}, {"include_entities", "true"},
		{"oauth_consumer_key", "xvz1evFS4wEEPTGEFPHBog"}, {"oauth_nonce", "kYjzVBB8Y0ZFabxSWbWovY3uYSQ2pTgmZeNu2VS4cg"},
		{"oauth_signature_method", "HMAC-SHA1"}, {"oauth_timestamp", "1318622958"},
		{"oauth_token", "370773112-GmHxMAgYyLbNEtIKZeRNFsMKPR9EyMZeS9weJAEb"}, {"oauth_version", "1.0"},
	}
	base := signatureBase("POST", "https://api.twitter.com/1/statuses/update.json", params)
	sig := hmacSHA1(base, "kAcSOqF21Fu85e7zjz7ZN2U4ZRhfV3WpwPAoE3Z7kBw", "LswwdoUaIvS8ltyTt5jkRh4J50vUPVVHtR2YPi5kE")
	if sig != "tnnArxj06cWHq44gCs1OSKk/jLY=" {
		t.Fatalf("signature %s", sig)
	}
}

func TestParseOAuthHeader(t *testing.T) {
	p := parseOAuthHeader(`OAuth realm="", oauth_consumer_key="abc", oauth_signature="tR3%2BTy81lMeYAr%2FFid0kMTYa%2FWM%3D",oauth_token="t%20k"`)
	if p["oauth_consumer_key"] != "abc" || p["oauth_signature"] != "tR3+Ty81lMeYAr/Fid0kMTYa/WM=" || p["oauth_token"] != "t k" {
		t.Fatalf("%v", p)
	}
}

// A request from a configured consumer must carry a valid signature and a
// fresh, unused nonce.
func TestVerifySignatureKnownConsumer(t *testing.T) {
	s := &Server{cfg: &config.Config{OAuthTimestampWindow: time.Hour}, oauth: newOAuthStore(time.Minute), now: func() time.Time { return time.Unix(1318622958, 0) }}
	form := url.Values{"status": {"Hello Ladies + Gentlemen, a signed OAuth request!"}}
	r := httptest.NewRequest("POST", "https://api.twitter.com/1/statuses/update.json?include_entities=true", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.ParseForm()
	p := map[string]string{
		"oauth_consumer_key": "xvz1evFS4wEEPTGEFPHBog", "oauth_nonce": "kYjzVBB8Y0ZFabxSWbWovY3uYSQ2pTgmZeNu2VS4cg",
		"oauth_signature_method": "HMAC-SHA1", "oauth_timestamp": "1318622958",
		"oauth_token": "370773112-GmHxMAgYyLbNEtIKZeRNFsMKPR9EyMZeS9weJAEb", "oauth_version": "1.0",
		"oauth_signature": "tnnArxj06cWHq44gCs1OSKk/jLY=",
	}
	c := &Ctx{s: s, r: r, Form: r.Form}
	cs, ts := "kAcSOqF21Fu85e7zjz7ZN2U4ZRhfV3WpwPAoE3Z7kBw", "LswwdoUaIvS8ltyTt5jkRh4J50vUPVVHtR2YPi5kE"
	if err := s.verifySignature(c, p, cs, ts); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if err := s.verifySignature(c, p, cs, ts); err == nil || !strings.Contains(err.Error(), "nonce") {
		t.Fatalf("replayed nonce accepted: %v", err)
	}
	p["oauth_nonce"] = "other"
	if err := s.verifySignature(c, p, cs, ts); err == nil {
		t.Fatal("tampered request accepted")
	}
	s.now = func() time.Time { return time.Unix(1318622958, 0).Add(3 * time.Hour) }
	p["oauth_nonce"] = "kYjzVBB8Y0ZFabxSWbWovY3uYSQ2pTgmZeNu2VS4cg"
	if err := s.verifySignature(c, p, cs, ts); err == nil || !strings.Contains(err.Error(), "timestamp") {
		t.Fatalf("stale timestamp accepted: %v", err)
	}
}
