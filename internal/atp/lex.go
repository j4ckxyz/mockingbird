package atp

import (
	"encoding/json"
	"strings"
	"time"
)

// ProfileViewer is the viewer-relative state on a profile.
type ProfileViewer struct {
	Muted      bool   `json:"muted,omitempty"`
	BlockedBy  bool   `json:"blockedBy,omitempty"`
	Blocking   string `json:"blocking,omitempty"`
	Following  string `json:"following,omitempty"`
	FollowedBy string `json:"followedBy,omitempty"`
}

// Profile covers profileViewBasic, profileView and profileViewDetailed.
type Profile struct {
	DID            string         `json:"did"`
	Handle         string         `json:"handle"`
	DisplayName    string         `json:"displayName,omitempty"`
	Description    string         `json:"description,omitempty"`
	Avatar         string         `json:"avatar,omitempty"`
	Banner         string         `json:"banner,omitempty"`
	FollowersCount *int64         `json:"followersCount,omitempty"`
	FollowsCount   *int64         `json:"followsCount,omitempty"`
	PostsCount     *int64         `json:"postsCount,omitempty"`
	CreatedAt      string         `json:"createdAt,omitempty"`
	IndexedAt      string         `json:"indexedAt,omitempty"`
	Viewer         *ProfileViewer `json:"viewer,omitempty"`
	Website        string         `json:"website,omitempty"`
	Labels         []Label        `json:"labels,omitempty"`
}

// Label is a moderation or self label.
type Label struct {
	Src string `json:"src,omitempty"`
	Val string `json:"val"`
	Neg bool   `json:"neg,omitempty"`
}

// NoUnauthenticated reports whether the account asked not to be shown to
// logged-out viewers (the "!no-unauthenticated" self-label).
func (p *Profile) NoUnauthenticated() bool {
	for _, l := range p.Labels {
		if l.Val == "!no-unauthenticated" && !l.Neg {
			return true
		}
	}
	return false
}

// HasCounts reports whether this is a detailed view with counts.
func (p *Profile) HasCounts() bool { return p != nil && p.FollowersCount != nil }

// StrongRef is a com.atproto.repo.strongRef.
type StrongRef struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

// FacetIndex is a UTF-8 byte range.
type FacetIndex struct {
	ByteStart int `json:"byteStart"`
	ByteEnd   int `json:"byteEnd"`
}

// FacetFeature is one feature of a facet (mention, link or tag).
type FacetFeature struct {
	Type string `json:"$type"`
	URI  string `json:"uri,omitempty"`
	DID  string `json:"did,omitempty"`
	Tag  string `json:"tag,omitempty"`
}

// Facet types.
const (
	FacetMention = "app.bsky.richtext.facet#mention"
	FacetLink    = "app.bsky.richtext.facet#link"
	FacetTag     = "app.bsky.richtext.facet#tag"
)

// Facet is an app.bsky.richtext.facet.
type Facet struct {
	Index    FacetIndex     `json:"index"`
	Features []FacetFeature `json:"features"`
}

// ReplyRef is the reply field of a post record.
type ReplyRef struct {
	Root   StrongRef `json:"root"`
	Parent StrongRef `json:"parent"`
}

// PostRecord is an app.bsky.feed.post record as read back.
type PostRecord struct {
	Type      string          `json:"$type,omitempty"`
	Text      string          `json:"text"`
	Facets    []Facet         `json:"facets,omitempty"`
	Reply     *ReplyRef       `json:"reply,omitempty"`
	Embed     json.RawMessage `json:"embed,omitempty"`
	Langs     []string        `json:"langs,omitempty"`
	CreatedAt string          `json:"createdAt"`
	// Via names the client that made the post. It is not part of the
	// app.bsky lexicon; some clients add it (e.g. "Witchsky Web App").
	Via string `json:"via,omitempty"`
}

// PostViewer is viewer-relative state on a post.
type PostViewer struct {
	Like   string `json:"like,omitempty"`
	Repost string `json:"repost,omitempty"`
}

// PostView is an app.bsky.feed.defs#postView.
type PostView struct {
	URI         string          `json:"uri"`
	CID         string          `json:"cid"`
	Author      Profile         `json:"author"`
	Record      json.RawMessage `json:"record"`
	Embed       *EmbedView      `json:"embed,omitempty"`
	ReplyCount  int64           `json:"replyCount"`
	RepostCount int64           `json:"repostCount"`
	LikeCount   int64           `json:"likeCount"`
	QuoteCount  int64           `json:"quoteCount"`
	IndexedAt   string          `json:"indexedAt"`
	Viewer      *PostViewer     `json:"viewer,omitempty"`

	rec *PostRecord
}

// UnmarshalJSON decodes the view and its record eagerly, so decoded views can
// be shared between goroutines without further mutation.
func (p *PostView) UnmarshalJSON(b []byte) error {
	type alias PostView
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*p = PostView(a)
	var r PostRecord
	if len(p.Record) > 0 && json.Unmarshal(p.Record, &r) == nil {
		p.rec = &r
	} else {
		p.rec = &PostRecord{}
	}
	return nil
}

// Post returns the decoded record; a malformed record yields an empty post.
func (p *PostView) Post() *PostRecord {
	if p.rec == nil {
		return &PostRecord{}
	}
	return p.rec
}

// SortAt is the time a post sorts by: its declared creation time, unless that
// is in the future or later than when the AppView saw it (the same rule the
// Bluesky AppView uses), falling back to indexedAt.
func (p *PostView) SortAt() time.Time {
	return SortTime(p.Post().CreatedAt, p.IndexedAt)
}

// SortTime implements the createdAt/indexedAt rule for any record.
func SortTime(createdAt, indexedAt string) time.Time {
	c, cerr := ParseTime(createdAt)
	i, ierr := ParseTime(indexedAt)
	switch {
	case cerr == nil && ierr == nil:
		if c.After(i) {
			return i
		}
		return c
	case ierr == nil:
		return i
	case cerr == nil:
		if now := time.Now(); c.After(now) {
			return now
		}
		return c
	}
	return time.Time{}
}

// ParseTime parses atproto datetimes, which vary in fractional precision and
// offset style.
func ParseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, errEmptyTime
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		// Some records omit the timezone.
		if t2, err2 := time.Parse("2006-01-02T15:04:05.999999999", strings.TrimSuffix(s, "Z")); err2 == nil {
			return t2.UTC(), nil
		}
		return time.Time{}, err
	}
	return t.UTC(), nil
}

type timeErr string

func (e timeErr) Error() string { return string(e) }

const errEmptyTime = timeErr("atp: empty time")

// Embed view union members.
const (
	EmbedImagesView          = "app.bsky.embed.images#view"
	EmbedVideoView           = "app.bsky.embed.video#view"
	EmbedExternalView        = "app.bsky.embed.external#view"
	EmbedRecordView          = "app.bsky.embed.record#view"
	EmbedRecordWithMediaView = "app.bsky.embed.recordWithMedia#view"
	EmbedGalleryView         = "app.bsky.embed.gallery#view"

	RecordViewRecord   = "app.bsky.embed.record#viewRecord"
	RecordViewNotFound = "app.bsky.embed.record#viewNotFound"
	RecordViewBlocked  = "app.bsky.embed.record#viewBlocked"
	RecordViewDetached = "app.bsky.embed.record#viewDetached"
)

// ImageView is one image in an images view.
type ImageView struct {
	Thumb       string       `json:"thumb"`
	Fullsize    string       `json:"fullsize"`
	Alt         string       `json:"alt"`
	AspectRatio *AspectRatio `json:"aspectRatio,omitempty"`
}

// AspectRatio is width:height.
type AspectRatio struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// ExternalView is a link card.
type ExternalView struct {
	URI         string `json:"uri"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Thumb       string `json:"thumb,omitempty"`
}

// EmbeddedRecord is the record inside a record view.
type EmbeddedRecord struct {
	Type      string          `json:"$type"`
	URI       string          `json:"uri"`
	CID       string          `json:"cid"`
	Author    *Profile        `json:"author,omitempty"`
	Value     json.RawMessage `json:"value,omitempty"`
	Embeds    []EmbedView     `json:"embeds,omitempty"`
	IndexedAt string          `json:"indexedAt,omitempty"`
	// Feed generator / list views carry a display name.
	DisplayName string `json:"displayName,omitempty"`
	Name        string `json:"name,omitempty"`
}

// Post decodes the embedded record's value as a post.
func (r *EmbeddedRecord) Post() *PostRecord {
	var p PostRecord
	_ = json.Unmarshal(r.Value, &p)
	return &p
}

// RecordEmbed is the record part of a record view.
type RecordEmbed struct {
	Record *EmbeddedRecord `json:"record"`
}

// EmbedView is the union of embed views, flattened. Only fields matching Type
// are populated.
type EmbedView struct {
	Type string `json:"$type"`
	// images
	Images []ImageView `json:"images,omitempty"`
	// video
	Thumbnail string `json:"thumbnail,omitempty"`
	Playlist  string `json:"playlist,omitempty"`
	Alt       string `json:"alt,omitempty"`
	// external
	External *ExternalView `json:"external,omitempty"`
	// record: the record member is itself a union (viewRecord, viewNotFound...)
	// recordWithMedia: record is a record#view and media is another view
	RawRecord json.RawMessage `json:"record,omitempty"`
	Media     *EmbedView      `json:"media,omitempty"`
}

// Record returns the embedded record for record and recordWithMedia views.
func (e *EmbedView) Record() *EmbeddedRecord {
	if e == nil || len(e.RawRecord) == 0 {
		return nil
	}
	switch e.Type {
	case EmbedRecordView:
		var r EmbeddedRecord
		if json.Unmarshal(e.RawRecord, &r) == nil {
			return &r
		}
	case EmbedRecordWithMediaView:
		var outer RecordEmbed
		if json.Unmarshal(e.RawRecord, &outer) == nil {
			return outer.Record
		}
	}
	return nil
}

// ReasonRepost marks a feed item as a repost.
type ReasonRepost struct {
	Type      string  `json:"$type"`
	By        Profile `json:"by"`
	URI       string  `json:"uri,omitempty"`
	CID       string  `json:"cid,omitempty"`
	IndexedAt string  `json:"indexedAt"`
}

// FeedReplyRef is the reply context on a feed item.
type FeedReplyRef struct {
	Root              json.RawMessage `json:"root"`
	Parent            json.RawMessage `json:"parent"`
	GrandparentAuthor *Profile        `json:"grandparentAuthor,omitempty"`
}

// ParentPost returns the parent post view if it is a visible post.
func (r *FeedReplyRef) ParentPost() *PostView {
	if r == nil || len(r.Parent) == 0 {
		return nil
	}
	var probe struct {
		Type string `json:"$type"`
	}
	if json.Unmarshal(r.Parent, &probe) != nil || (probe.Type != "" && probe.Type != "app.bsky.feed.defs#postView") {
		return nil
	}
	var p PostView
	if json.Unmarshal(r.Parent, &p) != nil || p.URI == "" {
		return nil
	}
	return &p
}

// FeedViewPost is one item of a feed.
type FeedViewPost struct {
	Post   PostView      `json:"post"`
	Reply  *FeedReplyRef `json:"reply,omitempty"`
	Reason *ReasonRepost `json:"reason,omitempty"`
}

// Repost returns the repost reason, if this item is a repost.
func (f *FeedViewPost) Repost() *ReasonRepost {
	if f.Reason != nil && f.Reason.Type == "app.bsky.feed.defs#reasonRepost" {
		return f.Reason
	}
	return nil
}

// Feed is a paginated feed response.
type Feed struct {
	Cursor string         `json:"cursor,omitempty"`
	Feed   []FeedViewPost `json:"feed"`
}

// Posts is a getPosts / searchPosts response.
type Posts struct {
	Cursor string     `json:"cursor,omitempty"`
	Posts  []PostView `json:"posts"`
}

// Notification is one app.bsky.notification.listNotifications item.
type Notification struct {
	URI           string          `json:"uri"`
	CID           string          `json:"cid"`
	Author        Profile         `json:"author"`
	Reason        string          `json:"reason"`
	ReasonSubject string          `json:"reasonSubject,omitempty"`
	Record        json.RawMessage `json:"record"`
	IsRead        bool            `json:"isRead"`
	IndexedAt     string          `json:"indexedAt"`
}

// Notifications is a listNotifications response.
type Notifications struct {
	Cursor        string         `json:"cursor,omitempty"`
	Notifications []Notification `json:"notifications"`
}

// Profiles is a getProfiles / searchActors response.
type Profiles struct {
	Cursor   string    `json:"cursor,omitempty"`
	Profiles []Profile `json:"profiles,omitempty"`
	Actors   []Profile `json:"actors,omitempty"`
}

// Follows is a getFollows / getFollowers response.
type Follows struct {
	Cursor    string    `json:"cursor,omitempty"`
	Subject   Profile   `json:"subject"`
	Follows   []Profile `json:"follows,omitempty"`
	Followers []Profile `json:"followers,omitempty"`
}

// RepostedBy is a getRepostedBy response.
type RepostedBy struct {
	Cursor     string    `json:"cursor,omitempty"`
	RepostedBy []Profile `json:"repostedBy"`
}

// Relationship is one app.bsky.graph.defs#relationship.
type Relationship struct {
	Type       string `json:"$type"`
	DID        string `json:"did"`
	Following  string `json:"following,omitempty"`
	FollowedBy string `json:"followedBy,omitempty"`
	Blocking   string `json:"blocking,omitempty"`
	BlockedBy  string `json:"blockedBy,omitempty"`
	NotFound   bool   `json:"notFound,omitempty"`
}

// Relationships is a getRelationships response.
type Relationships struct {
	Actor         string         `json:"actor,omitempty"`
	Relationships []Relationship `json:"relationships"`
}

// Session is a createSession / refreshSession response.
type Session struct {
	DID        string          `json:"did"`
	Handle     string          `json:"handle"`
	AccessJWT  string          `json:"accessJwt"`
	RefreshJWT string          `json:"refreshJwt"`
	DIDDoc     json.RawMessage `json:"didDoc,omitempty"`
	Active     *bool           `json:"active,omitempty"`
	Status     string          `json:"status,omitempty"`
}

// CreateRecordOutput is the result of createRecord.
type CreateRecordOutput struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

// Blob is a blob reference.
type Blob struct {
	Type     string `json:"$type"`
	Ref      Link   `json:"ref"`
	MimeType string `json:"mimeType"`
	Size     int64  `json:"size"`
}

// Link is a CID link in JSON form.
type Link struct {
	Link string `json:"$link"`
}

// UploadBlobOutput is the result of uploadBlob.
type UploadBlobOutput struct {
	Blob json.RawMessage `json:"blob"`
}

// TrendingTopic is one unspecced trending topic.
type TrendingTopic struct {
	Topic       string `json:"topic"`
	DisplayName string `json:"displayName,omitempty"`
	Link        string `json:"link"`
}

// TrendingTopics is a getTrendingTopics response.
type TrendingTopics struct {
	Topics    []TrendingTopic `json:"topics"`
	Suggested []TrendingTopic `json:"suggested,omitempty"`
}

// Chat types (chat.bsky.convo.*).

// ChatMember is a member of a conversation.
type ChatMember = Profile

// ChatMessage is a chat.bsky.convo.defs#messageView (or deletedMessageView).
type ChatMessage struct {
	Type   string  `json:"$type"`
	ID     string  `json:"id"`
	Rev    string  `json:"rev"`
	Text   string  `json:"text"`
	Facets []Facet `json:"facets,omitempty"`
	Sender struct {
		DID string `json:"did"`
	} `json:"sender"`
	SentAt string `json:"sentAt"`
}

// Deleted reports whether this is a deletedMessageView.
func (m *ChatMessage) Deleted() bool {
	return strings.HasSuffix(m.Type, "#deletedMessageView")
}

// Convo is a chat.bsky.convo.defs#convoView.
type Convo struct {
	ID          string          `json:"id"`
	Rev         string          `json:"rev"`
	Members     []ChatMember    `json:"members"`
	LastMessage json.RawMessage `json:"lastMessage,omitempty"`
	UnreadCount int             `json:"unreadCount"`
	Muted       bool            `json:"muted"`
	Status      string          `json:"status,omitempty"`
}

// Convos is a listConvos response.
type Convos struct {
	Cursor string  `json:"cursor,omitempty"`
	Convos []Convo `json:"convos"`
}

// ConvoOutput wraps a single convo.
type ConvoOutput struct {
	Convo Convo `json:"convo"`
}

// Messages is a getMessages response.
type Messages struct {
	Cursor   string        `json:"cursor,omitempty"`
	Messages []ChatMessage `json:"messages"`
}
