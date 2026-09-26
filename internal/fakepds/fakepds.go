// Package fakepds is an in-memory stand-in for a PDS plus AppView, used by
// tests and the load-test tool. It implements just enough XRPC for the bridge.
package fakepds

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// Account is a fake user.
type Account struct {
	DID         string
	Handle      string
	DisplayName string
	AppPassword string // accepted with scope com.atproto.appPass
	DMPassword  string // accepted with scope com.atproto.appPassPrivileged
	Password    string // full account password (should be rejected by the bridge)
	Follows     []string
	Labels      []string // self-labels such as "!no-unauthenticated"
}

// Post is a fake post.
type Post struct {
	URI       string
	CID       string
	Author    string // DID
	Text      string
	Facets    []map[string]any
	Reply     map[string]any
	Embed     map[string]any // record embed
	EmbedView map[string]any // hydrated view
	CreatedAt time.Time
	IndexedAt time.Time
	Likes     map[string]string // liker DID -> like URI
	Reposts   map[string]string // reposter DID -> repost URI
}

type repostEvent struct {
	by   string
	uri  string
	post *Post
	at   time.Time
}

// Server is the fake. Its zero value is not usable; call New.
type Server struct {
	mu       sync.RWMutex
	accounts map[string]*Account // by DID
	byHandle map[string]*Account
	posts    map[string]*Post // by URI
	order    []*Post          // newest first
	follows  map[string]map[string]string
	blocks   map[string]map[string]string
	tokens   map[string]string // refresh token -> DID
	reposts  []repostEvent
	seq      atomic.Int64

	// Latency is added to every request (load testing).
	Latency time.Duration
	// Calls counts requests by NSID.
	callMu sync.Mutex
	Calls  map[string]int
	// URL is the server's base URL, set by the caller after starting it.
	URL string
	// AccessTTL controls issued access token lifetime.
	AccessTTL time.Duration
}

// New returns an empty fake.
func New() *Server {
	return &Server{
		accounts: map[string]*Account{}, byHandle: map[string]*Account{}, posts: map[string]*Post{},
		follows: map[string]map[string]string{}, blocks: map[string]map[string]string{},
		tokens: map[string]string{}, Calls: map[string]int{}, AccessTTL: time.Hour,
	}
}

// AddAccount registers an account.
func (s *Server) AddAccount(a *Account) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts[a.DID] = a
	s.byHandle[a.Handle] = a
	for _, f := range a.Follows {
		s.followLocked(a.DID, f)
	}
}

func (s *Server) followLocked(from, to string) string {
	if s.follows[from] == nil {
		s.follows[from] = map[string]string{}
	}
	uri := fmt.Sprintf("at://%s/app.bsky.graph.follow/%s", from, s.tid())
	s.follows[from][to] = uri
	return uri
}

func (s *Server) tid() string {
	return fmt.Sprintf("3l%011d", s.seq.Add(1))
}

// AddPost inserts a post and returns it.
func (s *Server) AddPost(author, text string, at time.Time) *Post {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addPostLocked(author, text, at, nil, nil, nil)
}

func (s *Server) addPostLocked(author, text string, at time.Time, facets []map[string]any, reply, embed map[string]any) *Post {
	rkey := s.tid()
	p := &Post{
		URI: fmt.Sprintf("at://%s/app.bsky.feed.post/%s", author, rkey), CID: "bafyrei" + rkey,
		Author: author, Text: text, Facets: facets, Reply: reply, Embed: embed, CreatedAt: at, IndexedAt: at.Add(time.Second),
		Likes: map[string]string{}, Reposts: map[string]string{},
	}
	s.posts[p.URI] = p
	s.order = append(s.order, p)
	sort.SliceStable(s.order, func(i, j int) bool { return s.order[i].IndexedAt.After(s.order[j].IndexedAt) })
	return p
}

// SetEmbedView attaches a hydrated embed view to a post (tests).
func (s *Server) SetEmbedView(uri string, view map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.posts[uri]; p != nil {
		p.EmbedView = view
	}
}

// Directory returns an identity directory resolving every account to this server.
func (s *Server) Directory() identity.Directory {
	d := identity.NewMockDirectory()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.accounts {
		d.Insert(identity.Identity{
			DID:    syntax.DID(a.DID),
			Handle: syntax.Handle(a.Handle),
			Services: map[string]identity.ServiceEndpoint{
				"atproto_pds": {Type: "AtprotoPersonalDataServer", URL: s.URL},
			},
		})
	}
	return d
}

// Posts returns a snapshot of all posts, newest first.
func (s *Server) Posts() []*Post {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]*Post(nil), s.order...)
}

// Post returns a post by URI.
func (s *Server) Post(uri string) *Post {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.posts[uri]
}

// CallCount returns how many times nsid was called.
func (s *Server) CallCount(nsid string) int {
	s.callMu.Lock()
	defer s.callMu.Unlock()
	return s.Calls[nsid]
}

func jwt(claims map[string]any) string {
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"typ":"at+jwt","alg":"HS256"}`))
	b, _ := json.Marshal(claims)
	return h + "." + base64.RawURLEncoding.EncodeToString(b) + ".c2ln"
}

func xerr(w http.ResponseWriter, status int, name, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": name, "message": msg})
}

func ok(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

// ServeHTTP implements the XRPC surface.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.Latency > 0 {
		time.Sleep(s.Latency)
	}
	if strings.HasPrefix(r.URL.Path, "/img/") {
		// Stand-in for the Bluesky CDN.
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(fakeJPEG())
		return
	}
	nsid := strings.TrimPrefix(r.URL.Path, "/xrpc/")
	s.callMu.Lock()
	s.Calls[nsid]++
	s.callMu.Unlock()

	switch nsid {
	case "com.atproto.server.createSession":
		s.createSession(w, r)
		return
	case "com.atproto.server.refreshSession":
		s.refreshSession(w, r)
		return
	}
	viewer, scope, okAuth := s.auth(r)
	if !okAuth && r.Header.Get("Authorization") == "" && r.Method == http.MethodGet && strings.HasPrefix(nsid, "app.bsky.") && nsid != "app.bsky.feed.getTimeline" {
		viewer, okAuth = "", true // public AppView style anonymous read
	}
	if !okAuth {
		xerr(w, 401, "InvalidToken", "bad token")
		return
	}
	if nsid == "com.atproto.server.deleteSession" {
		w.WriteHeader(200)
		return
	}
	if nsid == "com.atproto.server.listAppPasswords" {
		if scope != "com.atproto.access" {
			xerr(w, 400, "InvalidToken", "Bad token scope")
			return
		}
		ok(w, map[string]any{"passwords": []any{}})
		return
	}
	if strings.HasPrefix(nsid, "chat.bsky.") {
		if scope != "com.atproto.appPassPrivileged" && scope != "com.atproto.access" {
			xerr(w, 400, "InvalidToken", "Bad token scope")
			return
		}
		s.chat(w, r, nsid, viewer)
		return
	}
	q := r.URL.Query()
	switch nsid {
	case "app.bsky.feed.getTimeline":
		s.timeline(w, q, viewer)
	case "app.bsky.feed.getAuthorFeed":
		actor := s.resolveActor(q.Get("actor"))
		s.feed(w, q, func(p *Post) bool { return p.Author == actor }, viewer)
	case "app.bsky.feed.getActorLikes":
		actor := s.resolveActor(q.Get("actor"))
		if actor != viewer {
			xerr(w, 400, "InvalidRequest", "Profile not found")
			return
		}
		s.feed(w, q, func(p *Post) bool { return p.Likes[viewer] != "" }, viewer)
	case "app.bsky.feed.getFeed":
		s.feed(w, q, func(p *Post) bool { return true }, viewer)
	case "app.bsky.feed.getPosts":
		s.getPosts(w, q["uris"], viewer)
	case "app.bsky.feed.searchPosts":
		term := strings.ToLower(q.Get("q"))
		s.searchPosts(w, q, func(p *Post) bool { return strings.Contains(strings.ToLower(p.Text), term) }, viewer)
	case "app.bsky.actor.getProfile":
		a := s.account(s.resolveActor(q.Get("actor")))
		if a == nil {
			xerr(w, 400, "InvalidRequest", "Profile not found")
			return
		}
		ok(w, s.profile(a, viewer, true))
	case "app.bsky.actor.getProfiles":
		var out []any
		for _, act := range q["actors"] {
			if a := s.account(s.resolveActor(act)); a != nil {
				out = append(out, s.profile(a, viewer, true))
			}
		}
		ok(w, map[string]any{"profiles": out})
	case "app.bsky.actor.searchActors":
		term := strings.ToLower(q.Get("q"))
		var out []any
		s.mu.RLock()
		for _, a := range s.sortedAccounts() {
			if strings.Contains(a.Handle, term) || strings.Contains(strings.ToLower(a.DisplayName), term) {
				out = append(out, s.profile(a, viewer, false))
			}
		}
		s.mu.RUnlock()
		ok(w, map[string]any{"actors": out})
	case "app.bsky.graph.getFollows", "app.bsky.graph.getFollowers":
		s.graph(w, q, nsid, viewer)
	case "app.bsky.graph.getRelationships":
		actor := s.resolveActor(q.Get("actor"))
		var rels []any
		s.mu.RLock()
		for _, o := range q["others"] {
			od := s.resolveActor(o)
			rel := map[string]any{"$type": "app.bsky.graph.defs#relationship", "did": od}
			if u := s.follows[actor][od]; u != "" {
				rel["following"] = u
			}
			if u := s.follows[od][actor]; u != "" {
				rel["followedBy"] = u
			}
			rels = append(rels, rel)
		}
		s.mu.RUnlock()
		ok(w, map[string]any{"actor": actor, "relationships": rels})
	case "app.bsky.feed.getRepostedBy":
		p := s.Post(q.Get("uri"))
		var out []any
		if p != nil {
			s.mu.RLock()
			for did := range p.Reposts {
				if a := s.accounts[did]; a != nil {
					out = append(out, s.profile(a, viewer, false))
				}
			}
			s.mu.RUnlock()
		}
		ok(w, map[string]any{"uri": q.Get("uri"), "repostedBy": out})
	case "app.bsky.graph.getBlocks":
		var out []any
		s.mu.RLock()
		for _, a := range s.sortedAccounts() {
			if s.blocks[viewer][a.DID] != "" {
				out = append(out, s.profile(a, viewer, false))
			}
		}
		s.mu.RUnlock()
		if out == nil {
			out = []any{}
		}
		ok(w, map[string]any{"blocks": out})
	case "app.bsky.notification.listNotifications":
		s.notifications(w, q, viewer)
	case "app.bsky.unspecced.getTrendingTopics":
		ok(w, map[string]any{"topics": []any{
			map[string]any{"topic": "Bluesky", "link": "/search?q=Bluesky"},
			map[string]any{"topic": "#caturday", "link": "/hashtag/caturday"},
		}})
	case "com.atproto.repo.createRecord":
		s.createRecord(w, r, viewer)
	case "com.atproto.repo.deleteRecord":
		s.deleteRecord(w, r, viewer)
	case "com.atproto.repo.uploadBlob":
		b, _ := io.ReadAll(r.Body)
		ok(w, map[string]any{"blob": map[string]any{"$type": "blob", "ref": map[string]string{"$link": fmt.Sprintf("bafkrei%010d", len(b))}, "mimeType": r.Header.Get("Content-Type"), "size": len(b)}})
	default:
		xerr(w, 501, "MethodNotImplemented", nsid)
	}
}

func (s *Server) sortedAccounts() []*Account {
	out := make([]*Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Handle < out[j].Handle })
	return out
}

func (s *Server) account(did string) *Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.accounts[did]
}

func (s *Server) resolveActor(a string) string {
	if strings.HasPrefix(a, "did:") {
		return a
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if acc := s.byHandle[a]; acc != nil {
		return acc.DID
	}
	return a
}

func (s *Server) auth(r *http.Request) (did, scope string, ok bool) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return "", "", false
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", false
	}
	var c struct {
		Sub   string `json:"sub"`
		Scope string `json:"scope"`
		Exp   int64  `json:"exp"`
	}
	if json.Unmarshal(b, &c) != nil || c.Scope == "com.atproto.refresh" {
		return "", "", false
	}
	if time.Now().Unix() > c.Exp {
		return "", "", false
	}
	return c.Sub, c.Scope, s.account(c.Sub) != nil
}

func (s *Server) issue(w http.ResponseWriter, a *Account, scope string) {
	refresh := jwt(map[string]any{"sub": a.DID, "scope": "com.atproto.refresh", "jti": s.tid()})
	s.mu.Lock()
	s.tokens[refresh] = a.DID + " " + scope
	s.mu.Unlock()
	ok(w, map[string]any{
		"did": a.DID, "handle": a.Handle, "active": true,
		"accessJwt":  jwt(map[string]any{"sub": a.DID, "scope": scope, "exp": time.Now().Add(s.AccessTTL).Unix()}),
		"refreshJwt": refresh,
	})
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var in struct{ Identifier, Password string }
	json.NewDecoder(r.Body).Decode(&in)
	a := s.account(s.resolveActor(in.Identifier))
	if a == nil {
		xerr(w, 401, "AuthenticationRequired", "Invalid identifier or password")
		return
	}
	switch {
	case a.AppPassword != "" && in.Password == a.AppPassword:
		s.issue(w, a, "com.atproto.appPass")
	case a.DMPassword != "" && in.Password == a.DMPassword:
		s.issue(w, a, "com.atproto.appPassPrivileged")
	case a.Password != "" && in.Password == a.Password:
		s.issue(w, a, "com.atproto.access")
	default:
		xerr(w, 401, "AuthenticationRequired", "Invalid identifier or password")
	}
}

func (s *Server) refreshSession(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	v, found := s.tokens[tok]
	delete(s.tokens, tok) // rotate
	s.mu.Unlock()
	if !found {
		xerr(w, 400, "ExpiredToken", "Token has been revoked")
		return
	}
	did, scope, _ := strings.Cut(v, " ")
	s.issue(w, s.account(did), scope)
}

// RevokeAll invalidates every refresh token (simulates app password revocation).
func (s *Server) RevokeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = map[string]string{}
}

func (s *Server) profile(a *Account, viewer string, detailed bool) map[string]any {
	p := map[string]any{
		"did": a.DID, "handle": a.Handle, "displayName": a.DisplayName,
		"avatar":    "https://cdn.bsky.app/img/avatar/plain/" + a.DID + "/bafkreiavatar@jpeg",
		"createdAt": "2023-04-01T12:00:00.000Z",
	}
	if len(a.Labels) > 0 {
		var ls []any
		for _, l := range a.Labels {
			ls = append(ls, map[string]any{"src": a.DID, "val": l})
		}
		p["labels"] = ls
	}
	v := map[string]any{}
	if u := s.follows[viewer][a.DID]; u != "" {
		v["following"] = u
	}
	if u := s.follows[a.DID][viewer]; u != "" {
		v["followedBy"] = u
	}
	if u := s.blocks[viewer][a.DID]; u != "" {
		v["blocking"] = u
	}
	p["viewer"] = v
	if detailed {
		followers := 0
		for _, f := range s.follows {
			if f[a.DID] != "" {
				followers++
			}
		}
		posts := 0
		for _, po := range s.posts {
			if po.Author == a.DID {
				posts++
			}
		}
		p["followersCount"] = followers
		p["followsCount"] = len(s.follows[a.DID])
		p["postsCount"] = posts
		p["description"] = "Fake account " + a.Handle
	}
	return p
}

func (s *Server) postView(p *Post, viewer string) map[string]any {
	rec := map[string]any{"$type": "app.bsky.feed.post", "text": p.Text, "createdAt": p.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z")}
	if p.Facets != nil {
		rec["facets"] = p.Facets
	}
	if p.Reply != nil {
		rec["reply"] = p.Reply
	}
	if p.Embed != nil {
		rec["embed"] = p.Embed
	}
	v := map[string]any{
		"uri": p.URI, "cid": p.CID, "author": s.profile(s.accounts[p.Author], viewer, false), "record": rec,
		"replyCount": 0, "repostCount": len(p.Reposts), "likeCount": len(p.Likes), "quoteCount": 0,
		"indexedAt": p.IndexedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
	}
	if p.EmbedView != nil {
		v["embed"] = p.EmbedView
	}
	pv := map[string]any{}
	if u := p.Likes[viewer]; u != "" {
		pv["like"] = u
	}
	if u := p.Reposts[viewer]; u != "" {
		pv["repost"] = u
	}
	v["viewer"] = pv
	return v
}

func (s *Server) feedItem(p *Post, viewer string) map[string]any {
	item := map[string]any{"post": s.postView(p, viewer)}
	if p.Reply != nil {
		par, _ := p.Reply["parent"].(map[string]any)
		root, _ := p.Reply["root"].(map[string]any)
		if pp := s.posts[str(par["uri"])]; pp != nil {
			rootView := s.postView(pp, viewer)
			if rp := s.posts[str(root["uri"])]; rp != nil {
				rootView = s.postView(rp, viewer)
			}
			pview := s.postView(pp, viewer)
			pview["$type"] = "app.bsky.feed.defs#postView"
			rootView["$type"] = "app.bsky.feed.defs#postView"
			item["reply"] = map[string]any{"parent": pview, "root": rootView}
		}
	}
	return item
}

func str(v any) string { s, _ := v.(string); return s }

func pageArgs(q map[string][]string) (offset, limit int) {
	limit = 50
	if l, err := strconv.Atoi(first(q["limit"])); err == nil && l > 0 {
		limit = min(l, 100)
	}
	offset, _ = strconv.Atoi(first(q["cursor"]))
	return
}

func first(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

func (s *Server) feed(w http.ResponseWriter, q map[string][]string, keep func(*Post) bool, viewer string) {
	offset, limit := pageArgs(q)
	s.mu.RLock()
	defer s.mu.RUnlock()
	var items []any
	i := 0
	next := ""
	for idx, p := range s.order {
		if !keep(p) {
			continue
		}
		if i < offset {
			i++
			continue
		}
		if len(items) == limit {
			next = strconv.Itoa(offset + limit)
			break
		}
		items = append(items, s.feedItem(p, viewer))
		i++
		_ = idx
	}
	if items == nil {
		items = []any{}
	}
	out := map[string]any{"feed": items}
	if next != "" {
		out["cursor"] = next
	}
	ok(w, out)
}

func (s *Server) searchPosts(w http.ResponseWriter, q map[string][]string, keep func(*Post) bool, viewer string) {
	offset, limit := pageArgs(q)
	s.mu.RLock()
	defer s.mu.RUnlock()
	var posts []any
	i := 0
	next := ""
	for _, p := range s.order {
		if !keep(p) {
			continue
		}
		if i < offset {
			i++
			continue
		}
		if len(posts) == limit {
			next = strconv.Itoa(offset + limit)
			break
		}
		posts = append(posts, s.postView(p, viewer))
		i++
	}
	if posts == nil {
		posts = []any{}
	}
	out := map[string]any{"posts": posts}
	if next != "" {
		out["cursor"] = next
	}
	ok(w, out)
}

func (s *Server) getPosts(w http.ResponseWriter, uris []string, viewer string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	posts := []any{}
	for _, u := range uris {
		if p := s.posts[u]; p != nil {
			posts = append(posts, s.postView(p, viewer))
		}
	}
	ok(w, map[string]any{"posts": posts})
}

func (s *Server) graph(w http.ResponseWriter, q map[string][]string, nsid, viewer string) {
	actor := s.resolveActor(first(q["actor"]))
	s.mu.RLock()
	defer s.mu.RUnlock()
	var list []any
	for _, a := range s.sortedAccounts() {
		if nsid == "app.bsky.graph.getFollows" && s.follows[actor][a.DID] != "" ||
			nsid == "app.bsky.graph.getFollowers" && s.follows[a.DID][actor] != "" {
			list = append(list, s.profile(a, viewer, false))
		}
	}
	offset, limit := pageArgs(q)
	next := ""
	if offset > len(list) {
		offset = len(list)
	}
	list = list[offset:]
	if len(list) > limit {
		list = list[:limit]
		next = strconv.Itoa(offset + limit)
	}
	if list == nil {
		list = []any{}
	}
	key := "follows"
	if nsid == "app.bsky.graph.getFollowers" {
		key = "followers"
	}
	out := map[string]any{key: list, "subject": s.profile(s.accounts[actor], viewer, false)}
	if next != "" {
		out["cursor"] = next
	}
	ok(w, out)
}

func (s *Server) notifications(w http.ResponseWriter, q map[string][]string, viewer string) {
	reasons := map[string]bool{}
	for _, r := range q["reasons"] {
		reasons[r] = true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var notes []any
	for _, p := range s.order {
		reason := ""
		subject := ""
		if p.Author == viewer {
			// reposts of my posts
			for did, ruri := range p.Reposts {
				if (len(reasons) == 0 || reasons["repost"]) && did != viewer {
					notes = append(notes, map[string]any{"uri": ruri, "cid": "bafyrepost", "author": s.profile(s.accounts[did], viewer, false),
						"reason": "repost", "reasonSubject": p.URI, "record": map[string]any{"$type": "app.bsky.feed.repost"}, "isRead": false,
						"indexedAt": p.IndexedAt.Add(time.Minute).UTC().Format(time.RFC3339)})
				}
			}
			continue
		}
		if p.Reply != nil {
			par, _ := p.Reply["parent"].(map[string]any)
			if pp := s.posts[str(par["uri"])]; pp != nil && pp.Author == viewer {
				reason, subject = "reply", pp.URI
			}
		}
		if reason == "" {
			for _, f := range p.Facets {
				for _, feat := range f["features"].([]any) {
					fm := feat.(map[string]any)
					if fm["did"] == viewer {
						reason = "mention"
					}
				}
			}
		}
		if reason == "" || (len(reasons) > 0 && !reasons[reason]) {
			continue
		}
		n := map[string]any{"uri": p.URI, "cid": p.CID, "author": s.profile(s.accounts[p.Author], viewer, false),
			"reason": reason, "record": map[string]any{"$type": "app.bsky.feed.post", "text": p.Text, "createdAt": p.CreatedAt.UTC().Format(time.RFC3339)},
			"isRead": false, "indexedAt": p.IndexedAt.UTC().Format(time.RFC3339)}
		if subject != "" {
			n["reasonSubject"] = subject
		}
		notes = append(notes, n)
	}
	offset, limit := pageArgs(q)
	next := ""
	if offset > len(notes) {
		offset = len(notes)
	}
	notes = notes[offset:]
	if len(notes) > limit {
		notes = notes[:limit]
		next = strconv.Itoa(offset + limit)
	}
	if notes == nil {
		notes = []any{}
	}
	out := map[string]any{"notifications": notes}
	if next != "" {
		out["cursor"] = next
	}
	ok(w, out)
}

func (s *Server) createRecord(w http.ResponseWriter, r *http.Request, viewer string) {
	var in struct {
		Repo       string         `json:"repo"`
		Collection string         `json:"collection"`
		Record     map[string]any `json:"record"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Repo != viewer {
		xerr(w, 400, "InvalidRequest", "bad record")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch in.Collection {
	case "app.bsky.feed.post":
		text, _ := in.Record["text"].(string)
		var facets []map[string]any
		if fs, ok := in.Record["facets"].([]any); ok {
			for _, f := range fs {
				facets = append(facets, f.(map[string]any))
			}
		}
		reply, _ := in.Record["reply"].(map[string]any)
		embed, _ := in.Record["embed"].(map[string]any)
		p := s.addPostLocked(viewer, text, time.Now(), facets, reply, embed)
		ok(w, map[string]string{"uri": p.URI, "cid": p.CID})
	case "app.bsky.feed.like", "app.bsky.feed.repost":
		subj, _ := in.Record["subject"].(map[string]any)
		p := s.posts[str(subj["uri"])]
		if p == nil {
			xerr(w, 400, "InvalidRequest", "subject not found")
			return
		}
		uri := fmt.Sprintf("at://%s/%s/%s", viewer, in.Collection, s.tid())
		if in.Collection == "app.bsky.feed.like" {
			p.Likes[viewer] = uri
		} else {
			p.Reposts[viewer] = uri
			s.reposts = append(s.reposts, repostEvent{by: viewer, uri: uri, post: p, at: time.Now()})
		}
		ok(w, map[string]string{"uri": uri, "cid": "bafyrec"})
	case "app.bsky.graph.follow":
		subj, _ := in.Record["subject"].(string)
		uri := s.followLocked(viewer, subj)
		ok(w, map[string]string{"uri": uri, "cid": "bafyrec"})
	case "app.bsky.graph.block":
		subj, _ := in.Record["subject"].(string)
		if s.blocks[viewer] == nil {
			s.blocks[viewer] = map[string]string{}
		}
		uri := fmt.Sprintf("at://%s/app.bsky.graph.block/%s", viewer, s.tid())
		s.blocks[viewer][subj] = uri
		ok(w, map[string]string{"uri": uri, "cid": "bafyrec"})
	default:
		xerr(w, 400, "InvalidRequest", "unsupported collection")
	}
}

func (s *Server) deleteRecord(w http.ResponseWriter, r *http.Request, viewer string) {
	var in struct {
		Repo, Collection, Rkey string
	}
	json.NewDecoder(r.Body).Decode(&in)
	if in.Repo != viewer {
		xerr(w, 400, "InvalidRequest", "not your repo")
		return
	}
	uri := fmt.Sprintf("at://%s/%s/%s", in.Repo, in.Collection, in.Rkey)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch in.Collection {
	case "app.bsky.feed.post":
		delete(s.posts, uri)
		for i, p := range s.order {
			if p.URI == uri {
				s.order = append(s.order[:i], s.order[i+1:]...)
				break
			}
		}
	case "app.bsky.feed.like", "app.bsky.feed.repost":
		for _, p := range s.posts {
			for k, v := range p.Likes {
				if v == uri {
					delete(p.Likes, k)
				}
			}
			for k, v := range p.Reposts {
				if v == uri {
					delete(p.Reposts, k)
				}
			}
		}
	case "app.bsky.graph.follow":
		for k, v := range s.follows[viewer] {
			if v == uri {
				delete(s.follows[viewer], k)
			}
		}
	case "app.bsky.graph.block":
		for k, v := range s.blocks[viewer] {
			if v == uri {
				delete(s.blocks[viewer], k)
			}
		}
	}
	ok(w, map[string]any{})
}

// Chat: one convo per pair of users, messages kept in memory.
type chatMsg struct {
	id, sender, text string
	sent             time.Time
	deletedFor       map[string]bool
}

var (
	chatMu   sync.Mutex
	chatLogs = map[*Server]map[string][]*chatMsg{}
)

func convoID(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return "convo-" + strings.TrimPrefix(a, "did:plc:") + "-" + strings.TrimPrefix(b, "did:plc:")
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request, nsid, viewer string) {
	chatMu.Lock()
	logs := chatLogs[s]
	if logs == nil {
		logs = map[string][]*chatMsg{}
		chatLogs[s] = logs
	}
	chatMu.Unlock()
	msgView := func(m *chatMsg) map[string]any {
		return map[string]any{"$type": "chat.bsky.convo.defs#messageView", "id": m.id, "rev": m.id, "text": m.text,
			"sender": map[string]string{"did": m.sender}, "sentAt": m.sent.UTC().Format(time.RFC3339Nano)}
	}
	members := func(id string) []any {
		var out []any
		for _, a := range s.sortedAccounts() {
			if strings.Contains(id, strings.TrimPrefix(a.DID, "did:plc:")) {
				out = append(out, s.profile(a, viewer, false))
			}
		}
		return out
	}
	switch nsid {
	case "chat.bsky.convo.listConvos":
		var convos []any
		chatMu.Lock()
		for id, msgs := range logs {
			if strings.Contains(id, strings.TrimPrefix(viewer, "did:plc:")) && len(msgs) > 0 {
				convos = append(convos, map[string]any{"id": id, "rev": "1", "members": members(id), "unreadCount": 0, "muted": false,
					"lastMessage": msgView(msgs[len(msgs)-1])})
			}
		}
		chatMu.Unlock()
		if convos == nil {
			convos = []any{}
		}
		ok(w, map[string]any{"convos": convos})
	case "chat.bsky.convo.getMessages":
		id := r.URL.Query().Get("convoId")
		var out []any
		chatMu.Lock()
		msgs := logs[id]
		for i := len(msgs) - 1; i >= 0; i-- {
			if !msgs[i].deletedFor[viewer] {
				out = append(out, msgView(msgs[i]))
			}
		}
		chatMu.Unlock()
		if out == nil {
			out = []any{}
		}
		ok(w, map[string]any{"messages": out})
	case "chat.bsky.convo.getConvoForMembers":
		ms := r.URL.Query()["members"]
		other := viewer
		for _, m := range ms {
			if m != viewer {
				other = m
			}
		}
		id := convoID(viewer, other)
		ok(w, map[string]any{"convo": map[string]any{"id": id, "rev": "1", "members": members(id), "unreadCount": 0, "muted": false}})
	case "chat.bsky.convo.sendMessage":
		var in struct {
			ConvoID string `json:"convoId"`
			Message struct {
				Text string `json:"text"`
			} `json:"message"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		m := &chatMsg{id: s.tid(), sender: viewer, text: in.Message.Text, sent: time.Now(), deletedFor: map[string]bool{}}
		chatMu.Lock()
		logs[in.ConvoID] = append(logs[in.ConvoID], m)
		chatMu.Unlock()
		ok(w, msgView(m))
	case "chat.bsky.convo.deleteMessageForSelf":
		var in struct {
			ConvoID   string `json:"convoId"`
			MessageID string `json:"messageId"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		chatMu.Lock()
		for _, m := range logs[in.ConvoID] {
			if m.id == in.MessageID {
				m.deletedFor[viewer] = true
				chatMu.Unlock()
				ok(w, map[string]any{"$type": "chat.bsky.convo.defs#deletedMessageView", "id": m.id, "rev": m.id,
					"sender": map[string]string{"did": m.sender}, "sentAt": m.sent.UTC().Format(time.RFC3339Nano)})
				return
			}
		}
		chatMu.Unlock()
		xerr(w, 400, "InvalidRequest", "message not found")
	default:
		xerr(w, 501, "MethodNotImplemented", nsid)
	}
}

// Seed creates n accounts named user0.test..., each following the next
// `follows` accounts and holding `posts` posts, for load tests.
func (s *Server) Seed(n, follows, posts int, password string) {
	base := time.Now().Add(-time.Duration(n*posts) * time.Minute)
	for i := 0; i < n; i++ {
		a := &Account{DID: fmt.Sprintf("did:plc:fake%06d", i), Handle: fmt.Sprintf("user%d.test", i),
			DisplayName: fmt.Sprintf("User %d", i), AppPassword: password}
		for j := 1; j <= follows && j < n; j++ {
			a.Follows = append(a.Follows, fmt.Sprintf("did:plc:fake%06d", (i+j)%n))
		}
		s.AddAccount(a)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := base
	for k := 0; k < posts; k++ {
		for i := 0; i < n; i++ {
			t = t.Add(time.Second)
			rkey := s.tid()
			did := fmt.Sprintf("did:plc:fake%06d", i)
			p := &Post{URI: fmt.Sprintf("at://%s/app.bsky.feed.post/%s", did, rkey), CID: "bafyrei" + rkey, Author: did,
				Text: fmt.Sprintf("Post %d from user %d with a link https://example.com/%d and #tag", k, i, k), CreatedAt: t, IndexedAt: t,
				Likes: map[string]string{}, Reposts: map[string]string{}}
			s.posts[p.URI] = p
			s.order = append(s.order, p)
		}
	}
	sort.SliceStable(s.order, func(i, j int) bool { return s.order[i].IndexedAt.After(s.order[j].IndexedAt) })
}

// timeline merges followed accounts' posts and reposts, newest first.
func (s *Server) timeline(w http.ResponseWriter, q map[string][]string, viewer string) {
	offset, limit := pageArgs(q)
	s.mu.RLock()
	defer s.mu.RUnlock()
	type entry struct {
		at   time.Time
		item map[string]any
	}
	var all []entry
	for _, p := range s.order {
		if p.Author == viewer || s.follows[viewer][p.Author] != "" {
			all = append(all, entry{p.IndexedAt, nil})
			all[len(all)-1].item = s.feedItem(p, viewer)
		}
	}
	for _, ev := range s.reposts {
		if ev.by == viewer || s.follows[viewer][ev.by] != "" {
			if s.posts[ev.post.URI] == nil || ev.post.Reposts[ev.by] != ev.uri {
				continue
			}
			item := s.feedItem(ev.post, viewer)
			item["reason"] = map[string]any{"$type": "app.bsky.feed.defs#reasonRepost", "by": s.profile(s.accounts[ev.by], viewer, false),
				"uri": ev.uri, "indexedAt": ev.at.UTC().Format("2006-01-02T15:04:05.000Z")}
			all = append(all, entry{ev.at, item})
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].at.After(all[j].at) })
	if offset > len(all) {
		offset = len(all)
	}
	all = all[offset:]
	next := ""
	if len(all) > limit {
		all = all[:limit]
		next = strconv.Itoa(offset + limit)
	}
	items := []any{}
	for _, e := range all {
		items = append(items, e.item)
	}
	out := map[string]any{"feed": items}
	if next != "" {
		out["cursor"] = next
	}
	ok(w, out)
}

var (
	jpegOnce sync.Once
	jpegData []byte
)

func fakeJPEG() []byte {
	jpegOnce.Do(func() {
		img := image.NewRGBA(image.Rect(0, 0, 400, 400))
		for y := 0; y < 400; y++ {
			for x := 0; x < 400; x++ {
				img.Set(x, y, color.RGBA{uint8(x), uint8(y), 180, 255})
			}
		}
		var b bytes.Buffer
		jpeg.Encode(&b, img, nil)
		jpegData = b.Bytes()
	})
	return jpegData
}
