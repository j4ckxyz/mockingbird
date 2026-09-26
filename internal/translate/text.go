// Package translate converts Bluesky views into Twitter API objects and
// Twitter-client input into Bluesky records.
package translate

import (
	"sort"
	"strings"

	"github.com/jackgilbert/mockingbird/internal/atp"
)

// EscapeHTML escapes &, < and > the way Twitter's text field did ("escaped
// and HTML encoded status body"). Quotes are left alone, as Twitter did.
func EscapeHTML(s string) string {
	if !strings.ContainsAny(s, "&<>") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// ExpandLinks replaces the display text of link facets with their full URI.
// Bluesky clients shorten long links in the visible text ("example.com/very/lo...")
// and keep the real target in a facet; 2009 clients only see the text, so the
// real URL must be in it. Invalid or overlapping facets are ignored.
func ExpandLinks(text string, facets []atp.Facet) string {
	type repl struct {
		start, end int
		uri        string
	}
	var rs []repl
	for _, f := range facets {
		s, e := f.Index.ByteStart, f.Index.ByteEnd
		if s < 0 || e > len(text) || s >= e {
			continue
		}
		for _, feat := range f.Features {
			if feat.Type == atp.FacetLink && feat.URI != "" {
				rs = append(rs, repl{s, e, feat.URI})
				break
			}
		}
	}
	if len(rs) == 0 {
		return text
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].start < rs[j].start })
	var b strings.Builder
	last := 0
	for _, r := range rs {
		if r.start < last {
			continue // overlapping
		}
		display := text[r.start:r.end]
		b.WriteString(text[last:r.start])
		if linkMatchesDisplay(display, r.uri) {
			b.WriteString(display)
		} else {
			b.WriteString(r.uri)
		}
		last = r.end
	}
	b.WriteString(text[last:])
	return b.String()
}

// linkMatchesDisplay reports whether the visible text already is the full
// link (so a scheme-less "example.com" stays as typed only when it is exact).
func linkMatchesDisplay(display, uri string) bool {
	if display == uri {
		return true
	}
	return false
}

// Links builds bridge URLs used inside translated text and objects.
type Links interface {
	// PostPage is the lightweight HTML page for a status.
	PostPage(statusID int64) string
	// Avatar is the signed avatar URL at Twitter's _normal size, or a default
	// image when cdnURL is empty or not a Bluesky CDN URL.
	Avatar(cdnURL string) string
	// StaticURL is a bridge static asset.
	StaticURL(path string) string
	// Profile is the web profile page for a handle (for RSS links).
	Profile(handle string) string
}

// PostText renders a post's text for a Twitter client: links expanded, embeds
// represented as links, then HTML-escaped.
//
// ownPage is the bridge page for this post (used for image and video
// embeds); quotePage returns the bridge page for a quoted post URI, or ""
// if the quote is unavailable.
func PostText(rec *atp.PostRecord, embed *atp.EmbedView, ownPage string, quotePage func(uri string) string) string {
	text := ExpandLinks(rec.Text, rec.Facets)
	var extra []string
	addMedia := func(e *atp.EmbedView) {
		if e == nil {
			return
		}
		switch e.Type {
		case atp.EmbedImagesView, atp.EmbedVideoView, atp.EmbedGalleryView:
			extra = append(extra, ownPage)
		case atp.EmbedExternalView:
			if e.External != nil && e.External.URI != "" && !strings.Contains(text, e.External.URI) {
				extra = append(extra, e.External.URI)
			}
		}
	}
	if embed != nil {
		switch embed.Type {
		case atp.EmbedRecordView:
			if r := embed.Record(); r != nil && r.Type == atp.RecordViewRecord {
				if u := quotePage(r.URI); u != "" {
					extra = append(extra, u)
				}
			}
		case atp.EmbedRecordWithMediaView:
			addMedia(embed.Media)
			if r := embed.Record(); r != nil && r.Type == atp.RecordViewRecord {
				if u := quotePage(r.URI); u != "" {
					extra = append(extra, u)
				}
			}
		default:
			addMedia(embed)
		}
	} else if len(rec.Embed) > 0 {
		// Views are missing (e.g. notification records): fall back to the
		// record's own embed so images are at least signposted.
		var raw struct {
			Type string `json:"$type"`
		}
		if jsonUnmarshal(rec.Embed, &raw) == nil && strings.HasPrefix(raw.Type, "app.bsky.embed.") && raw.Type != "app.bsky.embed.external" {
			extra = append(extra, ownPage)
		}
	}
	for _, e := range extra {
		if text == "" {
			text = e
		} else {
			text += " " + e
		}
	}
	return EscapeHTML(text)
}
