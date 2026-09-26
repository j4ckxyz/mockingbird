package media

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif" // decoders
	"image/jpeg"
	_ "image/png"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
	"golang.org/x/sync/singleflight"

	"github.com/jackgilbert/mockingbird/internal/atp"
	"github.com/jackgilbert/mockingbird/internal/cache"
)

// Proxy fetches, resizes and caches images.
type Proxy struct {
	Signer *Signer
	CDN    string // e.g. https://cdn.bsky.app
	HTTP   *http.Client
	Cache  *DiskCache
	Logger *slog.Logger
	// Hook observes upstream fetch outcomes (metrics).
	Hook func(outcome string)

	sf      singleflight.Group
	missing *cache.LRU[string, bool]
	once    sync.Once
}

func (p *Proxy) init() {
	p.once.Do(func() { p.missing = cache.New[string, bool](10_000) })
}

func (p *Proxy) hook(o string) {
	if p.Hook != nil {
		p.Hook(o)
	}
}

// ServeHTTP serves /img/... paths.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.init()
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req, ok := p.Signer.Parse(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	etag := `"` + shortHash(req.CacheKey()) + `"`
	if inm := r.Header.Get("If-None-Match"); inm != "" && strings.Contains(inm, etag) {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body, err := p.Get(r.Context(), req)
	if err != nil {
		if errors.Is(err, errUpstreamMissing) {
			http.NotFound(w, r)
			return
		}
		p.Logger.Warn("image proxy", "err", err, "kind", req.Kind)
		http.Error(w, "image unavailable", http.StatusBadGateway)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "image/jpeg")
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("ETag", etag)
	h.Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
}

var errUpstreamMissing = errors.New("media: upstream image not found")

// Get returns the rendered JPEG for req, from cache or upstream.
func (p *Proxy) Get(ctx context.Context, req *Request) ([]byte, error) {
	p.init()
	key := req.CacheKey()
	if b, ok := p.Cache.Get(key); ok {
		p.hook("hit")
		return b, nil
	}
	if p.missing.Len() > 0 {
		if _, gone := p.missing.Get(key); gone {
			return nil, errUpstreamMissing
		}
	}
	v, err, _ := p.sf.Do(key, func() (any, error) {
		// Detach from the first requester so its disconnect does not fail
		// everyone coalesced onto this fetch.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		src, err := p.fetch(ctx, req)
		if err != nil {
			return nil, err
		}
		out, err := Render(src, req.Width, req.Kind == KindAvatar && req.Suffix != "")
		if err != nil {
			return nil, err
		}
		p.Cache.Put(key, out)
		return out, nil
	})
	if err != nil {
		if errors.Is(err, errUpstreamMissing) {
			p.missing.AddTTL(key, true, 10*time.Minute)
			p.hook("missing")
		} else {
			p.hook("error")
		}
		return nil, err
	}
	p.hook("miss")
	return v.([]byte), nil
}

func (p *Proxy) fetch(ctx context.Context, req *Request) ([]byte, error) {
	u := p.CDN + "/img/" + req.UpstreamPreset() + "/plain/" + url.PathEscape(req.DID) + "/" + req.CID + "@jpeg"
	hr, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	resp, err := p.HTTP.Do(hr)
	atp.AddUpstream(ctx, time.Since(start))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusBadRequest {
		return nil, errUpstreamMissing
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("media: CDN status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// Render decodes an image and re-encodes it as a baseline JPEG no wider than
// maxW. Square crops are used for avatar sizes, matching Twitter's avatars.
func Render(src []byte, maxW int, square bool) ([]byte, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(src))
	if err != nil {
		return nil, fmt.Errorf("media: decode config: %w", err)
	}
	if cfg.Width*cfg.Height > 40_000_000 {
		return nil, errors.New("media: image too large")
	}
	img, _, err := image.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, fmt.Errorf("media: decode: %w", err)
	}
	b := img.Bounds()
	if square {
		side := min(b.Dx(), b.Dy())
		x0 := b.Min.X + (b.Dx()-side)/2
		y0 := b.Min.Y + (b.Dy()-side)/2
		b = image.Rect(x0, y0, x0+side, y0+side)
	}
	w, h := b.Dx(), b.Dy()
	if square {
		w, h = maxW, maxW
	} else if w > maxW {
		h = max(1, h*maxW/w)
		w = maxW
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	// White background for images with transparency (JPEG has none).
	draw.Draw(dst, dst.Bounds(), &image.Uniform{color.White}, image.Point{}, draw.Src)
	scaler := xdraw.ApproxBiLinear
	if w <= 128 {
		scaler = xdraw.CatmullRom
	}
	if w == b.Dx() && h == b.Dy() {
		draw.Draw(dst, dst.Bounds(), img, b.Min, draw.Over)
	} else {
		scaler.Scale(dst, dst.Bounds(), img, b, draw.Over, nil)
	}
	q := 78
	if w <= 128 {
		q = 85
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: q}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:12])
}

// DiskCache is a size-capped LRU of files under a directory.
type DiskCache struct {
	dir   string
	max   int64
	mu    sync.Mutex
	size  int64
	ll    *list.List
	items map[string]*list.Element
}

type diskEntry struct {
	name string // hashed file name
	size int64
}

// OpenDiskCache opens (and indexes) a cache directory.
func OpenDiskCache(dir string, maxBytes int64) (*DiskCache, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	c := &DiskCache{dir: dir, max: maxBytes, ll: list.New(), items: map[string]*list.Element{}}
	type f struct {
		name string
		size int64
		mod  time.Time
	}
	var files []f
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".tmp") {
			os.Remove(path)
			return nil
		}
		info, err := d.Info()
		if err == nil {
			files = append(files, f{name: filepath.Base(path), size: info.Size(), mod: info.ModTime()})
		}
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, fl := range files {
		c.items[fl.name] = c.ll.PushFront(&diskEntry{name: fl.name, size: fl.size})
		c.size += fl.size
	}
	c.evict()
	return c, nil
}

func (c *DiskCache) path(name string) string {
	return filepath.Join(c.dir, name[:2], name)
}

func fileName(key string) string { return shortHash(key) + ".jpg" }

// Get returns a cached file.
func (c *DiskCache) Get(key string) ([]byte, bool) {
	name := fileName(key)
	c.mu.Lock()
	el, ok := c.items[name]
	if ok {
		c.ll.MoveToFront(el)
	}
	c.mu.Unlock()
	if !ok {
		return nil, false
	}
	b, err := os.ReadFile(c.path(name))
	if err != nil {
		c.mu.Lock()
		if el, ok := c.items[name]; ok {
			c.size -= el.Value.(*diskEntry).size
			c.ll.Remove(el)
			delete(c.items, name)
		}
		c.mu.Unlock()
		return nil, false
	}
	return b, true
}

// Put stores a file atomically and evicts least recently used files.
func (c *DiskCache) Put(key string, b []byte) {
	name := fileName(key)
	p := c.path(name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[name]; ok {
		c.size -= el.Value.(*diskEntry).size
		c.ll.Remove(el)
	}
	c.items[name] = c.ll.PushFront(&diskEntry{name: name, size: int64(len(b))})
	c.size += int64(len(b))
	c.evict()
}

func (c *DiskCache) evict() {
	for c.size > c.max && c.ll.Len() > 0 {
		el := c.ll.Back()
		e := el.Value.(*diskEntry)
		c.ll.Remove(el)
		delete(c.items, e.name)
		c.size -= e.size
		os.Remove(c.path(e.name))
	}
}

// Size returns the cache's total bytes and file count.
func (c *DiskCache) Size() (int64, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.size, c.ll.Len()
}
