package api

import (
	"context"
	"net/url"
	"strconv"

	"github.com/j4ckxyz/mockingbird/internal/atp"
	"github.com/j4ckxyz/mockingbird/internal/paging"
	"github.com/j4ckxyz/mockingbird/internal/translate"
	"github.com/j4ckxyz/mockingbird/internal/twitter"
)

func (s *Server) homeTimeline(c *Ctx) (*Resp, error) {
	return c.timeline("home", c.feedFetch("app.bsky.feed.getTimeline", nil, nil), &twitter.FeedInfo{
		Title: "Twitter / Home", Link: s.cfg.PublicURL.String() + "/",
		SelfURL: s.cfg.PublicURL.String() + "/statuses/home_timeline." + c.Format, Description: "Home timeline of " + c.Sess.Handle(),
	})
}

func (s *Server) userTimeline(c *Ctx) (*Resp, error) {
	actor, err := c.targetActor()
	if err != nil {
		return nil, err
	}
	info := &twitter.FeedInfo{
		Title: "Twitter / " + actor, Link: links{s}.Profile(actor),
		SelfURL:     s.cfg.PublicURL.String() + "/statuses/user_timeline/" + url.PathEscape(actor) + "." + c.Format,
		Description: "Twitter updates from " + actor + ".",
	}
	params := url.Values{"actor": {actor}, "filter": {"posts_with_replies"}}
	return c.timeline("user:"+actor, c.feedFetch("app.bsky.feed.getAuthorFeed", params, nil), info)
}

func (s *Server) publicTimeline(c *Ctx) (*Resp, error) {
	return c.timeline("public", c.feedFetch("app.bsky.feed.getFeed", url.Values{"feed": {s.cfg.PublicTimelineFeed}}, nil),
		&twitter.FeedInfo{Title: "Twitter public timeline", Link: s.cfg.PublicURL.String() + "/",
			SelfURL: s.cfg.PublicURL.String() + "/statuses/public_timeline." + c.Format, Description: "Popular posts on Bluesky"})
}

func (s *Server) retweetedToMe(c *Ctx) (*Resp, error) {
	return c.timeline("rt-to-me", c.feedFetch("app.bsky.feed.getTimeline", nil, func(f *atp.FeedViewPost) bool {
		return f.Repost() != nil && f.Repost().By.DID != c.Sess.DID
	}), nil)
}

func (s *Server) retweetedByMe(c *Ctx) (*Resp, error) {
	params := url.Values{"actor": {c.Sess.DID}, "filter": {"posts_with_replies"}}
	return c.timeline("rt-by-me", c.feedFetch("app.bsky.feed.getAuthorFeed", params, func(f *atp.FeedViewPost) bool {
		return f.Repost() != nil
	}), nil)
}

func (s *Server) favorites(c *Ctx) (*Resp, error) {
	actor, err := c.targetActor()
	if err != nil {
		return nil, err
	}
	// Bluesky only exposes an account's likes to that account.
	if actor != c.Sess.DID && actor != c.Sess.Handle() {
		return c.statusList(nil, nil), nil
	}
	return c.timeline("likes", c.feedFetch("app.bsky.feed.getActorLikes", url.Values{"actor": {c.Sess.DID}}, nil), nil)
}

// notificationFetch pages listNotifications for the given reasons and
// hydrates the posts they point at. For reply/mention/quote the notification
// URI is the post; for repost/like it is reasonSubject (the viewer's post).
func (c *Ctx) notificationFetch(reasons []string, useSubject bool) paging.Fetch {
	seen := map[string]bool{}
	return func(ctx context.Context, cursor string, limit int) ([]translate.Item, string, error) {
		p := url.Values{"limit": {strconv.Itoa(max(limit, 25))}, "reasons": reasons}
		if cursor != "" {
			p.Set("cursor", cursor)
		}
		var res atp.Notifications
		if err := c.Caller().Do(ctx, &atp.Request{NSID: "app.bsky.notification.listNotifications", Params: p}, &res); err != nil {
			return nil, "", err
		}
		type ref struct {
			uri string
			at  string
		}
		var refs []ref
		var uris []string
		for _, n := range res.Notifications {
			if !contains(reasons, n.Reason) {
				continue // older PDSes ignore the reasons filter
			}
			uri := n.URI
			if useSubject {
				uri = n.ReasonSubject
			}
			if uri == "" || seen[uri] {
				continue
			}
			seen[uri] = true
			refs = append(refs, ref{uri, n.IndexedAt})
			uris = append(uris, uri)
		}
		posts, err := c.getPosts(uris)
		if err != nil {
			return nil, "", err
		}
		var items []translate.Item
		for _, r := range refs {
			pv := posts[r.uri]
			if pv == nil {
				continue
			}
			it := translate.Item{Post: pv}
			if !useSubject {
				if t, err := atp.ParseTime(r.at); err == nil {
					it.At = t
				}
			}
			items = append(items, it)
		}
		return items, res.Cursor, nil
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func (s *Server) mentions(c *Ctx) (*Resp, error) {
	return c.timeline("mentions", c.notificationFetch([]string{"mention", "reply", "quote"}, false), &twitter.FeedInfo{
		Title: "Twitter / Mentions", Link: s.cfg.PublicURL.String() + "/",
		SelfURL: s.cfg.PublicURL.String() + "/statuses/mentions." + c.Format, Description: "Mentions of " + c.Sess.Handle(),
	})
}

func (s *Server) retweetsOfMe(c *Ctx) (*Resp, error) {
	return c.timeline("rt-of-me", c.notificationFetch([]string{"repost"}, true), nil)
}

func (s *Server) retweets(c *Ctx) (*Resp, error) {
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
	var res atp.RepostedBy
	if err := c.Get("app.bsky.feed.getRepostedBy", url.Values{"uri": {uri}, "limit": {strconv.Itoa(c.Count(20, 100))}}, &res); err != nil {
		return nil, upstreamErr(err)
	}
	items := make([]translate.Item, 0, len(res.RepostedBy))
	for i := range res.RepostedBy {
		items = append(items, translate.Item{Post: post, Repost: &atp.ReasonRepost{By: res.RepostedBy[i], IndexedAt: post.IndexedAt}})
	}
	sts, err := s.builder(c).Statuses(c.Context(), items)
	if err != nil {
		return nil, err
	}
	return c.statusList(sts, nil), nil
}
