package translate

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rivo/uniseg"

	"github.com/jackgilbert/mockingbird/internal/atp"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// MaxGraphemes and MaxBytes are Bluesky's post text limits.
const (
	MaxGraphemes = 300
	MaxBytes     = 3000
)

// GraphemeCount counts user-perceived characters, as Bluesky does.
func GraphemeCount(s string) int { return uniseg.GraphemeClusterCount(s) }

// UnescapeClientText undoes the HTML entities some 2009 clients send back
// when quoting or retweeting text they received escaped from Twitter.
func UnescapeClientText(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	r := strings.NewReplacer("&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'", "&amp;", "&")
	return r.Replace(s)
}

// MentionResolver resolves an @mention to a DID. It is given the mention as
// typed (without "@"); it returns the full handle and DID, or ok=false.
type MentionResolver func(ctx context.Context, name string) (handle, did string, ok bool)

var (
	// Handles: dot-separated labels; a bare name is allowed and resolved later.
	mentionRE = regexp.MustCompile(`(^|[\s(\[{"'“‘])@([a-zA-Z0-9]([a-zA-Z0-9-]{0,62}[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]{0,62}[a-zA-Z0-9])?)*)`)
	// URLs with a scheme, or bare domains with a plausible TLD.
	urlRE = regexp.MustCompile(`(^|[\s(\[{"'“‘])((https?://[^\s<>"]+)|((www\.)?[a-zA-Z0-9][a-zA-Z0-9-]*(\.[a-zA-Z0-9-]+)*\.(com|net|org|io|app|social|dev|co|uk|de|fr|jp|ca|au|me|info|biz|tv|xyz|us|eu|nl|es|it|se|no|fi|br|in|ly|gl|gd|to|fm|ai|blog|news|tech|site|online)(/[^\s<>"]*)?))`)
	// Hashtags, following the Bluesky app's rule: not purely numeric, and
	// stopped by whitespace and invisible separators.
	tagRE         = regexp.MustCompile(`(^|\s)([#＃])([^\s\x{00AD}\x{2060}\x{200A}-\x{200D}\x{20e2}]*[^\d\s\p{P}\x{00AD}\x{2060}\x{200A}-\x{200D}\x{20e2}]+[^\s\x{00AD}\x{2060}\x{200A}-\x{200D}\x{20e2}]*)`)
	trailingPunct = ".,;:!?)]}'\"”’"
)

// BuildFacets detects mentions, links and hashtags in text (UTF-8 byte
// offsets). Mentions that cannot be resolved are left as plain text.
func BuildFacets(ctx context.Context, text string, resolve MentionResolver) []atp.Facet {
	var out []atp.Facet
	taken := make([]bool, len(text)+1)
	claim := func(s, e int) bool {
		for i := s; i < e; i++ {
			if taken[i] {
				return false
			}
		}
		for i := s; i < e; i++ {
			taken[i] = true
		}
		return true
	}

	for _, m := range urlRE.FindAllStringSubmatchIndex(text, -1) {
		s, e := m[4], m[5]
		raw := strings.TrimRight(text[s:e], trailingPunct)
		// Keep a closing paren that balances an opening one inside the URL.
		if strings.Count(raw, "(") > strings.Count(raw, ")") && e > s+len(raw) && text[s+len(raw)] == ')' {
			raw += ")"
		}
		e = s + len(raw)
		if !claim(s, e) {
			continue
		}
		uri := raw
		if !strings.HasPrefix(strings.ToLower(uri), "http://") && !strings.HasPrefix(strings.ToLower(uri), "https://") {
			uri = "https://" + uri
		}
		out = append(out, atp.Facet{Index: atp.FacetIndex{ByteStart: s, ByteEnd: e},
			Features: []atp.FacetFeature{{Type: atp.FacetLink, URI: uri}}})
	}

	for _, m := range mentionRE.FindAllStringSubmatchIndex(text, -1) {
		s, e := m[4]-1, m[5] // include the "@"
		name := strings.TrimRight(text[m[4]:m[5]], ".")
		e = m[4] + len(name)
		if resolve == nil {
			continue
		}
		_, did, ok := resolve(ctx, name)
		if !ok || !claim(s, e) {
			continue
		}
		out = append(out, atp.Facet{Index: atp.FacetIndex{ByteStart: s, ByteEnd: e},
			Features: []atp.FacetFeature{{Type: atp.FacetMention, DID: did}}})
	}

	for _, m := range tagRE.FindAllStringSubmatchIndex(text, -1) {
		s := m[4] // the "#" or "＃"
		tag := strings.TrimRightFunc(text[m[6]:m[7]], unicode.IsPunct)
		e := m[6] + len(tag)
		if tag == "" || utf8.RuneCountInString(tag) > 64 || !claim(s, e) {
			continue
		}
		out = append(out, atp.Facet{Index: atp.FacetIndex{ByteStart: s, ByteEnd: e},
			Features: []atp.FacetFeature{{Type: atp.FacetTag, Tag: tag}}})
	}
	sortFacets(out)
	return out
}

func sortFacets(fs []atp.Facet) {
	for i := 1; i < len(fs); i++ {
		for j := i; j > 0 && fs[j].Index.ByteStart < fs[j-1].Index.ByteStart; j-- {
			fs[j], fs[j-1] = fs[j-1], fs[j]
		}
	}
}
