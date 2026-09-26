package api

import (
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackgilbert/mockingbird/internal/atp"
	"github.com/jackgilbert/mockingbird/internal/twitter"
)

// search implements search.twitter.com/search.json and .atom.
func (s *Server) search(c *Ctx) (*Resp, error) {
	start := time.Now()
	q := strings.TrimSpace(c.Form.Get("q"))
	rpp := c.Count(15, 100)
	page := c.Page()
	resp := twitter.SearchResponse{Results: []twitter.SearchResult{}, ResultsPerPage: rpp, Page: page, Query: url.QueryEscape(q)}
	resp.RefreshURL = "?since_id=0&q=" + url.QueryEscape(q)
	if q == "" {
		return s.searchResp(c, resp, start), nil
	}
	sinceID, maxID := c.Int64Arg("since_id"), c.Int64Arg("max_id")
	params := url.Values{"q": {bskyQuery(q)}, "sort": {"latest"}, "limit": {strconv.Itoa(rpp)}}
	if sinceID > 0 {
		if _, t, err := s.d.Store.StatusByID(c.Context(), sinceID); err == nil && !t.IsZero() {
			params.Set("since", t.Add(time.Millisecond).Format(time.RFC3339Nano))
		}
	}
	if maxID > 0 {
		if _, t, err := s.d.Store.StatusByID(c.Context(), maxID); err == nil && !t.IsZero() {
			params.Set("until", t.Add(time.Millisecond).Format(time.RFC3339Nano))
		}
	}
	if l := c.Form.Get("lang"); l != "" {
		params.Set("lang", l)
	}
	var posts []atp.PostView
	cursor := ""
	more := false
	for i := 1; i <= page && i <= 10; i++ {
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var res atp.Posts
		if err := c.Get("app.bsky.feed.searchPosts", params, &res); err != nil {
			if c.Sess == nil {
				// The public AppView may refuse anonymous search; old
				// clients expect an empty result, not an error.
				resp.WarningText = "Log in to search Bluesky."
				return s.searchResp(c, resp, start), nil
			}
			return nil, upstreamErr(err)
		}
		posts, cursor = res.Posts, res.Cursor
		more = cursor != ""
		if !more && i < page {
			posts = nil
			break
		}
	}
	results, err := s.builder(c).SearchResults(c.Context(), posts)
	if err != nil {
		return nil, err
	}
	if c.Form.Get("show_user") == "true" {
		for i := range results {
			results[i].Text = results[i].FromUser + ": " + results[i].Text
		}
	}
	resp.Results = results
	for _, r := range results {
		resp.MaxID = max(resp.MaxID, r.ID)
	}
	if maxID > 0 {
		resp.MaxID = maxID
	}
	resp.SinceID = sinceID
	eq := url.QueryEscape(q)
	resp.RefreshURL = "?since_id=" + strconv.FormatInt(resp.MaxID, 10) + "&q=" + eq
	if more && len(results) > 0 {
		resp.NextPage = "?page=" + strconv.Itoa(page+1) + "&max_id=" + strconv.FormatInt(resp.MaxID, 10) + "&q=" + eq
	}
	return s.searchResp(c, resp, start), nil
}

// bskyQuery translates Twitter search operators that differ.
func bskyQuery(q string) string {
	var out []string
	for _, w := range strings.Fields(q) {
		switch {
		case strings.HasPrefix(w, "from:") || strings.HasPrefix(w, "to:"):
			op, who, _ := strings.Cut(w, ":")
			if op == "to" {
				op = "mentions"
			}
			who = strings.TrimPrefix(who, "@")
			out = append(out, op+":"+who)
		case len(w) > 1 && w[0] == '@' && !strings.Contains(w, ":"):
			out = append(out, "mentions:"+w[1:])
		default:
			out = append(out, w)
		}
	}
	return strings.Join(out, " ")
}

func (s *Server) searchResp(c *Ctx, resp twitter.SearchResponse, start time.Time) *Resp {
	resp.CompletedIn = float64(time.Since(start).Microseconds()) / 1e6
	resp.MaxIDStr = strconv.FormatInt(resp.MaxID, 10)
	resp.SinceIDStr = strconv.FormatInt(resp.SinceID, 10)
	if c.Format == "atom" {
		self := s.cfg.PublicURL.String() + "/search.atom?q=" + resp.Query
		l := links{s}
		body := twitter.EncodeSearchAtom(c.Form.Get("q"), self, resp.Results,
			func(r *twitter.SearchResult) string { return l.PostPage(r.ID) },
			func(h string) string { return l.Profile(h) })
		return &Resp{Raw: body, ContentType: "application/atom+xml; charset=utf-8", Cacheable: true}
	}
	return &Resp{Value: resp, Root: "search", Cacheable: true}
}

// trendingTopics fetches Bluesky's trending topics, if the AppView offers them.
func (s *Server) trendingTopics(c *Ctx) []atp.TrendingTopic {
	var res atp.TrendingTopics
	if err := c.Get("app.bsky.unspecced.getTrendingTopics", url.Values{"limit": {"10"}}, &res); err != nil {
		return nil
	}
	return res.Topics
}

func topicQuery(t atp.TrendingTopic) string {
	if t.Topic != "" {
		return t.Topic
	}
	return t.DisplayName
}

func (s *Server) trendsSearch(c *Ctx) (*Resp, error) {
	type out struct {
		Trends []twitter.Trend `json:"trends"`
		AsOf   string          `json:"as_of"`
	}
	o := out{Trends: []twitter.Trend{}, AsOf: s.now().UTC().Format(twitter.SearchTimeLayout)}
	for _, t := range s.trendingTopics(c) {
		q := topicQuery(t)
		o.Trends = append(o.Trends, twitter.Trend{Name: q, URL: s.cfg.PublicURL.String() + "/search?q=" + url.QueryEscape(q)})
	}
	return &Resp{Value: o, Root: "trends", Cacheable: true}, nil
}

func (s *Server) trendsCurrent(c *Ctx) (*Resp, error) {
	type out struct {
		Trends map[string][]twitter.TrendQuery `json:"trends"`
		AsOf   int64                           `json:"as_of"`
	}
	now := s.now().UTC()
	list := []twitter.TrendQuery{}
	for _, t := range s.trendingTopics(c) {
		q := topicQuery(t)
		list = append(list, twitter.TrendQuery{Name: q, Query: q})
	}
	return &Resp{Value: out{Trends: map[string][]twitter.TrendQuery{now.Format("2006-01-02 15:04:05"): list}, AsOf: now.Unix()}, Root: "trends", Cacheable: true}, nil
}

// trendLocation is a Yahoo! WOEID place, as trends/available returned them.
type trendLocation struct {
	Name      string `json:"name"`
	PlaceType struct {
		Name string `json:"name"`
		Code int    `json:"code"`
	} `json:"placeType"`
	WOEID       int     `json:"woeid"`
	Country     string  `json:"country"`
	URL         string  `json:"url"`
	CountryCode *string `json:"countryCode"`
}

func worldwide() trendLocation {
	l := trendLocation{Name: "Worldwide", WOEID: 1, URL: "http://where.yahooapis.com/v1/place/1"}
	l.PlaceType.Name, l.PlaceType.Code = "Supername", 19
	return l
}

type trendLocations []trendLocation

func (ls trendLocations) WriteXML(w *twitter.XMLWriter, name string) {
	w.Open("locations", "type", "array")
	for _, l := range ls {
		w.Open("location")
		w.Elem("woeid", strconv.Itoa(l.WOEID))
		w.Elem("name", l.Name)
		w.Elem("placeTypeName", l.PlaceType.Name, "code", strconv.Itoa(l.PlaceType.Code))
		w.Elem("country", l.Country, "type", "Country", "code", "")
		w.Elem("url", l.URL)
		w.Close("location")
	}
	w.Close("locations")
}

func (s *Server) trendsAvailable(c *Ctx) (*Resp, error) {
	return &Resp{Value: trendLocations{worldwide()}, Root: "locations"}, nil
}

type locTrend struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Query string `json:"query"`
}

type locTrendBlock struct {
	CreatedAt string     `json:"created_at"`
	Trends    []locTrend `json:"trends"`
	AsOf      string     `json:"as_of"`
	Locations []struct {
		Name  string `json:"name"`
		WOEID int    `json:"woeid"`
	} `json:"locations"`
}

type locTrendBlocks []locTrendBlock

func (bs locTrendBlocks) WriteXML(w *twitter.XMLWriter, name string) {
	w.Open("matching_trends", "type", "array")
	for _, b := range bs {
		w.Open("trends", "as_of", b.AsOf, "created_at", b.CreatedAt)
		w.Open("locations")
		for _, l := range b.Locations {
			w.Open("location")
			w.Elem("woeid", strconv.Itoa(l.WOEID))
			w.Elem("name", l.Name)
			w.Close("location")
		}
		w.Close("locations")
		for _, t := range b.Trends {
			w.Elem("trend", t.Name, "query", t.Query, "url", t.URL)
		}
		w.Close("trends")
	}
	w.Close("matching_trends")
}

func (s *Server) trendsLocation(c *Ctx) (*Resp, error) {
	if c.Params["woeid"] != "1" {
		return nil, errNotFound()
	}
	now := s.now().UTC().Format("2006-01-02T15:04:05Z")
	b := locTrendBlock{Trends: []locTrend{}, AsOf: now, CreatedAt: now}
	b.Locations = append(b.Locations, struct {
		Name  string `json:"name"`
		WOEID int    `json:"woeid"`
	}{"Worldwide", 1})
	for _, t := range s.trendingTopics(c) {
		q := topicQuery(t)
		b.Trends = append(b.Trends, locTrend{Name: q, Query: url.QueryEscape(q), URL: s.cfg.PublicURL.String() + "/search?q=" + url.QueryEscape(q)})
	}
	return &Resp{Value: locTrendBlocks{b}, Root: "matching_trends", Cacheable: true}, nil
}
