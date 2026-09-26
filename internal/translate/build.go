package translate

import (
	"context"
	"strings"
	"time"

	"github.com/j4ckxyz/mockingbird/internal/atp"
	"github.com/j4ckxyz/mockingbird/internal/cache"
	"github.com/j4ckxyz/mockingbird/internal/store"
	"github.com/j4ckxyz/mockingbird/internal/twitter"
)

// IDMapper assigns numeric IDs.
type IDMapper interface {
	StatusIDs(ctx context.Context, refs []store.StatusRef) (map[string]int64, error)
	UserIDs(ctx context.Context, refs []store.UserRef) (map[string]int64, error)
}

// ProfileSource returns detailed profiles (with counts) for DIDs, from cache
// where possible. Missing entries are allowed.
type ProfileSource func(ctx context.Context, dids []string) map[string]*atp.Profile

// Builder turns Bluesky views into Twitter objects.
type Builder struct {
	IDs      IDMapper
	Links    Links
	Profiles ProfileSource // optional
	// Handles remembers DID -> handle for every profile seen, so reply
	// targets can be named without another upstream call.
	Handles *cache.LRU[string, string]
	// OnProfile, if set, is called for every profile translated.
	OnProfile func(p *atp.Profile)
}

// Source is the "source" attribution on every status.
const Source = `<a href="https://bsky.app" rel="nofollow">Bluesky</a>`

// Default profile colours from Twitter's documented defaults.
const (
	defaultBackground = "9ae4e8"
	defaultText       = "000000"
	defaultLink       = "0000ff"
	defaultFill       = "e0ff92"
	defaultBorder     = "87bc44"
)

// bskyEpoch is used when a profile has no creation time.
var bskyEpoch = time.Date(2023, 2, 17, 0, 0, 0, 0, time.UTC)

// Item is one timeline entry: a post, optionally reposted, with optional
// reply-parent context.
type Item struct {
	Post   *atp.PostView
	Repost *atp.ReasonRepost
	Parent *atp.PostView
	// At overrides the sort time (e.g. a notification's indexedAt).
	At time.Time
}

// FromFeed converts feed items.
func FromFeed(feed []atp.FeedViewPost) []Item {
	out := make([]Item, 0, len(feed))
	for i := range feed {
		f := &feed[i]
		out = append(out, Item{Post: &f.Post, Repost: f.Repost(), Parent: f.Reply.ParentPost()})
	}
	return out
}

// FromPosts converts plain post views.
func FromPosts(posts []atp.PostView) []Item {
	out := make([]Item, 0, len(posts))
	for i := range posts {
		out = append(out, Item{Post: &posts[i]})
	}
	return out
}

// RepostKey identifies a repost as a status. Reposts carry their record
// URI in current AppViews; otherwise a synthetic key is used.
func RepostKey(r *atp.ReasonRepost, postURI string) string {
	if r.URI != "" {
		return r.URI
	}
	return "repost:" + r.By.DID + ":" + postURI
}

// Key returns the status identity of the item.
func (it *Item) Key() string {
	if it.Repost != nil {
		return RepostKey(it.Repost, it.Post.URI)
	}
	return it.Post.URI
}

// SortAt returns when the item entered the timeline.
func (it *Item) SortAt() time.Time {
	if !it.At.IsZero() {
		return it.At
	}
	if it.Repost != nil {
		if t, err := atp.ParseTime(it.Repost.IndexedAt); err == nil {
			return t
		}
	}
	return it.Post.SortAt()
}

// DIDFromURI returns the repo DID of an AT-URI ("at://did:plc:x/coll/rkey").
func DIDFromURI(uri string) string {
	rest, ok := strings.CutPrefix(uri, "at://")
	if !ok {
		return ""
	}
	did, _, _ := strings.Cut(rest, "/")
	if !strings.HasPrefix(did, "did:") {
		return ""
	}
	return did
}

// RkeyFromURI returns the record key of an AT-URI.
func RkeyFromURI(uri string) string {
	i := strings.LastIndexByte(uri, '/')
	if i < 0 {
		return ""
	}
	return uri[i+1:]
}

// CollectionFromURI returns the collection NSID of an AT-URI.
func CollectionFromURI(uri string) string {
	rest, ok := strings.CutPrefix(uri, "at://")
	if !ok {
		return ""
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return ""
	}
	return parts[1]
}

type batch struct {
	b      *Builder
	srefs  []store.StatusRef
	urefs  []store.UserRef
	sids   map[string]int64
	uids   map[string]int64
	detail map[string]*atp.Profile
}

func (b *Builder) newBatch() *batch { return &batch{b: b} }

func (x *batch) status(uri string, at time.Time) {
	x.srefs = append(x.srefs, store.StatusRef{URI: uri, SortAt: at})
}

func (x *batch) user(p *atp.Profile) {
	if p == nil || p.DID == "" {
		return
	}
	x.urefs = append(x.urefs, store.UserRef{DID: p.DID, Handle: p.Handle})
	if x.b.Handles != nil && p.Handle != "" && p.Handle != "handle.invalid" {
		x.b.Handles.Add(p.DID, p.Handle)
	}
	if x.b.OnProfile != nil {
		x.b.OnProfile(p)
	}
}

func (x *batch) did(did string) {
	if did != "" {
		x.urefs = append(x.urefs, store.UserRef{DID: did})
	}
}

func (x *batch) resolve(ctx context.Context, withCounts bool) error {
	var err error
	if x.sids, err = x.b.IDs.StatusIDs(ctx, x.srefs); err != nil {
		return err
	}
	if x.uids, err = x.b.IDs.UserIDs(ctx, x.urefs); err != nil {
		return err
	}
	if withCounts && x.b.Profiles != nil {
		seen := map[string]bool{}
		var dids []string
		for _, u := range x.urefs {
			if !seen[u.DID] {
				seen[u.DID] = true
				dids = append(dids, u.DID)
			}
		}
		x.detail = x.b.Profiles(ctx, dids)
	}
	return nil
}

// Statuses translates timeline items, in order.
func (b *Builder) Statuses(ctx context.Context, items []Item) ([]twitter.Status, error) {
	x := b.newBatch()
	for i := range items {
		it := &items[i]
		x.status(it.Key(), it.SortAt())
		x.gatherPost(it.Post, it.Parent)
		if it.Repost != nil {
			x.user(&it.Repost.By)
		}
	}
	if err := x.resolve(ctx, true); err != nil {
		return nil, err
	}
	out := make([]twitter.Status, 0, len(items))
	for i := range items {
		it := &items[i]
		st := x.buildPost(it.Post, it.Parent)
		if it.Repost != nil {
			orig := st
			st = twitter.Status{
				CreatedAt:           twitter.Time(it.SortAt()),
				ID:                  x.sids[it.Key()],
				Text:                "RT @" + it.Post.Author.Handle + ": " + orig.Text,
				Source:              Source,
				Favorited:           orig.Favorited,
				Retweeted:           orig.Retweeted,
				RetweetCount:        orig.RetweetCount,
				InReplyToStatusID:   nil,
				InReplyToUserID:     nil,
				InReplyToScreenName: nil,
				RetweetedStatus:     &orig,
			}
			u := x.buildUser(&it.Repost.By)
			st.User = &u
			st.IDStr = twitter.IDString(st.ID)
		}
		out = append(out, st)
	}
	return out, nil
}

// Status translates a single post.
func (b *Builder) Status(ctx context.Context, it Item) (twitter.Status, error) {
	s, err := b.Statuses(ctx, []Item{it})
	if err != nil {
		return twitter.Status{}, err
	}
	return s[0], nil
}

func (x *batch) gatherPost(p *atp.PostView, parent *atp.PostView) {
	x.status(p.URI, p.SortAt())
	x.user(&p.Author)
	rec := p.Post()
	if rec.Reply != nil {
		var at time.Time
		if parent != nil && parent.URI == rec.Reply.Parent.URI {
			at = parent.SortAt()
			x.user(&parent.Author)
		}
		x.status(rec.Reply.Parent.URI, at)
		x.did(DIDFromURI(rec.Reply.Parent.URI))
	}
	if r := p.Embed.Record(); r != nil && r.Type == atp.RecordViewRecord && r.URI != "" {
		x.status(r.URI, atp.SortTime(r.Post().CreatedAt, r.IndexedAt))
		x.user(r.Author)
	}
}

func (x *batch) buildPost(p *atp.PostView, parent *atp.PostView) twitter.Status {
	rec := p.Post()
	id := x.sids[p.URI]
	st := twitter.Status{
		CreatedAt: twitter.Time(p.SortAt()),
		ID:        id,
		Source:    Source,
		IDStr:     twitter.IDString(id),
	}
	st.Text = PostText(rec, p.Embed, x.b.Links.PostPage(id), func(uri string) string {
		if qid := x.sids[uri]; qid != 0 {
			return x.b.Links.PostPage(qid)
		}
		return ""
	})
	if p.Viewer != nil {
		st.Favorited = p.Viewer.Like != ""
		st.Retweeted = p.Viewer.Repost != ""
	}
	st.RetweetCount = p.RepostCount
	if rec.Reply != nil {
		pid := x.sids[rec.Reply.Parent.URI]
		st.InReplyToStatusID = &pid
		st.InReplyToStatusIDStr = twitter.Ptr(twitter.IDString(pid))
		pdid := DIDFromURI(rec.Reply.Parent.URI)
		if uid := x.uids[pdid]; uid != 0 {
			st.InReplyToUserID = &uid
			st.InReplyToUserIDStr = twitter.Ptr(twitter.IDString(uid))
		}
		handle := ""
		if parent != nil && parent.URI == rec.Reply.Parent.URI {
			handle = parent.Author.Handle
		} else if x.b.Handles != nil {
			handle, _ = x.b.Handles.Get(pdid)
		}
		if handle != "" {
			st.InReplyToScreenName = &handle
		}
	}
	u := x.buildUser(&p.Author)
	st.User = &u
	return st
}

func (x *batch) buildUser(p *atp.Profile) twitter.User {
	uid := x.uids[p.DID]
	d := p
	if p.FollowersCount == nil {
		if det, ok := x.detail[p.DID]; ok && det != nil {
			d = det
		}
	}
	return x.b.userFrom(p, d, uid)
}

// userFrom builds a user from a profile (p) and, if available, a detailed
// profile carrying counts and description (d, which may equal p).
func (b *Builder) userFrom(p, d *atp.Profile, uid int64) twitter.User {
	name := p.DisplayName
	if strings.TrimSpace(name) == "" {
		name = p.Handle
	}
	created := bskyEpoch
	for _, s := range []string{p.CreatedAt, d.CreatedAt, p.IndexedAt, d.IndexedAt} {
		if t, err := atp.ParseTime(s); err == nil && t.After(time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)) {
			created = t
			break
		}
	}
	desc := p.Description
	if desc == "" {
		desc = d.Description
	}
	u := twitter.User{
		ID:                        uid,
		Name:                      name,
		ScreenName:                p.Handle,
		Location:                  "",
		Description:               desc,
		ProfileImageURL:           b.Links.Avatar(p.Avatar),
		Protected:                 false,
		ProfileBackgroundColor:    defaultBackground,
		ProfileTextColor:          defaultText,
		ProfileLinkColor:          defaultLink,
		ProfileSidebarFillColor:   defaultFill,
		ProfileSidebarBorderColor: defaultBorder,
		CreatedAt:                 twitter.Time(created),
		ProfileBackgroundImageURL: b.Links.StaticURL("/static/bg.gif"),
		ProfileBackgroundTile:     false,
		ProfileUseBackgroundImage: true,
		Notifications:             twitter.Ptr(false),
		Verified:                  false,
		Lang:                      "en",
		FollowRequestSent:         twitter.Ptr(false),
		IDStr:                     twitter.IDString(uid),
	}
	if d.FollowersCount != nil {
		u.FollowersCount = *d.FollowersCount
	}
	if d.FollowsCount != nil {
		u.FriendsCount = *d.FollowsCount
	}
	if d.PostsCount != nil {
		u.StatusesCount = *d.PostsCount
	}
	if p.Website != "" {
		u.URL = twitter.Ptr(p.Website)
	}
	following := p.Viewer != nil && p.Viewer.Following != ""
	u.Following = &following
	return u
}

// Users translates profiles, fetching counts where missing.
func (b *Builder) Users(ctx context.Context, profiles []atp.Profile) ([]twitter.User, error) {
	x := b.newBatch()
	for i := range profiles {
		x.user(&profiles[i])
	}
	if err := x.resolve(ctx, true); err != nil {
		return nil, err
	}
	out := make([]twitter.User, 0, len(profiles))
	for i := range profiles {
		out = append(out, x.buildUser(&profiles[i]))
	}
	return out, nil
}

// User translates one profile.
func (b *Builder) User(ctx context.Context, p *atp.Profile) (twitter.User, error) {
	us, err := b.Users(ctx, []atp.Profile{*p})
	if err != nil {
		return twitter.User{}, err
	}
	return us[0], nil
}

// UserID returns (assigning if needed) the numeric ID for a DID.
func (b *Builder) UserID(ctx context.Context, did, handle string) (int64, error) {
	m, err := b.IDs.UserIDs(ctx, []store.UserRef{{DID: did, Handle: handle}})
	if err != nil {
		return 0, err
	}
	return m[did], nil
}

// StatusID returns (assigning if needed) the numeric ID for an AT-URI.
func (b *Builder) StatusID(ctx context.Context, uri string, at time.Time) (int64, error) {
	m, err := b.IDs.StatusIDs(ctx, []store.StatusRef{{URI: uri, SortAt: at}})
	if err != nil {
		return 0, err
	}
	return m[uri], nil
}

// SearchResults translates posts into the Search API's result shape.
func (b *Builder) SearchResults(ctx context.Context, posts []atp.PostView) ([]twitter.SearchResult, error) {
	x := b.newBatch()
	for i := range posts {
		x.gatherPost(&posts[i], nil)
	}
	if err := x.resolve(ctx, false); err != nil {
		return nil, err
	}
	out := make([]twitter.SearchResult, 0, len(posts))
	for i := range posts {
		p := &posts[i]
		st := x.buildPost(p, nil)
		lang := "en"
		if l := p.Post().Langs; len(l) > 0 && l[0] != "" {
			lang, _, _ = strings.Cut(l[0], "-")
		}
		r := twitter.SearchResult{
			Text:            st.Text,
			ToUserID:        st.InReplyToUserID,
			ToUser:          st.InReplyToScreenName,
			FromUser:        p.Author.Handle,
			Metadata:        twitter.SearchMetadata{ResultType: "recent"},
			ID:              st.ID,
			FromUserID:      st.User.ID,
			ISOLanguageCode: lang,
			Source:          strings.ReplaceAll(EscapeHTML(Source), `"`, "&quot;"),
			ProfileImageURL: st.User.ProfileImageURL,
			CreatedAt:       twitter.SearchTime(p.SortAt()),
			IDStr:           st.IDStr,
			FromUserIDStr:   st.User.IDStr,
			ToUserIDStr:     st.InReplyToUserIDStr,
		}
		out = append(out, r)
	}
	return out, nil
}

// DirectMessages translates chat messages. ids maps "convo/msg" to DM IDs.
func (b *Builder) DirectMessages(ctx context.Context, msgs []DMInput, dmIDs map[store.DMRef]int64) ([]twitter.DirectMessage, error) {
	x := b.newBatch()
	for _, m := range msgs {
		x.user(m.Sender)
		x.user(m.Recipient)
	}
	if err := x.resolve(ctx, true); err != nil {
		return nil, err
	}
	out := make([]twitter.DirectMessage, 0, len(msgs))
	for _, m := range msgs {
		id := dmIDs[store.DMRef{ConvoID: m.ConvoID, MsgID: m.Msg.ID}]
		sender := x.buildUser(m.Sender)
		recip := x.buildUser(m.Recipient)
		at, _ := atp.ParseTime(m.Msg.SentAt)
		out = append(out, twitter.DirectMessage{
			ID: id, SenderID: sender.ID, Text: EscapeHTML(ExpandLinks(m.Msg.Text, m.Msg.Facets)),
			RecipientID: recip.ID, CreatedAt: twitter.Time(at),
			SenderScreenName: sender.ScreenName, RecipientScreenName: recip.ScreenName,
			Sender: &sender, Recipient: &recip, IDStr: twitter.IDString(id),
		})
	}
	return out, nil
}

// DMInput is one chat message with its resolved participants.
type DMInput struct {
	ConvoID   string
	Msg       *atp.ChatMessage
	Sender    *atp.Profile
	Recipient *atp.Profile
}
