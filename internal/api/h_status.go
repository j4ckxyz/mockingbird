package api

import (
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackgilbert/mockingbird/internal/atp"
	"github.com/jackgilbert/mockingbird/internal/translate"
	"github.com/jackgilbert/mockingbird/internal/twitter"
)

func (s *Server) showStatus(c *Ctx) (*Resp, error) {
	key, err := c.statusURI()
	if err != nil {
		return nil, err
	}
	st, err := c.statusFor(key)
	if err != nil {
		return nil, err
	}
	r := statusResp(st)
	r.Cacheable = true
	return r, nil
}

var uploadURLRE = regexp.MustCompile(`\s*https?://[^\s/]+/m/([A-Za-z0-9_-]{16,64})\b`)

func (s *Server) updateStatus(c *Ctx) (*Resp, error) {
	text := strings.TrimSpace(translate.UnescapeClientText(c.Form.Get("status")))
	ctx := c.Context()

	// Images uploaded through the TwitPic endpoint appear as bridge links;
	// strip them and attach the real image embed instead.
	var images []map[string]any
	text = uploadURLRE.ReplaceAllStringFunc(text, func(m string) string {
		tok := uploadURLRE.FindStringSubmatch(m)[1]
		up, err := s.d.Store.GetUpload(ctx, tok)
		if err != nil || up.DID != c.Sess.DID || len(images) >= 4 {
			return m
		}
		var blob json.RawMessage = json.RawMessage(up.BlobJSON)
		img := map[string]any{"alt": "", "image": blob}
		if up.Width > 0 && up.Height > 0 {
			img["aspectRatio"] = map[string]int{"width": up.Width, "height": up.Height}
		}
		images = append(images, img)
		return ""
	})
	text = strings.TrimSpace(text)
	if text == "" && len(images) == 0 {
		return nil, errForbidden("Status is empty.")
	}
	if n := translate.GraphemeCount(text); n > translate.MaxGraphemes || len(text) > translate.MaxBytes {
		return nil, errForbidden("Status is over 300 characters.")
	}

	rec := map[string]any{
		"$type":     "app.bsky.feed.post",
		"text":      text,
		"createdAt": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}
	facets := translate.BuildFacets(ctx, text, s.mentionResolver(c))
	if len(facets) > 0 {
		rec["facets"] = facets
	}
	if len(images) > 0 {
		rec["embed"] = map[string]any{"$type": "app.bsky.embed.images", "images": images}
	}
	var parent *atp.PostView
	if c.Int64Arg("in_reply_to_status_id") > 0 {
		key, err := c.statusURI("in_reply_to_status_id")
		if err != nil {
			return nil, err
		}
		puri, err := c.originalPostURI(key)
		if err != nil {
			return nil, err
		}
		posts, err := c.getPosts([]string{puri})
		if err != nil {
			return nil, err
		}
		if parent = posts[puri]; parent == nil {
			return nil, errForbidden("The post you are replying to is not available.")
		}
		root := atp.StrongRef{URI: parent.URI, CID: parent.CID}
		if pr := parent.Post().Reply; pr != nil && pr.Root.URI != "" {
			root = pr.Root
		}
		rec["reply"] = map[string]any{"root": root, "parent": atp.StrongRef{URI: parent.URI, CID: parent.CID}}
	}

	var out atp.CreateRecordOutput
	if err := c.Post("com.atproto.repo.createRecord", map[string]any{
		"repo": c.Sess.DID, "collection": "app.bsky.feed.post", "record": rec,
	}, &out); err != nil {
		return nil, upstreamErr(err)
	}
	// Build the response from what we just wrote; the AppView may lag.
	recJSON, _ := json.Marshal(rec)
	me, err := c.selfProfile()
	if err != nil {
		return nil, err
	}
	var pv atp.PostView
	if err := json.Unmarshal(mustJSON(map[string]any{
		"uri": out.URI, "cid": out.CID, "author": me, "record": json.RawMessage(recJSON),
		"indexedAt": time.Now().UTC().Format(time.RFC3339Nano),
	}), &pv); err != nil {
		return nil, err
	}
	st, err := s.builder(c).Status(ctx, translate.Item{Post: &pv, Parent: parent})
	if err != nil {
		return nil, err
	}
	return statusResp(&st), nil
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// selfProfile returns the viewer's profile (cached briefly).
func (c *Ctx) selfProfile() (*atp.Profile, error) {
	if p, ok := c.s.profiles.Get(c.Sess.DID); ok {
		cp := *p
		return &cp, nil
	}
	return c.profile(c.Sess.DID)
}

// mentionResolver resolves @names for outgoing posts: full handles through
// identity resolution; bare names through handles the viewer has seen
// recently, then <name>.<default host>.
func (s *Server) mentionResolver(c *Ctx) translate.MentionResolver {
	budget := 10 // resolutions per post; each may hit DNS and HTTPS
	return func(ctx context.Context, name string) (string, string, bool) {
		if budget == 0 {
			return "", "", false
		}
		budget--
		handle := c.fullHandle(name)
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		who, err := s.d.Resolver.Resolve(ctx, handle)
		if err != nil || who.Handle == "handle.invalid" {
			return "", "", false
		}
		return who.Handle, who.DID, true
	}
}

// deleteOwn deletes one of the viewer's records by URI.
func (c *Ctx) deleteOwn(uri string) error {
	if translate.DIDFromURI(uri) != c.Sess.DID {
		return errForbidden("You may not delete another user's status!")
	}
	if err := c.Post("com.atproto.repo.deleteRecord", map[string]string{
		"repo": c.Sess.DID, "collection": translate.CollectionFromURI(uri), "rkey": translate.RkeyFromURI(uri),
	}, nil); err != nil {
		return upstreamErr(err)
	}
	return nil
}

func (s *Server) destroyStatus(c *Ctx) (*Resp, error) {
	key, err := c.statusURI()
	if err != nil {
		return nil, err
	}
	if translate.DIDFromURI(key) != c.Sess.DID && !strings.HasPrefix(key, "repost:"+c.Sess.DID+":") {
		return nil, errForbidden("You may not delete another user's status!")
	}
	st, err := c.statusFor(key)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(key, "repost:") {
		// Synthetic key: find the viewer's repost record through the post.
		uri, _ := c.originalPostURI(key)
		posts, err := c.getPosts([]string{uri})
		if err != nil || posts[uri] == nil || posts[uri].Viewer == nil || posts[uri].Viewer.Repost == "" {
			return nil, errNotFound()
		}
		key = posts[uri].Viewer.Repost
	}
	if err := c.deleteOwn(key); err != nil {
		return nil, err
	}
	return statusResp(st), nil
}

func (s *Server) retweet(c *Ctx) (*Resp, error) {
	key, err := c.statusURI()
	if err != nil {
		return nil, err
	}
	uri, err := c.originalPostURI(key)
	if err != nil {
		return nil, err
	}
	posts, err := c.getPosts([]string{uri})
	if err != nil {
		return nil, err
	}
	post := posts[uri]
	if post == nil {
		return nil, errNotFound()
	}
	if post.Viewer != nil && post.Viewer.Repost != "" {
		return nil, errForbidden("sharing is not permissable for this status (Share validations failed)")
	}
	var out atp.CreateRecordOutput
	if err := c.Post("com.atproto.repo.createRecord", map[string]any{
		"repo": c.Sess.DID, "collection": "app.bsky.feed.repost",
		"record": map[string]any{"$type": "app.bsky.feed.repost", "subject": atp.StrongRef{URI: post.URI, CID: post.CID},
			"createdAt": time.Now().UTC().Format("2006-01-02T15:04:05.000Z")},
	}, &out); err != nil {
		return nil, upstreamErr(err)
	}
	me, err := c.selfProfile()
	if err != nil {
		return nil, err
	}
	if post.Viewer == nil {
		post.Viewer = &atp.PostViewer{}
	}
	post.Viewer.Repost = out.URI
	post.RepostCount++
	s.reposts.Add(out.URI, post.URI)
	st, err := s.builder(c).Status(c.Context(), translate.Item{Post: post,
		Repost: &atp.ReasonRepost{By: *me, URI: out.URI, IndexedAt: time.Now().UTC().Format(time.RFC3339Nano)}})
	if err != nil {
		return nil, err
	}
	return statusResp(&st), nil
}

func (s *Server) favoriteCreate(c *Ctx) (*Resp, error) {
	key, err := c.statusURI()
	if err != nil {
		return nil, err
	}
	uri, err := c.originalPostURI(key)
	if err != nil {
		return nil, err
	}
	posts, err := c.getPosts([]string{uri})
	if err != nil {
		return nil, err
	}
	post := posts[uri]
	if post == nil {
		return nil, errNotFound()
	}
	if post.Viewer == nil || post.Viewer.Like == "" {
		var out atp.CreateRecordOutput
		if err := c.Post("com.atproto.repo.createRecord", map[string]any{
			"repo": c.Sess.DID, "collection": "app.bsky.feed.like",
			"record": map[string]any{"$type": "app.bsky.feed.like", "subject": atp.StrongRef{URI: post.URI, CID: post.CID},
				"createdAt": time.Now().UTC().Format("2006-01-02T15:04:05.000Z")},
		}, &out); err != nil {
			return nil, upstreamErr(err)
		}
		if post.Viewer == nil {
			post.Viewer = &atp.PostViewer{}
		}
		post.Viewer.Like = out.URI
	}
	st, err := s.builder(c).Status(c.Context(), translate.Item{Post: post})
	if err != nil {
		return nil, err
	}
	return statusResp(&st), nil
}

func (s *Server) favoriteDestroy(c *Ctx) (*Resp, error) {
	key, err := c.statusURI()
	if err != nil {
		return nil, err
	}
	uri, err := c.originalPostURI(key)
	if err != nil {
		return nil, err
	}
	posts, err := c.getPosts([]string{uri})
	if err != nil {
		return nil, err
	}
	post := posts[uri]
	if post == nil {
		return nil, errNotFound()
	}
	if post.Viewer != nil && post.Viewer.Like != "" {
		if err := c.Post("com.atproto.repo.deleteRecord", map[string]string{
			"repo": c.Sess.DID, "collection": "app.bsky.feed.like", "rkey": translate.RkeyFromURI(post.Viewer.Like),
		}, nil); err != nil {
			return nil, upstreamErr(err)
		}
		post.Viewer.Like = ""
	}
	st, err := s.builder(c).Status(c.Context(), translate.Item{Post: post})
	if err != nil {
		return nil, err
	}
	return statusResp(&st), nil
}

// latestStatus returns an actor's newest post for embedding in user objects.
func (c *Ctx) latestStatus(actor string) *twitter.Status {
	var feed atp.Feed
	err := c.Get("app.bsky.feed.getAuthorFeed", url.Values{"actor": {actor}, "limit": {"1"}, "filter": {"posts_no_replies"}}, &feed)
	if err != nil || len(feed.Feed) == 0 {
		return nil
	}
	f := feed.Feed[0]
	f.Reason = nil
	st, err := c.s.builder(c).Status(c.Context(), translate.Item{Post: &f.Post})
	if err != nil {
		return nil
	}
	st.User = nil // nested inside the user already
	return &st
}
