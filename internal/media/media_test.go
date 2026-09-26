package media

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSignedURLRoundTrip(t *testing.T) {
	s := NewSigner([]byte("k"), "http://bridge.example")
	u := s.FromCDN("https://cdn.bsky.app/img/avatar/plain/did:plc:abc123/bafkreiabcdefgh@jpeg", "_normal")
	if !strings.HasPrefix(u, "http://bridge.example/img/a/") || !strings.HasSuffix(u, "/did~plc~abc123/bafkreiabcdefgh_normal.jpg") {
		t.Fatalf("url %q", u)
	}
	path := strings.TrimPrefix(u, "http://bridge.example")
	r, ok := s.Parse(path)
	if !ok || r.DID != "did:plc:abc123" || r.CID != "bafkreiabcdefgh" || r.Width != 48 {
		t.Fatalf("parse %+v %v", r, ok)
	}
	// Clients swap the size suffix themselves; the signature must survive.
	for suffix, w := range map[string]int{"_bigger": 73, "_mini": 24, "": 480} {
		p := strings.Replace(path, "_normal", suffix, 1)
		r, ok := s.Parse(p)
		if !ok || r.Width != w {
			t.Fatalf("suffix %q: %+v %v", suffix, r, ok)
		}
	}
	// Tampering with the DID or CID breaks the signature.
	if _, ok := s.Parse(strings.Replace(path, "abc123", "evil99", 1)); ok {
		t.Fatal("tampered DID accepted")
	}
	if _, ok := s.Parse(strings.Replace(path, "bafkreiabcdefgh", "bafkreiabcdefgx", 1)); ok {
		t.Fatal("tampered CID accepted")
	}
	if s.FromCDN("https://evil.example/img/avatar/plain/did:plc:abc/../../x", "") != "" {
		t.Fatal("non-CDN URL should not be signed")
	}
}

func testJPEG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var b bytes.Buffer
	jpeg.Encode(&b, img, nil)
	return b.Bytes()
}

func TestRender(t *testing.T) {
	out, err := Render(testJPEG(1000, 500), 480, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := jpeg.DecodeConfig(bytes.NewReader(out))
	if cfg.Width != 480 || cfg.Height != 240 {
		t.Fatalf("got %dx%d", cfg.Width, cfg.Height)
	}
	out, _ = Render(testJPEG(300, 200), 48, true)
	cfg, _ = jpeg.DecodeConfig(bytes.NewReader(out))
	if cfg.Width != 48 || cfg.Height != 48 {
		t.Fatalf("avatar got %dx%d", cfg.Width, cfg.Height)
	}
	if len(out) > 4000 {
		t.Fatalf("48px avatar is %d bytes; should be tiny", len(out))
	}
}

func TestDiskCacheEviction(t *testing.T) {
	dir := t.TempDir()
	c, err := OpenDiskCache(dir, 250)
	if err != nil {
		t.Fatal(err)
	}
	c.Put("a", bytes.Repeat([]byte{1}, 100))
	c.Put("b", bytes.Repeat([]byte{2}, 100))
	c.Get("a")
	c.Put("c", bytes.Repeat([]byte{3}, 100))
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should be evicted")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a should remain (recently used)")
	}
	size, n := c.Size()
	if size > 250 || n != 2 {
		t.Fatalf("size=%d n=%d", size, n)
	}
	// Reopen: index rebuilt from disk.
	c2, _ := OpenDiskCache(dir, 250)
	if _, ok := c2.Get("c"); !ok {
		t.Fatal("cache not persisted")
	}
}

func TestProxyCoalescesAndCaches(t *testing.T) {
	var hits atomic.Int32
	src := testJPEG(400, 400)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !strings.HasPrefix(r.URL.Path, "/img/avatar_thumbnail/plain/did:plc:abc/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(src)
	}))
	defer cdn.Close()
	dc, _ := OpenDiskCache(t.TempDir(), 1<<20)
	s := NewSigner([]byte("k"), "http://bridge")
	p := &Proxy{Signer: s, CDN: cdn.URL, HTTP: cdn.Client(), Cache: dc, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	path := strings.TrimPrefix(s.URL(KindAvatar, "did:plc:abc", "bafkreiabcdefgh", "_normal"), "http://bridge")
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/jpeg" || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
			t.Fatalf("status %d headers %v", rec.Code, rec.Header())
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("CDN hit %d times, want 1", hits.Load())
	}
	// Conditional request.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", path, nil)
	p.ServeHTTP(rec, req)
	req.Header.Set("If-None-Match", rec.Header().Get("ETag"))
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("want 304, got %d", rec2.Code)
	}
	// Unsigned path is refused without touching the CDN.
	rec3 := httptest.NewRecorder()
	p.ServeHTTP(rec3, httptest.NewRequest("GET", "/img/a/AAAAAAAAAAAAAAAA/did~plc~abc/bafkreiabcdefgh.jpg", nil))
	if rec3.Code != 404 || hits.Load() != 1 {
		t.Fatalf("unsigned request: %d, cdn hits %d", rec3.Code, hits.Load())
	}
}
