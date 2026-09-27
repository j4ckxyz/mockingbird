package api

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/j4ckxyz/mockingbird/internal/atp"
	"github.com/j4ckxyz/mockingbird/internal/ident"
	"github.com/j4ckxyz/mockingbird/internal/paging"
	"github.com/j4ckxyz/mockingbird/internal/store"
	"github.com/j4ckxyz/mockingbird/internal/translate"
	"github.com/j4ckxyz/mockingbird/internal/twitter"
)

// links implements translate.Links.
type links struct{ s *Server }

func (s *Server) links() translate.Links { return links{s} }

func (l links) base() string { return l.s.cfg.PublicURL.String() }

func (l links) PostPage(id int64) string { return l.base() + "/p/" + strconv.FormatInt(id, 10) }

func (l links) Avatar(cdn string) string {
	if u := l.s.d.Signer.FromCDN(cdn, "_normal"); u != "" {
		return u
	}
	return l.base() + "/static/default_profile_normal.png"
}

func (l links) StaticURL(p string) string { return l.base() + p }

func (l links) Profile(handle string) string {
	return "https://bsky.app/profile/" + url.PathEscape(handle)
}

// detailedProfiles returns cached detailed profiles, fetching misses in
// batches of 25 with a short time budget. Counts are not viewer-specific, so
// the cache is shared by all users.
func (s *Server) detailedProfiles(ctx context.Context, c *Ctx, dids []string) map[string]*atp.Profile {
	out := make(map[string]*atp.Profile, len(dids))
	var miss []string
	for _, d := range dids {
		if p, ok := s.profiles.Get(d); ok {
			out[d] = p
		} else {
			miss = append(miss, d)
		}
	}
	if len(miss) == 0 {
		return out
	}
	sort.Strings(miss)
	deadline := time.Now().Add(3 * time.Second) // budget for all batches
	// Coalesce per viewer: the fetch runs as this viewer's session, which
	// must not be borrowed for another user's request.
	viewer := "anon"
	if c.Sess != nil {
		viewer = string(c.Sess.Key)
	}
	for i := 0; i < len(miss); i += 25 {
		batch := miss[i:min(i+25, len(miss))]
		key := "profiles:" + viewer + ":" + strings.Join(batch, ",")
		v, err, _ := s.sf.Do(key, func() (any, error) {
			// Detached, so a waiter is not failed by the first caller leaving.
			ctx, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
			defer cancel()
			var res atp.Profiles
			err := c.Caller().Do(ctx, &atp.Request{NSID: "app.bsky.actor.getProfiles", Params: url.Values{"actors": batch}}, &res)
			return res.Profiles, err
		})
		if err != nil {
			continue
		}
		for _, p := range v.([]atp.Profile) {
			p := p
			p.Viewer = nil // shared cache: strip viewer-relative state
			s.profiles.AddTTL(p.DID, &p, s.cfg.ProfileCacheTTL)
			out[p.DID] = &p
		}
	}
	return out
}

// actor turns a Twitter user reference (numeric ID, screen name, DID) into
// an actor string for Bluesky APIs.
func (c *Ctx) actor(ref string) (string, error) {
	ref = strings.TrimSpace(strings.TrimPrefix(ref, "@"))
	if ref == "" {
		return "", errNotFound()
	}
	if strings.HasPrefix(ref, "did:") {
		return ref, nil
	}
	if n, err := strconv.ParseInt(ref, 10, 64); err == nil && n > 0 {
		did, _, err := c.s.d.Store.UserByID(c.Context(), n)
		if err != nil {
			return "", errNotFound()
		}
		return did, nil
	}
	return c.fullHandle(ref), nil
}

// fullHandle expands a dotless screen name for reading: first the handle
// the viewer saw most recently, then the default host (alice ->
// alice.bsky.social). Clients truncate linked "@alice.example.com" at the
// first dot, so this is how a tapped name finds the right profile.
func (c *Ctx) fullHandle(name string) string {
	name = strings.ToLower(name)
	if strings.Contains(name, ".") {
		return name
	}
	if c.Sess != nil {
		if hs := c.s.shortNamesFor(c.Sess.DID).get(name); len(hs) > 0 {
			return hs[0]
		}
	}
	return ident.NormalizeIdentifier(name, c.s.cfg.DefaultHandleHost)
}

// writeHandle expands a dotless name for an action that reaches another
// account (following, blocking, messaging, mentioning). Recency alone is not
// enough here: anyone whose handle starts "alice." could appear in the
// viewer's timeline and capture messages meant for another alice. So the
// name must be unambiguous across the handles the viewer has seen and the
// default-host account.
func (c *Ctx) writeHandle(ctx context.Context, name string) (string, error) {
	name = strings.ToLower(name)
	if strings.Contains(name, ".") {
		return name, nil
	}
	def := ident.NormalizeIdentifier(name, c.s.cfg.DefaultHandleHost)
	var cands []string
	if c.Sess != nil {
		cands = c.s.shortNamesFor(c.Sess.DID).get(name)
	}
	if len(cands) == 0 {
		return def, nil
	}
	if !slices.Contains(cands, def) {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		who, err := c.s.d.Resolver.Resolve(rctx, def)
		cancel()
		if err == nil && who.Handle == def {
			cands = append(cands, def)
		}
	}
	if len(cands) == 1 {
		return cands[0], nil
	}
	return "", &APIError{Status: 403, Msg: fmt.Sprintf("More than one account is called @%s (%s). Use the full handle.", name, strings.Join(cands, ", "))}
}

// writeActor is actor for actions that reach another account.
func (c *Ctx) writeActor(ref string) (string, error) {
	ref = strings.TrimSpace(strings.TrimPrefix(ref, "@"))
	if ref == "" || strings.HasPrefix(ref, "did:") || strings.Contains(ref, ".") {
		return c.actor(ref)
	}
	if n, err := strconv.ParseInt(ref, 10, 64); err == nil && n > 0 {
		return c.actor(ref)
	}
	return c.writeHandle(c.Context(), ref)
}

// userArg reads the target user of a request: path :id, or id / user_id /
// screen_name / user parameters. Empty means "the authenticated user".
func (c *Ctx) userArg() string {
	if v := c.Arg("id"); v != "" {
		return v
	}
	if v := c.Arg("user_id"); v != "" {
		return v
	}
	if v := c.Arg("screen_name", "user"); v != "" {
		return strings.TrimPrefix(v, "@")
	}
	return ""
}

// targetActor returns the requested user, or the viewer.
func (c *Ctx) targetActor() (string, error) {
	ref := c.userArg()
	if ref == "" {
		if c.Sess == nil {
			return "", errUnauthorized()
		}
		return c.Sess.DID, nil
	}
	return c.actor(ref)
}

// profile fetches a detailed profile.
func (c *Ctx) profile(actor string) (*atp.Profile, error) {
	var p atp.Profile
	if err := c.Get("app.bsky.actor.getProfile", url.Values{"actor": {actor}}, &p); err != nil {
		return nil, upstreamErr(err)
	}
	cp := p
	cp.Viewer = nil
	c.s.profiles.AddTTL(p.DID, &cp, c.s.cfg.ProfileCacheTTL)
	return &p, nil
}

// statusURI resolves a status ID from the path or id parameter.
func (c *Ctx) statusURI(names ...string) (string, error) {
	if len(names) == 0 {
		names = []string{"id"}
	}
	id := c.Int64Arg(names...)
	if id <= 0 {
		return "", errNotFound()
	}
	uri, err := c.s.ids.StatusURI(c.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return "", errNotFound()
	}
	return uri, err
}

// originalPostURI maps a status key to the underlying post: a repost record
// or synthetic repost key resolves to the reposted post.
func (c *Ctx) originalPostURI(key string) (string, error) {
	if strings.HasPrefix(key, "repost:") {
		// repost:<did>:<post uri>
		rest := strings.TrimPrefix(key, "repost:")
		if i := strings.Index(rest, ":at://"); i >= 0 {
			return rest[i+1:], nil
		}
		return "", errNotFound()
	}
	if translate.CollectionFromURI(key) == "app.bsky.feed.repost" {
		if uri, ok := c.s.reposts.Get(key); ok {
			return uri, nil
		}
		// Read the repost record from the reposter's own PDS.
		var rec struct {
			Value struct {
				Subject atp.StrongRef `json:"subject"`
			} `json:"value"`
		}
		if err := c.s.publicRecord(c.Context(), key, &rec); err != nil || rec.Value.Subject.URI == "" {
			return "", errNotFound()
		}
		c.s.reposts.Add(key, rec.Value.Subject.URI)
		return rec.Value.Subject.URI, nil
	}
	return key, nil
}

// getPosts hydrates post URIs, 25 at a time, preserving order.
func (c *Ctx) getPosts(uris []string) (map[string]*atp.PostView, error) {
	out := map[string]*atp.PostView{}
	for i := 0; i < len(uris); i += 25 {
		batch := uris[i:min(i+25, len(uris))]
		var res atp.Posts
		if err := c.Get("app.bsky.feed.getPosts", url.Values{"uris": batch}, &res); err != nil {
			return nil, upstreamErr(err)
		}
		for j := range res.Posts {
			out[res.Posts[j].URI] = &res.Posts[j]
		}
	}
	return out, nil
}

// statusFor builds one status (or retweet wrapper) from a status key.
func (c *Ctx) statusFor(key string) (*twitter.Status, error) {
	postURI, err := c.originalPostURI(key)
	if err != nil {
		return nil, err
	}
	posts, err := c.getPosts([]string{postURI})
	if err != nil {
		return nil, err
	}
	p := posts[postURI]
	if p == nil {
		return nil, errNotFound()
	}
	it := translate.Item{Post: p}
	if key != postURI {
		by := atp.Profile{DID: translate.DIDFromURI(key)}
		if prof, err := c.profile(by.DID); err == nil {
			by = *prof
		}
		repostURI := ""
		if !strings.HasPrefix(key, "repost:") {
			repostURI = key
		}
		it.Repost = &atp.ReasonRepost{By: by, URI: repostURI, IndexedAt: p.IndexedAt}
		if t, err := c.statusTime(key); err == nil {
			it.Repost.IndexedAt = t.Format(time.RFC3339Nano)
		}
	}
	st, err := c.s.builder(c).Status(c.Context(), it)
	if err != nil {
		return nil, err
	}
	return &st, nil
}

func (c *Ctx) statusTime(key string) (time.Time, error) {
	ids, err := c.s.ids.StatusIDs(c.Context(), []store.StatusRef{{URI: key}})
	if err != nil {
		return time.Time{}, err
	}
	_, t, err := c.s.d.Store.StatusByID(c.Context(), ids[key])
	if t.IsZero() && err == nil {
		err = errNotFound()
	}
	return t, err
}

// timeline runs a paged feed request and renders statuses.
func (c *Ctx) timeline(feedKey string, fetch paging.Fetch, info *twitter.FeedInfo) (*Resp, error) {
	w := c.window(feedKey)
	q := paging.Query{
		Count: c.Count(20, 200), Page: c.Page(),
		Since: w.since, SinceKey: w.sinceKey, Max: w.max, MaxKey: w.maxKey, StartCursor: w.cursor,
		MaxCalls: c.s.cfg.MaxUpstreamPages,
	}
	res, err := paging.Collect(c.Context(), fetch, q)
	if err != nil {
		return nil, upstreamErr(err)
	}
	c.s.noteReposts(res.Items)
	sts, err := c.s.builder(c).Statuses(c.Context(), res.Items)
	if err != nil {
		return nil, err
	}
	if n := len(sts); n > 0 {
		c.remember(feedKey, sts[n-1].ID, res.LastCursor)
	}
	return c.statusList(sts, info), nil
}

func (c *Ctx) statusList(sts []twitter.Status, info *twitter.FeedInfo) *Resp {
	if sts == nil {
		sts = []twitter.Status{}
	}
	if info != nil && info.StatusURL == nil {
		info.StatusURL = func(st *twitter.Status) string { return links{c.s}.PostPage(st.ID) }
	}
	return &Resp{Value: sts, Root: "statuses", Item: "status", Statuses: sts, Feed: info, Cacheable: true}
}

// feedFetch adapts a feed-returning XRPC method to paging.Fetch.
func (c *Ctx) feedFetch(nsid string, params url.Values, keep func(*atp.FeedViewPost) bool) paging.Fetch {
	return func(ctx context.Context, cursor string, limit int) ([]translate.Item, string, error) {
		p := url.Values{}
		for k, v := range params {
			p[k] = v
		}
		p.Set("limit", strconv.Itoa(limit))
		if cursor != "" {
			p.Set("cursor", cursor)
		}
		var feed atp.Feed
		if err := c.Caller().Do(ctx, &atp.Request{NSID: nsid, Params: p}, &feed); err != nil {
			return nil, "", err
		}
		if keep != nil {
			kept := feed.Feed[:0]
			for i := range feed.Feed {
				if keep(&feed.Feed[i]) {
					kept = append(kept, feed.Feed[i])
				}
			}
			feed.Feed = kept
		}
		return translate.FromFeed(feed.Feed), feed.Cursor, nil
	}
}

// userResp renders a single user.
func userResp(u twitter.User) *Resp {
	return &Resp{Value: u, Root: "user", Cacheable: true}
}

func statusResp(st *twitter.Status) *Resp {
	return &Resp{Value: st, Root: "status"}
}

func boolPtr(b bool) *bool { return &b }

// noteReposts remembers which post each repost points at, so a later
// statuses/show or destroy on the retweet ID needs no record lookup.
func (s *Server) noteReposts(items []translate.Item) {
	for i := range items {
		if items[i].Repost != nil {
			s.reposts.Add(items[i].Key(), items[i].Post.URI)
		}
	}
}

// publicRecord reads a record from the PDS hosting its repo, without auth.
func (s *Server) publicRecord(ctx context.Context, uri string, out any) error {
	did := translate.DIDFromURI(uri)
	who, err := s.d.Resolver.Resolve(ctx, did)
	if err != nil {
		return err
	}
	return s.d.Sessions.Client(who.PDS).Do(ctx, &atp.Request{NSID: "com.atproto.repo.getRecord", Params: url.Values{
		"repo": {did}, "collection": {translate.CollectionFromURI(uri)}, "rkey": {translate.RkeyFromURI(uri)},
	}}, out)
}
