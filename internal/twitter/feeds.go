package twitter

import (
	"strconv"
	"time"
)

func timeValue(t Time) time.Time { return time.Time(t) }

// FeedInfo describes an RSS/Atom rendering of a status list.
type FeedInfo struct {
	Title       string
	Link        string // HTML page for the feed
	SelfURL     string
	Description string
	StatusURL   func(s *Status) string
}

// EncodeRSS renders statuses as RSS 2.0, as twitter.com did for timelines.
func EncodeRSS(info FeedInfo, statuses []Status) []byte {
	var w XMLWriter
	w.Header()
	w.Open("rss", "version", "2.0", "xmlns:atom", "http://www.w3.org/2005/Atom")
	w.Open("channel")
	w.Elem("title", info.Title)
	w.Elem("link", info.Link)
	w.WriteString(`<atom:link type="application/rss+xml" rel="self" href="`)
	w.Attr(info.SelfURL)
	w.WriteString(`"/>`)
	w.Elem("description", info.Description)
	w.Elem("language", "en-us")
	w.Elem("ttl", "40")
	for i := range statuses {
		s := &statuses[i]
		line := statusLine(s)
		w.Open("item")
		w.Elem("title", line)
		w.Elem("description", line)
		w.Elem("pubDate", time.Time(s.CreatedAt).UTC().Format(time.RFC1123Z))
		u := info.StatusURL(s)
		w.Elem("guid", u)
		w.Elem("link", u)
		w.Close("item")
	}
	w.Close("channel")
	w.Close("rss")
	return w.Bytes()
}

// EncodeAtom renders statuses as an Atom feed.
func EncodeAtom(info FeedInfo, statuses []Status) []byte {
	var w XMLWriter
	w.Header()
	w.Open("feed", "xmlns", "http://www.w3.org/2005/Atom", "xml:lang", "en-US")
	w.Elem("title", info.Title)
	w.Elem("id", "tag:mockingbird,2009:"+info.SelfURL)
	w.WriteString(`<link type="text/html" rel="alternate" href="`)
	w.Attr(info.Link)
	w.WriteString(`"/><link type="application/atom+xml" rel="self" href="`)
	w.Attr(info.SelfURL)
	w.WriteString(`"/>`)
	updated := time.Now().UTC()
	if len(statuses) > 0 {
		updated = time.Time(statuses[0].CreatedAt).UTC()
	}
	w.Elem("updated", updated.Format(time.RFC3339))
	w.Elem("subtitle", info.Description)
	for i := range statuses {
		s := &statuses[i]
		u := info.StatusURL(s)
		w.Open("entry")
		w.Elem("title", statusLine(s))
		w.Elem("content", statusLine(s), "type", "html")
		w.Elem("id", "tag:mockingbird,2009:"+u)
		w.Elem("published", time.Time(s.CreatedAt).UTC().Format(time.RFC3339))
		w.Elem("updated", time.Time(s.CreatedAt).UTC().Format(time.RFC3339))
		w.WriteString(`<link type="text/html" rel="alternate" href="`)
		w.Attr(u)
		w.WriteString(`"/>`)
		if s.User != nil {
			w.WriteString(`<link type="image/png" rel="image" href="`)
			w.Attr(s.User.ProfileImageURL)
			w.WriteString(`"/>`)
			w.Open("author")
			w.Elem("name", s.User.ScreenName+" ("+s.User.Name+")")
			w.Close("author")
		}
		w.Close("entry")
	}
	w.Close("feed")
	return w.Bytes()
}

func statusLine(s *Status) string {
	if s.User == nil {
		return s.Text
	}
	return s.User.ScreenName + ": " + s.Text
}

// EncodeSearchAtom renders Search API results as Atom, the format
// search.twitter.com served at /search.atom.
func EncodeSearchAtom(query, selfURL string, results []SearchResult, resultURL func(r *SearchResult) string, userURL func(screenName string) string) []byte {
	var w XMLWriter
	w.Header()
	w.Open("feed", "xmlns:google", "http://base.google.com/ns/1.0", "xml:lang", "en-US",
		"xmlns:openSearch", "http://a9.com/-/spec/opensearch/1.1/", "xmlns", "http://www.w3.org/2005/Atom",
		"xmlns:twitter", "http://api.twitter.com/")
	w.Elem("id", "tag:search.twitter.com,2005:search/"+query)
	w.WriteString(`<link type="application/atom+xml" rel="self" href="`)
	w.Attr(selfURL)
	w.WriteString(`"/>`)
	w.Elem("title", query+" - Twitter Search")
	updated := time.Now().UTC()
	if len(results) > 0 {
		updated = time.Time(results[0].CreatedAt).UTC()
	}
	w.Elem("updated", updated.Format(time.RFC3339))
	w.Elem("openSearch:itemsPerPage", strconv.Itoa(len(results)))
	for i := range results {
		r := &results[i]
		u := resultURL(r)
		w.Open("entry")
		w.Elem("id", "tag:search.twitter.com,2005:"+strconv.FormatInt(r.ID, 10))
		w.Elem("published", time.Time(r.CreatedAt).UTC().Format(time.RFC3339))
		w.WriteString(`<link type="text/html" rel="alternate" href="`)
		w.Attr(u)
		w.WriteString(`"/>`)
		w.Elem("title", r.Text)
		w.Elem("content", r.Text, "type", "html")
		w.Elem("updated", time.Time(r.CreatedAt).UTC().Format(time.RFC3339))
		w.WriteString(`<link type="image/png" rel="image" href="`)
		w.Attr(r.ProfileImageURL)
		w.WriteString(`"/>`)
		w.Elem("twitter:source", r.Source)
		w.Elem("twitter:lang", r.ISOLanguageCode)
		w.Open("author")
		w.Elem("name", r.FromUser)
		w.Elem("uri", userURL(r.FromUser))
		w.Close("author")
		w.Close("entry")
	}
	w.Close("feed")
	return w.Bytes()
}
