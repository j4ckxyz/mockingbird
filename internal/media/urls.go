// Package media serves Bluesky images to 2009 clients: signed bridge URLs,
// a resizing proxy with a size-capped disk cache, and default avatars.
package media

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Kinds of proxied image, mapped to Bluesky CDN presets.
const (
	KindAvatar = "a"
	KindFull   = "f"
	KindThumb  = "t"
	KindBanner = "b"
)

// Signer builds and verifies HMAC-signed image URLs so the proxy cannot be
// used to fetch arbitrary images. The signature covers kind, DID and CID but
// not the size suffix: clients rewrite "_normal" to "_bigger" themselves.
type Signer struct {
	key  []byte
	base string // public base URL, no trailing slash
}

// NewSigner returns a signer. key should be derived from the server secret.
func NewSigner(key []byte, base string) *Signer {
	return &Signer{key: key, base: strings.TrimRight(base, "/")}
}

func (s *Signer) sig(kind, did, cid string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(kind + "\x00" + did + "\x00" + cid))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:12])
}

// Verify checks a signature in constant time.
func (s *Signer) Verify(kind, did, cid, sig string) bool {
	return hmac.Equal([]byte(s.sig(kind, did, cid)), []byte(sig))
}

// encodeDID makes a DID path-safe for old URL parsers ("did:plc:x" -> "did~plc~x").
func encodeDID(did string) string { return strings.ReplaceAll(did, ":", "~") }
func decodeDID(s string) string   { return strings.ReplaceAll(s, "~", ":") }

// URL returns the bridge URL for an image. suffix is "", "_mini", "_normal"
// or "_bigger".
func (s *Signer) URL(kind, did, cid, suffix string) string {
	return s.base + "/img/" + kind + "/" + s.sig(kind, did, cid) + "/" + encodeDID(did) + "/" + cid + suffix + ".jpg"
}

var cdnRE = regexp.MustCompile(`^https://[^/]+/img/(avatar|avatar_thumbnail|feed_fullsize|feed_thumbnail|banner)/plain/(did:[a-z]+:[A-Za-z0-9._:%-]+)/([a-z0-9]+)(@[a-z]+)?$`)

// FromCDN converts a Bluesky CDN URL into a signed bridge URL. It returns ""
// for anything it does not recognise.
func (s *Signer) FromCDN(cdnURL, suffix string) string {
	m := cdnRE.FindStringSubmatch(cdnURL)
	if m == nil {
		return ""
	}
	kind := KindFull
	switch m[1] {
	case "avatar", "avatar_thumbnail":
		kind = KindAvatar
	case "feed_thumbnail":
		kind = KindThumb
	case "banner":
		kind = KindBanner
	}
	did, err := url.PathUnescape(m[2])
	if err != nil {
		return ""
	}
	return s.URL(kind, did, m[3], suffix)
}

// Request is a parsed /img/ path.
type Request struct {
	Kind   string
	DID    string
	CID    string
	Suffix string
	Width  int // target max width (and height for avatars)
}

var pathRE = regexp.MustCompile(`^/img/([aftb])/([A-Za-z0-9_-]{16})/(did~[a-z]+~[A-Za-z0-9._~%-]+)/([a-z0-9]{8,100}?)(_mini|_normal|_bigger|_reasonably_small)?\.(jpg|jpeg|png|gif)$`)

// Parse validates a request path and its signature.
func (s *Signer) Parse(path string) (*Request, bool) {
	m := pathRE.FindStringSubmatch(path)
	if m == nil {
		return nil, false
	}
	r := &Request{Kind: m[1], DID: decodeDID(m[3]), CID: m[4], Suffix: m[5]}
	if !s.Verify(r.Kind, r.DID, r.CID, m[2]) {
		return nil, false
	}
	r.Width = TargetWidth(r.Kind, r.Suffix)
	return r, true
}

// TargetWidth maps Twitter's avatar suffixes to pixel sizes and caps
// everything else at 480px, which covers the original iPhone's screen.
func TargetWidth(kind, suffix string) int {
	switch suffix {
	case "_mini":
		return 24
	case "_normal":
		return 48
	case "_bigger":
		return 73
	case "_reasonably_small":
		return 128
	}
	if kind == KindThumb {
		return 320
	}
	return 480
}

// CacheKey identifies a rendered variant.
func (r *Request) CacheKey() string {
	return r.Kind + "/" + r.DID + "/" + r.CID + "/" + strconv.Itoa(r.Width)
}

// UpstreamPreset picks the smallest CDN preset that satisfies the request.
func (r *Request) UpstreamPreset() string {
	switch r.Kind {
	case KindAvatar:
		if r.Width <= 128 {
			return "avatar_thumbnail"
		}
		return "avatar"
	case KindThumb:
		return "feed_thumbnail"
	case KindBanner:
		return "banner"
	}
	return "feed_fullsize"
}
