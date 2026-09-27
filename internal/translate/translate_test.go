package translate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/j4ckxyz/mockingbird/internal/atp"
	"github.com/j4ckxyz/mockingbird/internal/cache"
	"github.com/j4ckxyz/mockingbird/internal/store"
)

func TestEscapeHTML(t *testing.T) {
	got := EscapeHTML(`I <3 "Bluesky" & you > them`)
	want := `I &lt;3 "Bluesky" &amp; you &gt; them`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestExpandLinks(t *testing.T) {
	text := "read this: example.com/very/lo... and 🙂 more"
	start := strings.Index(text, "example.com")
	end := start + len("example.com/very/lo...")
	facets := []atp.Facet{{Index: atp.FacetIndex{ByteStart: start, ByteEnd: end},
		Features: []atp.FacetFeature{{Type: atp.FacetLink, URI: "https://example.com/very/long/path?q=1"}}}}
	got := ExpandLinks(text, facets)
	want := "read this: https://example.com/very/long/path?q=1 and 🙂 more"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestExpandLinksIgnoresBadFacets(t *testing.T) {
	text := "short"
	facets := []atp.Facet{
		{Index: atp.FacetIndex{ByteStart: 3, ByteEnd: 99}, Features: []atp.FacetFeature{{Type: atp.FacetLink, URI: "https://x"}}},
		{Index: atp.FacetIndex{ByteStart: 4, ByteEnd: 2}, Features: []atp.FacetFeature{{Type: atp.FacetLink, URI: "https://y"}}},
	}
	if got := ExpandLinks(text, facets); got != text {
		t.Fatalf("bad facets changed text: %q", got)
	}
}

func TestExpandLinksOverlapping(t *testing.T) {
	text := "aaa bbb ccc"
	facets := []atp.Facet{
		{Index: atp.FacetIndex{ByteStart: 0, ByteEnd: 7}, Features: []atp.FacetFeature{{Type: atp.FacetLink, URI: "https://one"}}},
		{Index: atp.FacetIndex{ByteStart: 4, ByteEnd: 11}, Features: []atp.FacetFeature{{Type: atp.FacetLink, URI: "https://two"}}},
	}
	if got := ExpandLinks(text, facets); got != "https://one ccc" {
		t.Fatalf("got %q", got)
	}
}

func facetTexts(text string, fs []atp.Facet) []string {
	var out []string
	for _, f := range fs {
		ft := f.Features[0]
		v := ft.URI + ft.DID + ft.Tag
		out = append(out, fmt.Sprintf("%s=%s(%s)", strings.TrimPrefix(ft.Type, "app.bsky.richtext.facet#"), text[f.Index.ByteStart:f.Index.ByteEnd], v))
	}
	return out
}

func TestBuildFacets(t *testing.T) {
	resolve := func(ctx context.Context, name string) (string, string, bool) {
		switch name {
		case "alice", "alice.bsky.social":
			return "alice.bsky.social", "did:plc:alice", true
		case "bob.example.com":
			return "bob.example.com", "did:plc:bob", true
		}
		return "", "", false
	}
	text := "héllo @alice and @bob.example.com, see https://example.com/a_(b) and example.org. #café #123 #go! @nobody 🎉"
	got := facetTexts(text, BuildFacets(context.Background(), text, resolve))
	want := []string{
		"mention=@alice(did:plc:alice)",
		"mention=@bob.example.com(did:plc:bob)",
		"link=https://example.com/a_(b)(https://example.com/a_(b))",
		"link=example.org(https://example.org)",
		"tag=#café(café)",
		"tag=#go(go)",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("facets:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestEmailIsNotMention(t *testing.T) {
	text := "mail me at someone@example.com"
	fs := BuildFacets(context.Background(), text, func(ctx context.Context, n string) (string, string, bool) { return n, "did:plc:x", true })
	for _, f := range fs {
		if f.Features[0].Type == atp.FacetMention {
			t.Fatalf("email parsed as mention: %v", facetTexts(text, fs))
		}
	}
}

func TestGraphemeCount(t *testing.T) {
	// Family emoji is one grapheme but many code points and bytes.
	s := "👨‍👩‍👧‍👦" + strings.Repeat("a", 299)
	if n := GraphemeCount(s); n != 300 {
		t.Fatalf("got %d graphemes", n)
	}
	if GraphemeCount(s+"b") <= MaxGraphemes == true {
		t.Fatal("301 graphemes should exceed the limit")
	}
}

func TestUnescapeClientText(t *testing.T) {
	if got := UnescapeClientText("RT @a: I &lt;3 you &amp;amp; them"); got != "RT @a: I <3 you &amp; them" {
		t.Fatalf("got %q", got)
	}
}

type fakeLinks struct{}

func (fakeLinks) PostPage(id int64) string  { return fmt.Sprintf("http://bridge/p/%d", id) }
func (fakeLinks) Avatar(u string) string    { return "http://bridge/img/" + u }
func (fakeLinks) StaticURL(p string) string { return "http://bridge" + p }
func (fakeLinks) Profile(h string) string   { return "http://bridge/u/" + h }

type memIDs struct {
	mu     sync.Mutex
	s, u   map[string]int64
	ns, nu int64
}

func (m *memIDs) StatusIDs(ctx context.Context, refs []store.StatusRef) (map[string]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int64{}
	for _, r := range refs {
		if m.s[r.URI] == 0 {
			m.ns++
			m.s[r.URI] = m.ns
		}
		out[r.URI] = m.s[r.URI]
	}
	return out, nil
}

func (m *memIDs) UserIDs(ctx context.Context, refs []store.UserRef) (map[string]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int64{}
	for _, r := range refs {
		if m.u[r.DID] == 0 {
			m.nu++
			m.u[r.DID] = m.nu
		}
		out[r.DID] = m.u[r.DID]
	}
	return out, nil
}

func newBuilder() *Builder {
	return &Builder{IDs: &memIDs{s: map[string]int64{}, u: map[string]int64{}}, Links: fakeLinks{}, Handles: cache.New[string, string](100)}
}

const feedFixture = `{"feed":[
 {"post":{"uri":"at://did:plc:carol/app.bsky.feed.post/3kq","cid":"bafy1","author":{"did":"did:plc:carol","handle":"carol.test","displayName":"Carol","avatar":"https://cdn.bsky.app/img/avatar/plain/did:plc:carol/bafkav@jpeg"},
  "record":{"$type":"app.bsky.feed.post","text":"look at this","createdAt":"2024-05-01T12:00:00.000Z",
    "embed":{"$type":"app.bsky.embed.images","images":[]}},
  "embed":{"$type":"app.bsky.embed.images#view","images":[{"thumb":"https://cdn.bsky.app/img/feed_thumbnail/plain/did:plc:carol/bafkimg@jpeg","fullsize":"https://cdn.bsky.app/img/feed_fullsize/plain/did:plc:carol/bafkimg@jpeg","alt":"a cat"}]},
  "repostCount":3,"likeCount":5,"indexedAt":"2024-05-01T12:00:01.000Z","viewer":{"like":"at://did:plc:me/app.bsky.feed.like/1"}},
  "reason":{"$type":"app.bsky.feed.defs#reasonRepost","by":{"did":"did:plc:dave","handle":"dave.test","displayName":""},"uri":"at://did:plc:dave/app.bsky.feed.repost/3kr","indexedAt":"2024-05-01T13:00:00.000Z"}},
 {"post":{"uri":"at://did:plc:erin/app.bsky.feed.post/3ks","cid":"bafy2","author":{"did":"did:plc:erin","handle":"erin.test"},
  "record":{"$type":"app.bsky.feed.post","text":"replying <here> & there","createdAt":"2024-05-01T11:00:00.000Z",
    "reply":{"root":{"uri":"at://did:plc:carol/app.bsky.feed.post/3kp","cid":"x"},"parent":{"uri":"at://did:plc:carol/app.bsky.feed.post/3kp","cid":"x"}}},
  "indexedAt":"2024-05-01T11:00:00.500Z"},
  "reply":{"parent":{"$type":"app.bsky.feed.defs#postView","uri":"at://did:plc:carol/app.bsky.feed.post/3kp","cid":"x","author":{"did":"did:plc:carol","handle":"carol.test"},"record":{"text":"parent","createdAt":"2024-05-01T10:00:00Z"},"indexedAt":"2024-05-01T10:00:00Z"},
           "root":{"$type":"app.bsky.feed.defs#postView","uri":"at://did:plc:carol/app.bsky.feed.post/3kp","cid":"x","author":{"did":"did:plc:carol","handle":"carol.test"},"record":{"text":"parent","createdAt":"2024-05-01T10:00:00Z"},"indexedAt":"2024-05-01T10:00:00Z"}}},
 {"post":{"uri":"at://did:plc:erin/app.bsky.feed.post/weird","cid":"bafy3","author":{"did":"did:plc:erin","handle":"erin.test"},
  "record":{"$type":"app.bsky.feed.post","text":"quoting something exotic","createdAt":"2024-05-01T10:30:00Z"},
  "embed":{"$type":"app.bsky.embed.record#view","record":{"$type":"com.example.unknown#view","uri":"at://did:plc:x/com.example.thing/1"}},
  "indexedAt":"2024-05-01T10:30:00Z"}}
]}`

func TestStatusesFromFeed(t *testing.T) {
	var feed atp.Feed
	if err := json.Unmarshal([]byte(feedFixture), &feed); err != nil {
		t.Fatalf("fixture must decode even with unknown embed types: %v", err)
	}
	b := newBuilder()
	sts, err := b.Statuses(context.Background(), FromFeed(feed.Feed))
	if err != nil {
		t.Fatal(err)
	}
	if len(sts) != 3 {
		t.Fatalf("got %d statuses", len(sts))
	}
	rt := sts[0]
	if rt.RetweetedStatus == nil {
		t.Fatal("repost should carry retweeted_status")
	}
	orig := rt.RetweetedStatus
	if !strings.HasPrefix(rt.Text, "RT @carol.test: look at this http://bridge/p/") {
		t.Fatalf("retweet fallback text: %q", rt.Text)
	}
	if rt.User.ScreenName != "dave.test" || rt.User.Name != "dave.test" {
		t.Fatalf("retweeter user: %+v", rt.User)
	}
	if time.Time(rt.CreatedAt).Format(time.RFC3339) != "2024-05-01T13:00:00Z" {
		t.Fatalf("retweet time should be repost time, got %v", rt.CreatedAt)
	}
	if rt.ID == orig.ID {
		t.Fatal("retweet and original must have distinct IDs")
	}
	if !orig.Favorited || orig.RetweetCount != 3 {
		t.Fatalf("viewer state lost: %+v", orig)
	}
	if orig.Text != fmt.Sprintf("look at this http://bridge/p/%d", orig.ID) {
		t.Fatalf("image embed not linked: %q", orig.Text)
	}
	reply := sts[1]
	if reply.Text != "replying &lt;here&gt; &amp; there" {
		t.Fatalf("text not escaped: %q", reply.Text)
	}
	if reply.InReplyToStatusID == nil || reply.InReplyToScreenName == nil || *reply.InReplyToScreenName != "carol.test" || reply.InReplyToUserID == nil {
		t.Fatalf("reply fields: %+v", reply)
	}
	if *reply.InReplyToUserID != sts[0].RetweetedStatus.User.ID {
		t.Fatal("reply user ID should match carol's user ID")
	}
	if sts[2].Text != "quoting something exotic" {
		t.Fatalf("unknown quote embed should be ignored: %q", sts[2].Text)
	}
	if got := sts[0].RetweetedStatus.User.ProfileImageURL; got != "http://bridge/img/https://cdn.bsky.app/img/avatar/plain/did:plc:carol/bafkav@jpeg" {
		t.Fatalf("avatar: %q", got)
	}
}

func TestDIDFromURI(t *testing.T) {
	if DIDFromURI("at://did:plc:abc/app.bsky.feed.post/1") != "did:plc:abc" {
		t.Fatal("did")
	}
	if DIDFromURI("at://alice.test/app.bsky.feed.post/1") != "" {
		t.Fatal("handle authority should not be treated as DID")
	}
	if CollectionFromURI("at://did:plc:abc/app.bsky.feed.repost/1") != "app.bsky.feed.repost" || RkeyFromURI("at://did:plc:abc/app.bsky.feed.repost/1") != "1" {
		t.Fatal("collection/rkey")
	}
}

// A post record's "via" names its client; it becomes the status source as
// escaped plain text, falling back to the Bluesky link.
func TestSourceFor(t *testing.T) {
	cases := map[string]string{
		"":                       Source,
		"   ":                    Source,
		"​\x07":                  Source,
		"Witchsky Web App":       "Witchsky Web App",
		"  Tweetie  ":            "Tweetie",
		"a\n\tb   c":             "a b c",
		`<a href="x">y</a>`:      "&lt;a href=&quot;x&quot;&gt;y&lt;/a&gt;",
		"Tom & Jerry":            "Tom &amp; Jerry",
		"zero‍width":             "zerowidth",
		strings.Repeat("x", 100): strings.Repeat("x", maxViaRunes),
	}
	for in, want := range cases {
		if got := SourceFor(in); got != want {
			t.Errorf("SourceFor(%q) = %q, want %q", in, got, want)
		}
	}
	if got := searchSource(SourceFor("A&B")); got != "A&amp;amp;B" {
		t.Errorf("search source: %q", got)
	}
}
