package api

import (
	"math/rand/v2"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/j4ckxyz/mockingbird/internal/atp"
	"github.com/j4ckxyz/mockingbird/internal/cache"
	"github.com/j4ckxyz/mockingbird/internal/store"
	"github.com/j4ckxyz/mockingbird/internal/translate"
	"github.com/j4ckxyz/mockingbird/internal/twitter"
)

func (s *Server) userWithStatus(c *Ctx, actor string) (*Resp, error) {
	p, err := c.profile(actor)
	if err != nil {
		return nil, err
	}
	u, err := s.builder(c).User(c.Context(), p)
	if err != nil {
		return nil, err
	}
	u.Status = c.latestStatus(p.DID)
	return userResp(u), nil
}

func (s *Server) verifyCredentials(c *Ctx) (*Resp, error) {
	return s.userWithStatus(c, c.Sess.DID)
}

func (s *Server) showUser(c *Ctx) (*Resp, error) {
	actor, err := c.targetActor()
	if err != nil {
		return nil, err
	}
	return s.userWithStatus(c, actor)
}

func (s *Server) rateLimitStatus(c *Ctx) (*Resp, error) {
	limit := s.cfg.ReportedRateLimit
	used := 0
	now := s.now()
	reset := time.Unix((now.Unix()/3600+1)*3600, 0)
	if c.Sess != nil {
		used, reset = s.countRequest(c.Sess.DID)
	}
	return &Resp{Root: "hash", Value: twitter.RateLimitStatus{
		RemainingHits: limit - min(used, limit/10), HourlyLimit: limit,
		ResetTimeInSeconds: reset.Unix(), ResetTime: twitter.Time(reset),
	}}, nil
}

func (s *Server) endSession(c *Ctx) (*Resp, error) {
	c.Sess.Logout(c.Context())
	return &Resp{Root: "hash", Value: twitter.ErrorBody{Request: c.r.URL.Path, Error: "Logged out."}}, nil
}

func (s *Server) lookupUsers(c *Ctx) (*Resp, error) {
	var actors []string
	for _, id := range splitComma(c.Form.Get("user_id")) {
		if a, err := c.actor(id); err == nil {
			actors = append(actors, a)
		}
	}
	for _, sn := range splitComma(c.Form.Get("screen_name")) {
		actors = append(actors, c.fullHandle(strings.TrimPrefix(sn, "@")))
	}
	if len(actors) > 100 {
		actors = actors[:100]
	}
	var profiles []atp.Profile
	for i := 0; i < len(actors); i += 25 {
		var res atp.Profiles
		if err := c.Get("app.bsky.actor.getProfiles", url.Values{"actors": actors[i:min(i+25, len(actors))]}, &res); err != nil {
			return nil, upstreamErr(err)
		}
		profiles = append(profiles, res.Profiles...)
	}
	us, err := s.builder(c).Users(c.Context(), profiles)
	if err != nil {
		return nil, err
	}
	return usersResp(us), nil
}

func splitComma(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func usersResp(us []twitter.User) *Resp {
	if us == nil {
		us = []twitter.User{}
	}
	return &Resp{Value: us, Root: "users", Item: "user", Cacheable: true}
}

func (s *Server) searchUsers(c *Ctx) (*Resp, error) {
	q := strings.TrimSpace(c.Form.Get("q"))
	if q == "" {
		return usersResp(nil), nil
	}
	per := c.Count(20, 20)
	page := c.Page()
	cursor := ""
	var actors []atp.Profile
	for i := 1; i <= page && i <= 10; i++ {
		var res atp.Profiles
		p := url.Values{"q": {q}, "limit": {strconv.Itoa(per)}}
		if cursor != "" {
			p.Set("cursor", cursor)
		}
		if err := c.Get("app.bsky.actor.searchActors", p, &res); err != nil {
			return nil, upstreamErr(err)
		}
		actors = res.Actors
		if res.Cursor == "" && i < page {
			actors = nil
			break
		}
		cursor = res.Cursor
	}
	us, err := s.builder(c).Users(c.Context(), actors)
	if err != nil {
		return nil, err
	}
	return usersResp(us), nil
}

// cursorMap hands out numeric Twitter cursors for opaque Bluesky cursors.
type cursorMap struct {
	m *cache.LRU[int64, string]
}

var numericCursors = &cursorMap{m: cache.New[int64, string](50_000)}

func (cm *cursorMap) put(bsky string) int64 {
	if bsky == "" {
		return 0
	}
	n := rand.Int64N(1<<53-2) + 2 // positive, JavaScript-safe, never 0 or -1
	cm.m.AddTTL(n, bsky, time.Hour)
	return n
}

func (cm *cursorMap) get(n int64) (string, bool) { return cm.m.Get(n) }

// graphPage fetches follows or followers. With a Twitter cursor, one page is
// returned plus next_cursor; without, up to maxPages pages are merged.
func (c *Ctx) graphPage(nsid, actor string, perPage, maxPages int) ([]atp.Profile, int64, bool, error) {
	cursored := c.Form.Has("cursor")
	bcur := ""
	if cursored {
		if n, err := strconv.ParseInt(c.Form.Get("cursor"), 10, 64); err == nil && n > 0 {
			var ok bool
			if bcur, ok = numericCursors.get(n); !ok {
				return nil, 0, true, nil
			}
		}
		maxPages = 1
	}
	var all []atp.Profile
	for i := 0; i < maxPages; i++ {
		var res atp.Follows
		p := url.Values{"actor": {actor}, "limit": {strconv.Itoa(perPage)}}
		if bcur != "" {
			p.Set("cursor", bcur)
		}
		if err := c.Get(nsid, p, &res); err != nil {
			return nil, 0, cursored, upstreamErr(err)
		}
		all = append(all, res.Follows...)
		all = append(all, res.Followers...)
		bcur = res.Cursor
		if bcur == "" {
			break
		}
	}
	return all, numericCursors.put(bcur), cursored, nil
}

func (s *Server) usersGraph(c *Ctx, nsid string) (*Resp, error) {
	actor, err := c.targetActor()
	if err != nil {
		return nil, err
	}
	profiles, next, cursored, err := c.graphPage(nsid, actor, 100, 1)
	if err != nil {
		return nil, err
	}
	us, err := s.builder(c).Users(c.Context(), profiles)
	if err != nil {
		return nil, err
	}
	if !cursored {
		return usersResp(us), nil
	}
	if us == nil {
		us = []twitter.User{}
	}
	return &Resp{Root: "users_list", Value: twitter.UserList{Users: us, NextCursor: next, NextCursorStr: twitter.IDString(next), PreviousCursorStr: "0"}, Cacheable: true}, nil
}

func (s *Server) friendsList(c *Ctx) (*Resp, error) {
	return s.usersGraph(c, "app.bsky.graph.getFollows")
}
func (s *Server) followersList(c *Ctx) (*Resp, error) {
	return s.usersGraph(c, "app.bsky.graph.getFollowers")
}

func (s *Server) idsGraph(c *Ctx, nsid string) (*Resp, error) {
	actor, err := c.targetActor()
	if err != nil {
		return nil, err
	}
	profiles, next, cursored, err := c.graphPage(nsid, actor, 100, 10)
	if err != nil {
		return nil, err
	}
	ids, err := s.userIDList(c, profiles)
	if err != nil {
		return nil, err
	}
	if !cursored {
		return &Resp{Root: "ids", Item: "id", Value: ids, Cacheable: true}, nil
	}
	return &Resp{Root: "id_list", Value: twitter.IDList{IDs: ids, NextCursor: next, NextCursorStr: twitter.IDString(next), PreviousCursorStr: "0"}, Cacheable: true}, nil
}

func (s *Server) friendIDs(c *Ctx) (*Resp, error) { return s.idsGraph(c, "app.bsky.graph.getFollows") }
func (s *Server) followerIDs(c *Ctx) (*Resp, error) {
	return s.idsGraph(c, "app.bsky.graph.getFollowers")
}

// targetProfile resolves the request's target user to a detailed profile.
func (c *Ctx) targetProfile() (*atp.Profile, error) {
	ref := c.userArg()
	if ref == "" {
		return nil, errNotFound()
	}
	actor, err := c.actor(ref)
	if err != nil {
		return nil, err
	}
	return c.profile(actor)
}

func (s *Server) follow(c *Ctx) (*Resp, error) {
	p, err := c.targetProfile()
	if err != nil {
		return nil, err
	}
	if p.Viewer != nil && p.Viewer.Following != "" {
		return nil, errForbidden("Could not follow user: " + p.Handle + " is already on your list.")
	}
	var out atp.CreateRecordOutput
	if err := c.Post("com.atproto.repo.createRecord", map[string]any{
		"repo": c.Sess.DID, "collection": "app.bsky.graph.follow",
		"record": map[string]any{"$type": "app.bsky.graph.follow", "subject": p.DID, "createdAt": nowRecordTime()},
	}, &out); err != nil {
		return nil, upstreamErr(err)
	}
	if p.Viewer == nil {
		p.Viewer = &atp.ProfileViewer{}
	}
	p.Viewer.Following = out.URI
	u, err := s.builder(c).User(c.Context(), p)
	if err != nil {
		return nil, err
	}
	return userResp(u), nil
}

func (s *Server) unfollow(c *Ctx) (*Resp, error) {
	p, err := c.targetProfile()
	if err != nil {
		return nil, err
	}
	if p.Viewer != nil && p.Viewer.Following != "" {
		if err := c.deleteOwn(p.Viewer.Following); err != nil {
			return nil, err
		}
		p.Viewer.Following = ""
	}
	u, err := s.builder(c).User(c.Context(), p)
	if err != nil {
		return nil, err
	}
	return userResp(u), nil
}

func (s *Server) block(c *Ctx) (*Resp, error) {
	p, err := c.targetProfile()
	if err != nil {
		return nil, err
	}
	if p.Viewer == nil || p.Viewer.Blocking == "" {
		if err := c.Post("com.atproto.repo.createRecord", map[string]any{
			"repo": c.Sess.DID, "collection": "app.bsky.graph.block",
			"record": map[string]any{"$type": "app.bsky.graph.block", "subject": p.DID, "createdAt": nowRecordTime()},
		}, nil); err != nil {
			return nil, upstreamErr(err)
		}
	}
	u, err := s.builder(c).User(c.Context(), p)
	if err != nil {
		return nil, err
	}
	return userResp(u), nil
}

func (s *Server) unblock(c *Ctx) (*Resp, error) {
	p, err := c.targetProfile()
	if err != nil {
		return nil, err
	}
	if p.Viewer != nil && p.Viewer.Blocking != "" {
		if err := c.deleteOwn(p.Viewer.Blocking); err != nil {
			return nil, err
		}
	}
	u, err := s.builder(c).User(c.Context(), p)
	if err != nil {
		return nil, err
	}
	return userResp(u), nil
}

func (s *Server) blockExists(c *Ctx) (*Resp, error) {
	p, err := c.targetProfile()
	if err != nil {
		return nil, err
	}
	if p.Viewer == nil || p.Viewer.Blocking == "" {
		return nil, &APIError{Status: 404, Msg: "You are not blocking this user."}
	}
	u, err := s.builder(c).User(c.Context(), p)
	if err != nil {
		return nil, err
	}
	return userResp(u), nil
}

func (s *Server) blockedProfiles(c *Ctx) ([]atp.Profile, error) {
	var res struct {
		Blocks []atp.Profile `json:"blocks"`
	}
	if err := c.Get("app.bsky.graph.getBlocks", url.Values{"limit": {"100"}}, &res); err != nil {
		return nil, upstreamErr(err)
	}
	return res.Blocks, nil
}

func (s *Server) blocking(c *Ctx) (*Resp, error) {
	ps, err := s.blockedProfiles(c)
	if err != nil {
		return nil, err
	}
	us, err := s.builder(c).Users(c.Context(), ps)
	if err != nil {
		return nil, err
	}
	return usersResp(us), nil
}

func (s *Server) blockingIDs(c *Ctx) (*Resp, error) {
	ps, err := s.blockedProfiles(c)
	if err != nil {
		return nil, err
	}
	ids, err := s.userIDList(c, ps)
	if err != nil {
		return nil, err
	}
	return &Resp{Root: "ids", Item: "id", Value: ids}, nil
}

func (s *Server) relationshipSide(c *Ctx, b *translate.Builder, did, handle string) (twitter.RelationshipSide, error) {
	id, err := b.UserID(c.Context(), did, handle)
	return twitter.RelationshipSide{ID: id, ScreenName: handle, IDStr: twitter.IDString(id)}, err
}

func (s *Server) friendshipShow(c *Ctx) (*Resp, error) {
	srcRef := c.Arg("source_id", "source_screen_name")
	tgtRef := c.Arg("target_id", "target_screen_name")
	if tgtRef == "" {
		return nil, errNotFound()
	}
	if srcRef == "" {
		if c.Sess == nil {
			return nil, errUnauthorized()
		}
		srcRef = c.Sess.DID
	}
	src, err := c.actor(srcRef)
	if err != nil {
		return nil, err
	}
	tgt, err := c.actor(tgtRef)
	if err != nil {
		return nil, err
	}
	sp, err := c.profile(src)
	if err != nil {
		return nil, err
	}
	tp, err := c.profile(tgt)
	if err != nil {
		return nil, err
	}
	var rel atp.Relationships
	if err := c.Get("app.bsky.graph.getRelationships", url.Values{"actor": {sp.DID}, "others": {tp.DID}}, &rel); err != nil {
		return nil, upstreamErr(err)
	}
	following, followedBy, blocking := false, false, false
	for _, r := range rel.Relationships {
		if r.DID == tp.DID {
			following, followedBy, blocking = r.Following != "", r.FollowedBy != "", r.Blocking != ""
		}
	}
	b := s.builder(c)
	var out twitter.Relationship
	if out.Relationship.Source, err = s.relationshipSide(c, b, sp.DID, sp.Handle); err != nil {
		return nil, err
	}
	if out.Relationship.Target, err = s.relationshipSide(c, b, tp.DID, tp.Handle); err != nil {
		return nil, err
	}
	out.Relationship.Source.Following, out.Relationship.Source.FollowedBy = following, followedBy
	out.Relationship.Target.Following, out.Relationship.Target.FollowedBy = followedBy, following
	out.Relationship.Source.NotificationsEnabled = boolPtr(false)
	out.Relationship.Source.Blocking = boolPtr(blocking)
	out.Relationship.Source.MarkedSpam = boolPtr(false)
	out.Relationship.Source.WantRetweets = boolPtr(true)
	return &Resp{Root: "relationship", Value: xmlRelationship{out}, Cacheable: true}, nil
}

// xmlRelationship renders friendships/show: JSON wraps in "relationship",
// XML uses <relationship> as the root with source and target children.
type xmlRelationship struct{ twitter.Relationship }

func (r xmlRelationship) MarshalJSON() ([]byte, error) { return twitter.EncodeJSON(r.Relationship) }

func (r xmlRelationship) WriteXML(w *twitter.XMLWriter, name string) {
	w.Open("relationship")
	w.Value("source", reflectValue(r.Relationship.Relationship.Source), "")
	w.Value("target", reflectValue(r.Relationship.Relationship.Target), "")
	w.Close("relationship")
}

func (s *Server) friendshipExists(c *Ctx) (*Resp, error) {
	a, err := c.actor(c.Form.Get("user_a"))
	if err != nil {
		return nil, err
	}
	bRef, err := c.actor(c.Form.Get("user_b"))
	if err != nil {
		return nil, err
	}
	bp, err := c.profile(bRef)
	if err != nil {
		return nil, err
	}
	var rel atp.Relationships
	if err := c.Get("app.bsky.graph.getRelationships", url.Values{"actor": {a}, "others": {bp.DID}}, &rel); err != nil {
		return nil, upstreamErr(err)
	}
	friends := false
	for _, r := range rel.Relationships {
		if r.DID == bp.DID && r.Following != "" {
			friends = true
		}
	}
	return &Resp{Root: "friends", Value: friends}, nil
}

func nowRecordTime() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

func reflectValue(v any) reflect.Value { return reflect.ValueOf(v) }

// userIDList maps profiles to user IDs in one batch.
func (s *Server) userIDList(c *Ctx, ps []atp.Profile) ([]int64, error) {
	refs := make([]store.UserRef, len(ps))
	for i := range ps {
		refs[i] = store.UserRef{DID: ps[i].DID, Handle: ps[i].Handle}
	}
	m, err := s.ids.UserIDs(c.Context(), refs)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(ps))
	for i := range ps {
		ids = append(ids, m[ps[i].DID])
	}
	return ids, nil
}
