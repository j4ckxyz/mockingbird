// Package web serves the bridge's human-facing pages: setup instructions,
// the OAuth sign-in form, lightweight post pages for in-app browsers, and
// the legacy TLS CA download.
package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/j4ckxyz/mockingbird/internal/api"
	"github.com/j4ckxyz/mockingbird/internal/atp"
	"github.com/j4ckxyz/mockingbird/internal/cache"
	"github.com/j4ckxyz/mockingbird/internal/config"
	"github.com/j4ckxyz/mockingbird/internal/media"
	"github.com/j4ckxyz/mockingbird/internal/store"
	"github.com/j4ckxyz/mockingbird/internal/translate"
)

// Deps are the web handler's collaborators.
type Deps struct {
	Config *config.Config
	API    *api.Server
	Store  *store.Store
	Public *atp.Client
	Signer *media.Signer
	Logger *slog.Logger
	// CA returns the DER CA certificate when the legacy TLS listener is on.
	CA func() []byte
}

// Handler serves pages.
type Handler struct {
	d     Deps
	posts *cache.LRU[int64, *postPage]
}

// New builds the handler.
func New(d Deps) *Handler {
	return &Handler{d: d, posts: cache.New[int64, *postPage](5000)}
}

type pageData struct {
	Title string
	Theme string
	Here  string
	// home
	Base            string
	DefaultHost     string
	IP              string
	TLS             bool
	NameConstrained bool
	TLSHosts        string
	CAFingerprint   string
	// authorize
	Page api.AuthorizePage
	// post
	Post *postPage
	// message
	Heading, Message string
}

func (h *Handler) data(r *http.Request, title string) pageData {
	theme := "auto"
	if c, err := r.Cookie("mb_theme"); err == nil && (c.Value == "light" || c.Value == "dark") {
		theme = c.Value
	}
	return pageData{Title: title, Theme: theme, Here: r.URL.Path}
}

func (h *Handler) render(w http.ResponseWriter, r *http.Request, name string, d pageData, status int) {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, d); err != nil {
		h.d.Logger.Error("template", "name", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", "text/html; charset=utf-8")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("X-Frame-Options", "DENY")
	hd.Set("Referrer-Policy", "no-referrer")
	hd.Set("Content-Security-Policy", "default-src 'none'; img-src 'self' "+h.d.Config.PublicURL.Scheme+"://"+h.d.Config.PublicURL.Host+"; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

var (
	statusPathRE = regexp.MustCompile(`^/[A-Za-z0-9_.:-]+/status(?:es)?/([0-9]{1,15})/?$`)
	userPathRE   = regexp.MustCompile(`^/([A-Za-z0-9_.-]{1,253})/?$`)
)

// reserved names are never treated as twitter.com/<screen_name> links.
var reserved = map[string]bool{"metrics": true, "admin": true, "debug": true, "api": true, "oauth": true, "search": true,
	"login": true, "logout": true, "home": true, "settings": true, "account": true, "statuses": true, "users": true,
	"img": true, "static": true, "p": true, "m": true, "1": true, "healthz": true, "sessions": true}

// ServeHTTP routes page requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/" || p == "/setup":
		h.home(w, r)
	case p == "/healthz":
		w.Header().Set("Content-Type", "text/plain")
		if err := h.d.Store.Ping(r.Context()); err != nil {
			http.Error(w, "db unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	case p == "/robots.txt":
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("User-agent: *\nDisallow: /\n"))
	case p == "/theme":
		h.theme(w, r)
	case p == "/favicon.ico":
		h.static(w, r, "favicon.ico")
	case strings.HasPrefix(p, "/static/"):
		h.static(w, r, strings.TrimPrefix(p, "/static/"))
	case p == "/oauth/authorize" || p == "/oauth/authenticate":
		h.d.API.OAuthAuthorize(w, r, func(w http.ResponseWriter, r *http.Request, page api.AuthorizePage) {
			d := h.data(r, "Sign in with Bluesky")
			d.Page = page
			status := http.StatusOK
			if page.Error != "" && page.Token == "" {
				status = http.StatusNotFound
			}
			h.render(w, r, "authorize", d, status)
		})
	case strings.HasPrefix(p, "/p/"):
		id, err := strconv.ParseInt(strings.TrimPrefix(p, "/p/"), 10, 64)
		if err != nil || id <= 0 {
			h.notFound(w, r)
			return
		}
		h.post(w, r, id)
	case strings.HasPrefix(p, "/m/"):
		h.d.API.ServeUpload(w, r, strings.TrimPrefix(p, "/m/"))
	case p == "/ca.crt" || p == "/ca.cer":
		h.caCert(w, r)
	case p == "/mockingbird.mobileconfig":
		h.mobileconfig(w, r)
	default:
		// twitter.com web URLs that clients open in their in-app browser.
		if m := statusPathRE.FindStringSubmatch(p); m != nil {
			http.Redirect(w, r, "/p/"+m[1], http.StatusFound)
			return
		}
		if m := userPathRE.FindStringSubmatch(p); m != nil && !strings.Contains(m[1], "..") && !reserved[strings.ToLower(m[1])] {
			handle := strings.ToLower(m[1])
			if !strings.Contains(handle, ".") {
				handle += "." + h.d.Config.DefaultHandleHost
			}
			http.Redirect(w, r, "https://bsky.app/profile/"+url.PathEscape(handle), http.StatusFound)
			return
		}
		h.notFound(w, r)
	}
}

func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	d := h.data(r, "Not found")
	d.Heading, d.Message = "Nothing here", "That page does not exist."
	h.render(w, r, "message", d, http.StatusNotFound)
}

func (h *Handler) home(w http.ResponseWriter, r *http.Request) {
	d := h.data(r, "mockingbird: vintage Twitter clients on Bluesky")
	cfg := h.d.Config
	d.Base = cfg.PublicURL.String()
	d.DefaultHost = cfg.DefaultHandleHost
	if host := cfg.PublicURL.Hostname(); net.ParseIP(host) != nil {
		d.IP = host
	}
	if h.d.CA != nil {
		if der := h.d.CA(); der != nil {
			d.TLS = true
			sum := sha256.Sum256(der)
			var parts []string
			for _, b := range sum {
				parts = append(parts, fmt.Sprintf("%02X", b))
			}
			d.CAFingerprint = strings.Join(parts, ":")
			d.NameConstrained = cfg.TLSNameConstraints
			hosts := append(append([]string{}, cfg.BridgeHosts...), cfg.TLSExtraHosts...)
			sort.Strings(hosts)
			d.TLSHosts = strings.Join(hosts, ", ")
		}
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	h.render(w, r, "home", d, http.StatusOK)
}

func (h *Handler) theme(w http.ResponseWriter, r *http.Request) {
	set := r.URL.Query().Get("set")
	if set != "light" && set != "dark" {
		set = "auto"
	}
	http.SetCookie(w, &http.Cookie{Name: "mb_theme", Value: set, Path: "/", MaxAge: 365 * 24 * 3600, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	back := r.URL.Query().Get("r")
	if !localPath(back) {
		back = "/"
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// localPath reports whether p is a path on this site, so redirecting to it
// cannot leave the site. Browsers drop tabs and newlines from URLs, so
// "/\t/evil.example" would become "//evil.example": any control character
// is refused, as are backslashes (treated as "/" by browsers).
func localPath(p string) bool {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.Contains(p, "\\") {
		return false
	}
	for i := 0; i < len(p); i++ {
		if p[i] < 0x20 || p[i] == 0x7f {
			return false
		}
	}
	u, err := url.Parse(p)
	return err == nil && u.Scheme == "" && u.Host == ""
}

func (h *Handler) static(w http.ResponseWriter, r *http.Request, name string) {
	b, ok := loadAssets()[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	ct := "image/png"
	switch {
	case strings.HasSuffix(name, ".gif"):
		ct = "image/gif"
	case strings.HasSuffix(name, ".ico"):
		ct = "image/png"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=2592000")
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(b))
}

func (h *Handler) caCert(w http.ResponseWriter, r *http.Request) {
	if h.d.CA == nil || h.d.CA() == nil {
		h.notFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/x-x509-ca-cert")
	w.Header().Set("Content-Disposition", `attachment; filename="mockingbird-ca.crt"`)
	w.Write(h.d.CA())
}

// mobileconfig serves an unsigned configuration profile with the CA as a
// root certificate payload; iPhone OS 3 Safari installs these directly.
func (h *Handler) mobileconfig(w http.ResponseWriter, r *http.Request) {
	if h.d.CA == nil || h.d.CA() == nil {
		h.notFound(w, r)
		return
	}
	der := h.d.CA()
	sum := sha256.Sum256(der)
	uuid := func(salt byte) string {
		b := sha256.Sum256(append([]byte{salt}, sum[:]...))
		return fmt.Sprintf("%X-%X-%X-%X-%X", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	}
	host := h.d.Config.PublicURL.Hostname()
	body := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>PayloadContent</key><array><dict>
<key>PayloadCertificateFileName</key><string>mockingbird-ca.cer</string>
<key>PayloadContent</key><data>` + base64.StdEncoding.EncodeToString(der) + `</data>
<key>PayloadDescription</key><string>Trust the mockingbird bridge's certificate authority</string>
<key>PayloadDisplayName</key><string>mockingbird CA</string>
<key>PayloadIdentifier</key><string>bridge.mockingbird.ca.cert</string>
<key>PayloadType</key><string>com.apple.security.root</string>
<key>PayloadUUID</key><string>` + uuid(1) + `</string>
<key>PayloadVersion</key><integer>1</integer>
</dict></array>
<key>PayloadDescription</key><string>Lets vintage apps connect to the mockingbird bridge at ` + xmlEscape(host) + ` over HTTPS.</string>
<key>PayloadDisplayName</key><string>mockingbird bridge</string>
<key>PayloadIdentifier</key><string>bridge.mockingbird.profile</string>
<key>PayloadOrganization</key><string>mockingbird</string>
<key>PayloadRemovalDisallowed</key><false/>
<key>PayloadType</key><string>Configuration</string>
<key>PayloadUUID</key><string>` + uuid(2) + `</string>
<key>PayloadVersion</key><integer>1</integer>
</dict></plist>
`
	w.Header().Set("Content-Type", "application/x-apple-aspen-config")
	w.Header().Set("Content-Disposition", `attachment; filename="mockingbird.mobileconfig"`)
	w.Write([]byte(body))
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// errHidden marks posts whose authors limit logged-out visibility.
var errHidden = errors.New("web: author limits logged-out visibility")

// Post pages.

type segment struct{ Text, Href string }

type imageRef struct{ Thumb, Full, Alt string }

type quoteRef struct{ Name, Handle, Text, Page string }

type postPage struct {
	Name, Handle, Avatar string
	Segments             []segment
	Images               []imageRef
	Video                bool
	Card                 *atp.ExternalView
	Quote                *quoteRef
	When                 string
	BskyURL              string
}

func (h *Handler) post(w http.ResponseWriter, r *http.Request, id int64) {
	pp, ok := h.posts.Get(id)
	if !ok {
		var err error
		pp, err = h.loadPost(r.Context(), id)
		if errors.Is(err, errHidden) {
			d := h.data(r, "Post unavailable")
			d.Heading, d.Message = "Sign in to see this post", "The author has chosen to show their posts only to people signed in to Bluesky."
			h.render(w, r, "message", d, http.StatusForbidden)
			return
		}
		if err != nil {
			d := h.data(r, "Post unavailable")
			d.Heading, d.Message = "Post unavailable", "This post was deleted, is not public, or Bluesky could not be reached."
			h.render(w, r, "message", d, http.StatusNotFound)
			return
		}
		h.posts.AddTTL(id, pp, 5*time.Minute)
	}
	d := h.data(r, pp.Name+" on Bluesky")
	d.Post = pp
	w.Header().Set("Cache-Control", "public, max-age=300")
	h.render(w, r, "post", d, http.StatusOK)
}

func (h *Handler) loadPost(ctx context.Context, id int64) (*postPage, error) {
	uri, _, err := h.d.Store.StatusByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if translate.CollectionFromURI(uri) != "app.bsky.feed.post" {
		return nil, store.ErrNotFound
	}
	var res atp.Posts
	if err := h.d.Public.Do(ctx, &atp.Request{NSID: "app.bsky.feed.getPosts", Params: url.Values{"uris": {uri}}}, &res); err != nil {
		return nil, err
	}
	if len(res.Posts) == 0 {
		return nil, store.ErrNotFound
	}
	p := &res.Posts[0]
	if p.Author.NoUnauthenticated() {
		// The author opted out of being shown to logged-out viewers; this
		// page is public, so respect that.
		return nil, errHidden
	}
	rec := p.Post()
	pp := &postPage{
		Name: p.Author.DisplayName, Handle: p.Author.Handle,
		Avatar:   h.avatar(p.Author.Avatar),
		Segments: segments(rec.Text, rec.Facets),
		When:     p.SortAt().UTC().Format("3:04 PM Jan 2nd, 2006 UTC"),
		BskyURL:  "https://bsky.app/profile/" + p.Author.Handle + "/post/" + translate.RkeyFromURI(p.URI),
	}
	pp.When = strings.Replace(pp.When, "2nd", ordinal(p.SortAt().UTC().Day()), 1)
	if pp.Name == "" {
		pp.Name = p.Author.Handle
	}
	h.addEmbed(ctx, pp, p.Embed)
	return pp, nil
}

func ordinal(d int) string {
	suffix := "th"
	switch {
	case d%100 >= 11 && d%100 <= 13:
	case d%10 == 1:
		suffix = "st"
	case d%10 == 2:
		suffix = "nd"
	case d%10 == 3:
		suffix = "rd"
	}
	return strconv.Itoa(d) + suffix
}

func (h *Handler) avatar(cdn string) string {
	if u := h.d.Signer.FromCDN(cdn, "_normal"); u != "" {
		return u
	}
	return "/static/default_profile_normal.png"
}

func (h *Handler) addEmbed(ctx context.Context, pp *postPage, e *atp.EmbedView) {
	if e == nil {
		return
	}
	switch e.Type {
	case atp.EmbedImagesView:
		for _, im := range e.Images {
			pp.Images = append(pp.Images, imageRef{
				Thumb: orEmpty(h.d.Signer.FromCDN(im.Thumb, ""), h.d.Signer.FromCDN(im.Fullsize, "")),
				Full:  h.d.Signer.FromCDN(im.Fullsize, ""),
				Alt:   im.Alt,
			})
		}
	case atp.EmbedVideoView, atp.EmbedGalleryView:
		pp.Video = true
	case atp.EmbedExternalView:
		pp.Card = e.External
	case atp.EmbedRecordView, atp.EmbedRecordWithMediaView:
		if e.Media != nil {
			h.addEmbed(ctx, pp, e.Media)
		}
		if rec := e.Record(); rec != nil && rec.Type == atp.RecordViewRecord && rec.Author != nil && !rec.Author.NoUnauthenticated() {
			q := &quoteRef{Name: orEmpty(rec.Author.DisplayName, rec.Author.Handle), Handle: rec.Author.Handle, Text: rec.Post().Text}
			ids, err := h.d.Store.StatusIDs(ctx, []store.StatusRef{{URI: rec.URI, SortAt: atp.SortTime(rec.Post().CreatedAt, rec.IndexedAt)}})
			if err == nil {
				q.Page = "/p/" + strconv.FormatInt(ids[rec.URI], 10)
			}
			pp.Quote = q
		}
	}
}

func orEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// segments splits text into plain and linked runs using facets. The
// template escapes each run, so no text reaches the page unescaped.
func segments(text string, facets []atp.Facet) []segment {
	type span struct {
		s, e int
		href string
	}
	var spans []span
	for _, f := range facets {
		if f.Index.ByteStart < 0 || f.Index.ByteEnd > len(text) || f.Index.ByteStart >= f.Index.ByteEnd {
			continue
		}
		for _, ft := range f.Features {
			href := ""
			switch ft.Type {
			case atp.FacetLink:
				if u, err := url.Parse(ft.URI); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
					href = ft.URI
				}
			case atp.FacetMention:
				href = "https://bsky.app/profile/" + url.PathEscape(ft.DID)
			case atp.FacetTag:
				href = "https://bsky.app/hashtag/" + url.PathEscape(ft.Tag)
			}
			if href != "" {
				spans = append(spans, span{f.Index.ByteStart, f.Index.ByteEnd, href})
				break
			}
		}
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].s < spans[j].s })
	var out []segment
	last := 0
	for _, sp := range spans {
		if sp.s < last {
			continue
		}
		if sp.s > last {
			out = append(out, segment{Text: text[last:sp.s]})
		}
		label := text[sp.s:sp.e]
		out = append(out, segment{Text: label, Href: sp.href})
		last = sp.e
	}
	if last < len(text) {
		out = append(out, segment{Text: text[last:]})
	}
	return out
}
