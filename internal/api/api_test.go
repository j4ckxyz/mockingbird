package api_test

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"github.com/jackgilbert/mockingbird/internal/fakepds"
	"image"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type tUser struct {
	ID              int64    `json:"id"`
	IDStr           string   `json:"id_str"`
	ScreenName      string   `json:"screen_name"`
	Name            string   `json:"name"`
	ProfileImageURL string   `json:"profile_image_url"`
	FollowersCount  int64    `json:"followers_count"`
	FriendsCount    int64    `json:"friends_count"`
	StatusesCount   int64    `json:"statuses_count"`
	Following       *bool    `json:"following"`
	Protected       bool     `json:"protected"`
	CreatedAt       string   `json:"created_at"`
	Status          *tStatus `json:"status"`
}

type tStatus struct {
	ID                  int64    `json:"id"`
	IDStr               string   `json:"id_str"`
	Text                string   `json:"text"`
	CreatedAt           string   `json:"created_at"`
	InReplyToStatusID   *int64   `json:"in_reply_to_status_id"`
	InReplyToScreenName *string  `json:"in_reply_to_screen_name"`
	Favorited           bool     `json:"favorited"`
	Retweeted           bool     `json:"retweeted"`
	User                *tUser   `json:"user"`
	RetweetedStatus     *tStatus `json:"retweeted_status"`
	Source              string   `json:"source"`
}

func TestVerifyCredentials(t *testing.T) {
	h := newHarness(t)
	h.seed()
	for _, path := range []string{"/account/verify_credentials.json", "/1/account/verify_credentials.json"} {
		r := h.get(path, alice)
		if r.code != 200 {
			t.Fatalf("%s: %d %s", path, r.code, r.body)
		}
		var u tUser
		r.json(t, &u)
		if u.ScreenName != "alice.test" || u.Name != "Alice <A>" || u.ID == 0 || u.IDStr != fmt.Sprint(u.ID) {
			t.Fatalf("user %+v", u)
		}
		if !strings.HasPrefix(u.ProfileImageURL, "http://bird.test/img/a/") || !strings.HasSuffix(u.ProfileImageURL, "_normal.jpg") {
			t.Fatalf("avatar should be a signed bridge URL: %s", u.ProfileImageURL)
		}
		if u.FollowersCount != 1 || u.FriendsCount != 1 || u.Protected {
			t.Fatalf("counts: %+v", u)
		}
		if _, err := time.Parse("Mon Jan 02 15:04:05 -0700 2006", u.CreatedAt); err != nil {
			t.Fatalf("created_at format: %q", u.CreatedAt)
		}
		if u.Status == nil || !strings.Contains(u.Status.Text, "alice says hi") {
			t.Fatalf("latest status missing: %+v", u.Status)
		}
		if r.hdr.Get("X-RateLimit-Limit") == "" || r.hdr.Get("X-RateLimit-Remaining") == "" || r.hdr.Get("X-RateLimit-Reset") == "" {
			t.Fatalf("rate limit headers missing: %v", r.hdr)
		}
	}
	if n := h.pds.CallCount("com.atproto.server.createSession"); n != 1 {
		t.Fatalf("createSession called %d times", n)
	}
}

func TestAuthFailures(t *testing.T) {
	h := newHarness(t)
	r := h.get("/account/verify_credentials.json")
	if r.code != 401 || !strings.Contains(string(r.body), `"error":"Could not authenticate you."`) || r.hdr.Get("WWW-Authenticate") == "" {
		t.Fatalf("no creds: %d %s %v", r.code, r.body, r.hdr)
	}
	r = h.get("/account/verify_credentials.json", basic("alice.test", "wrong-wron-gwro-ngwr"))
	if r.code != 401 {
		t.Fatalf("wrong password: %d", r.code)
	}
	r = h.get("/account/verify_credentials.json", basic("alice.test", "full-password"))
	if r.code != 401 || !strings.Contains(string(r.body), "app password") {
		t.Fatalf("full password should be refused with a clear message: %d %s", r.code, r.body)
	}
	r = h.get("/account/verify_credentials.xml", basic("alice.test", "nope"))
	if r.code != 401 || !strings.Contains(string(r.body), "<hash><request>/account/verify_credentials.xml</request><error>") {
		t.Fatalf("XML error: %s", r.body)
	}
	// suppress_response_codes forces 200 but keeps the error body.
	r = h.get("/account/verify_credentials.json?suppress_response_codes=true")
	if r.code != 200 || !strings.Contains(string(r.body), "Could not authenticate") {
		t.Fatalf("suppress_response_codes: %d %s", r.code, r.body)
	}
}

func TestHomeTimelineAndSinceID(t *testing.T) {
	h := newHarness(t)
	bob1, bob2, _ := h.seed()
	r := h.get("/statuses/home_timeline.json?count=20", alice)
	if r.code != 200 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	var sts []tStatus
	r.json(t, &sts)
	if len(sts) != 3 {
		t.Fatalf("want 3 statuses, got %d: %s", len(sts), r.body)
	}
	if sts[0].Text != bob2.Text || sts[2].Text != bob1.Text {
		t.Fatalf("order: %q .. %q", sts[0].Text, sts[2].Text)
	}
	if sts[1].Text != "alice says hi &amp; &lt;waves&gt;" {
		t.Fatalf("text must be HTML-escaped like Twitter: %q", sts[1].Text)
	}
	if !(sts[0].ID > sts[1].ID && sts[1].ID > sts[2].ID) {
		t.Fatalf("IDs of a first-seen page should ascend with time: %d %d %d", sts[0].ID, sts[1].ID, sts[2].ID)
	}
	if sts[0].Source != `<a href="https://bsky.app" rel="nofollow">Bluesky</a>` {
		t.Fatalf("source: %q", sts[0].Source)
	}
	newest := sts[0].ID

	// Poll: nothing new.
	r = h.get(fmt.Sprintf("/statuses/home_timeline.json?since_id=%d", newest), alice)
	r.json(t, &sts)
	if len(sts) != 0 {
		t.Fatalf("since_id with nothing new returned %d", len(sts))
	}
	// A new post arrives.
	h.pds.AddPost("did:plc:bob", "third post", time.Now())
	r = h.get(fmt.Sprintf("/1/statuses/friends_timeline.json?since_id=%d", newest), alice)
	r.json(t, &sts)
	if len(sts) != 1 || sts[0].Text != "third post" || sts[0].ID <= newest {
		t.Fatalf("since_id poll: %+v", sts)
	}
	// max_id paging (client sends oldest - 1).
	r = h.get("/statuses/home_timeline.json?count=2", alice)
	r.json(t, &sts)
	oldest := sts[len(sts)-1].ID
	r = h.get(fmt.Sprintf("/statuses/home_timeline.json?count=2&max_id=%d", oldest-1), alice)
	r.json(t, &sts)
	if len(sts) != 2 || sts[0].Text != "alice says hi &amp; &lt;waves&gt;" {
		t.Fatalf("max_id page: %+v", sts)
	}
}

func TestTimelineXMLAndFeeds(t *testing.T) {
	h := newHarness(t)
	h.seed()
	r := h.get("/statuses/home_timeline.xml", alice)
	if r.code != 200 || !strings.HasPrefix(r.hdr.Get("Content-Type"), "application/xml") {
		t.Fatalf("%d %v", r.code, r.hdr)
	}
	var doc struct {
		XMLName  xml.Name `xml:"statuses"`
		Type     string   `xml:"type,attr"`
		Statuses []struct {
			ID   int64  `xml:"id"`
			Text string `xml:"text"`
			User struct {
				ScreenName string `xml:"screen_name"`
			} `xml:"user"`
		} `xml:"status"`
	}
	if err := xml.Unmarshal(r.body, &doc); err != nil {
		t.Fatalf("XML must parse: %v\n%s", err, r.body)
	}
	if doc.Type != "array" || len(doc.Statuses) != 3 || doc.Statuses[1].Text != "alice says hi &amp; &lt;waves&gt;" {
		t.Fatalf("doc: %+v", doc)
	}
	for _, f := range []string{"rss", "atom"} {
		r = h.get("/statuses/home_timeline."+f, alice)
		if r.code != 200 || !strings.Contains(string(r.body), "second post from bob") {
			t.Fatalf("%s: %d %s", f, r.code, r.body)
		}
	}
	// RSS on an endpoint without feeds is a clean 404.
	r = h.get("/account/verify_credentials.rss", alice)
	if r.code != 404 {
		t.Fatalf("rss on verify_credentials: %d", r.code)
	}
}

func TestUpdateReplyMentionAndDestroy(t *testing.T) {
	h := newHarness(t)
	bob1, _, _ := h.seed()
	r := h.get("/statuses/home_timeline.json", alice)
	var sts []tStatus
	r.json(t, &sts)
	var bob1ID int64
	for _, s := range sts {
		if s.Text == bob1.Text {
			bob1ID = s.ID
		}
	}
	// Reply mentioning bob by bare name (seen in the timeline) and carol by full handle.
	r = h.post("/statuses/update.json", url.Values{"status": {"@bob hey, cc @carol.other.example see https://example.com #yay"},
		"in_reply_to_status_id": {fmt.Sprint(bob1ID)}}, alice)
	if r.code != 200 {
		t.Fatalf("update: %d %s", r.code, r.body)
	}
	var st tStatus
	r.json(t, &st)
	if st.InReplyToStatusID == nil || *st.InReplyToStatusID != bob1ID || st.InReplyToScreenName == nil || *st.InReplyToScreenName != "bob.test" {
		t.Fatalf("reply fields: %+v", st)
	}
	posts := h.pds.Posts()
	p := posts[0]
	if p.Author != "did:plc:alice" || p.Reply == nil {
		t.Fatalf("record not written as reply: %+v", p)
	}
	par := p.Reply["parent"].(map[string]any)
	root := p.Reply["root"].(map[string]any)
	if par["uri"] != bob1.URI || root["uri"] != bob1.URI || par["cid"] != bob1.CID {
		t.Fatalf("reply refs: %+v", p.Reply)
	}
	kinds := map[string]string{}
	for _, f := range p.Facets {
		feat := f["features"].([]any)[0].(map[string]any)
		kinds[feat["$type"].(string)] = fmt.Sprint(feat["did"], feat["uri"], feat["tag"])
	}
	if !strings.Contains(kinds["app.bsky.richtext.facet#mention"], "did:plc:") || !strings.Contains(kinds["app.bsky.richtext.facet#link"], "https://example.com") || !strings.Contains(kinds["app.bsky.richtext.facet#tag"], "yay") {
		t.Fatalf("facets: %v", kinds)
	}
	mentions := 0
	for _, f := range p.Facets {
		if f["features"].([]any)[0].(map[string]any)["$type"] == "app.bsky.richtext.facet#mention" {
			mentions++
		}
	}
	if mentions != 2 {
		t.Fatalf("want 2 mention facets, got %d", mentions)
	}
	// Reply to a reply: root stays the thread root.
	r = h.post("/statuses/update.json", url.Values{"status": {"nested"}, "in_reply_to_status_id": {fmt.Sprint(st.ID)}}, basic("bob.test", bobPW))
	if r.code != 200 {
		t.Fatalf("nested reply: %d %s", r.code, r.body)
	}
	nested := h.pds.Posts()[0]
	if nested.Reply["root"].(map[string]any)["uri"] != bob1.URI || nested.Reply["parent"].(map[string]any)["uri"] != p.URI {
		t.Fatalf("nested refs: %+v", nested.Reply)
	}

	// Too long: 301 graphemes.
	r = h.post("/statuses/update.json", url.Values{"status": {strings.Repeat("é", 301)}}, alice)
	if r.code != 403 || !strings.Contains(string(r.body), "over 300 characters") {
		t.Fatalf("too long: %d %s", r.code, r.body)
	}
	// 300 emoji graphemes are fine.
	r = h.post("/statuses/update.json", url.Values{"status": {strings.Repeat("🐦", 300)}}, alice)
	if r.code != 200 {
		t.Fatalf("300 graphemes: %d %s", r.code, r.body)
	}

	// Destroy own; refuse others'.
	r = h.post(fmt.Sprintf("/statuses/destroy/%d.json", st.ID), nil, alice)
	if r.code != 200 || h.pds.Post(p.URI) != nil {
		t.Fatalf("destroy: %d %s", r.code, r.body)
	}
	r = h.post(fmt.Sprintf("/statuses/destroy/%d.json", bob1ID), nil, alice)
	if r.code != 403 {
		t.Fatalf("destroying another user's status: %d", r.code)
	}
}

func TestRetweetAndFavorite(t *testing.T) {
	h := newHarness(t)
	bob1, _, _ := h.seed()
	var sts []tStatus
	h.get("/statuses/home_timeline.json", alice).json(t, &sts)
	var id int64
	for _, s := range sts {
		if s.Text == bob1.Text {
			id = s.ID
		}
	}
	r := h.post(fmt.Sprintf("/statuses/retweet/%d.json", id), nil, alice)
	if r.code != 200 {
		t.Fatalf("retweet: %d %s", r.code, r.body)
	}
	var rt tStatus
	r.json(t, &rt)
	if rt.RetweetedStatus == nil || rt.RetweetedStatus.ID != id || rt.User.ScreenName != "alice.test" || !strings.HasPrefix(rt.Text, "RT @bob.test: ") {
		t.Fatalf("retweet shape: %+v", rt)
	}
	// Bob's timeline shows alice's repost as a retweet.
	h.get("/statuses/home_timeline.json", basic("bob.test", bobPW)).json(t, &sts)
	if sts[0].RetweetedStatus == nil || sts[0].User.ScreenName != "alice.test" {
		t.Fatalf("repost not surfaced as retweet: %+v", sts[0])
	}
	// statuses/show on the retweet ID.
	r = h.get(fmt.Sprintf("/statuses/show/%d.json", rt.ID), alice)
	var shown tStatus
	r.json(t, &shown)
	if shown.RetweetedStatus == nil || shown.ID != rt.ID {
		t.Fatalf("show retweet: %s", r.body)
	}
	// Undo by destroying the retweet status.
	r = h.post(fmt.Sprintf("/statuses/destroy/%d.json", rt.ID), nil, alice)
	if r.code != 200 || len(h.pds.Post(bob1.URI).Reposts) != 0 {
		t.Fatalf("undo retweet: %d %s", r.code, r.body)
	}

	r = h.post(fmt.Sprintf("/favorites/create/%d.json", id), nil, alice)
	var fav tStatus
	r.json(t, &fav)
	if !fav.Favorited || len(h.pds.Post(bob1.URI).Likes) != 1 {
		t.Fatalf("favorite: %s", r.body)
	}
	h.get("/favorites.json", alice).json(t, &sts)
	if len(sts) != 1 || sts[0].ID != id {
		t.Fatalf("favorites list: %+v", sts)
	}
	r = h.post(fmt.Sprintf("/favorites/destroy/%d.json", id), nil, alice)
	r.json(t, &fav)
	if fav.Favorited || len(h.pds.Post(bob1.URI).Likes) != 0 {
		t.Fatalf("unfavorite: %s", r.body)
	}
}

func TestMentions(t *testing.T) {
	h := newHarness(t)
	h.seed()
	r := h.post("/statuses/update.json", url.Values{"status": {"hello @alice.test"}}, basic("bob.test", bobPW))
	if r.code != 200 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	var sts []tStatus
	r = h.get("/statuses/mentions.json", alice)
	r.json(t, &sts)
	if len(sts) != 1 || sts[0].Text != "hello @alice.test" || sts[0].User.ScreenName != "bob.test" {
		t.Fatalf("mentions: %s", r.body)
	}
}

func TestUsersAndFriendships(t *testing.T) {
	h := newHarness(t)
	h.seed()
	var u tUser
	h.get("/users/show/bob.json", alice).json(t, &u) // dotless screen name -> bob.test
	if u.ScreenName != "bob.test" || u.Following == nil || !*u.Following {
		t.Fatalf("users/show: %+v", u)
	}
	bobID := u.ID
	h.get(fmt.Sprintf("/users/show.json?user_id=%d", bobID), alice).json(t, &u)
	if u.ScreenName != "bob.test" {
		t.Fatal("lookup by numeric user_id failed")
	}
	r := h.get("/friendships/show.json?target_screen_name=carol.other.example", alice)
	if !strings.Contains(string(r.body), `"relationship":{"source":{`) || !strings.Contains(string(r.body), `"following":false`) {
		t.Fatalf("friendships/show: %s", r.body)
	}
	r = h.post("/friendships/create/carol.other.example.json", nil, alice)
	if r.code != 200 {
		t.Fatalf("follow: %d %s", r.code, r.body)
	}
	r = h.get("/friendships/exists.json?user_a=alice.test&user_b=carol.other.example", alice)
	if string(r.body) != "true" {
		t.Fatalf("exists: %s", r.body)
	}
	r = h.get("/friendships/exists.xml?user_a=alice.test&user_b=carol.other.example", alice)
	if !strings.Contains(string(r.body), "<friends>true</friends>") {
		t.Fatalf("exists xml: %s", r.body)
	}
	var ids []int64
	h.get("/friends/ids.json", alice).json(t, &ids)
	if len(ids) != 2 {
		t.Fatalf("friends/ids: %v", ids)
	}
	r = h.get("/friends/ids.json?cursor=-1", alice)
	if !strings.Contains(string(r.body), `"next_cursor":0`) || !strings.Contains(string(r.body), `"ids":[`) {
		t.Fatalf("cursored ids: %s", r.body)
	}
	r = h.post("/friendships/destroy/carol.other.example.json", nil, alice)
	if r.code != 200 || strings.Contains(string(h.get("/friendships/exists.json?user_a=alice.test&user_b=carol.other.example", alice).body), "true") {
		t.Fatalf("unfollow: %d %s", r.code, r.body)
	}
	var us []tUser
	h.get("/users/lookup.json?screen_name=bob.test,carol.other.example", alice).json(t, &us)
	if len(us) != 2 {
		t.Fatalf("lookup: %+v", us)
	}
	h.get("/users/search.json?q=carol", alice).json(t, &us)
	if len(us) != 1 || us[0].ScreenName != "carol.other.example" {
		t.Fatalf("search: %+v", us)
	}
}

func TestDirectMessages(t *testing.T) {
	h := newHarness(t)
	// Normal app password: empty list, clear error on send.
	r := h.get("/direct_messages.json", alice)
	if r.code != 200 || string(r.body) != "[]" {
		t.Fatalf("no-scope list: %d %s", r.code, r.body)
	}
	r = h.post("/direct_messages/new.json", url.Values{"user": {"bob.test"}, "text": {"hi"}}, alice)
	if r.code != 403 || !strings.Contains(string(r.body), "direct messages") {
		t.Fatalf("no-scope send: %d %s", r.code, r.body)
	}
	aliceDMAuth := basic("alice.test", aliceDM)
	r = h.post("/direct_messages/new.json", url.Values{"screen_name": {"bob"}, "text": {"secret plans <3"}}, aliceDMAuth)
	if r.code != 200 {
		t.Fatalf("send: %d %s", r.code, r.body)
	}
	var dm struct {
		ID                  int64  `json:"id"`
		Text                string `json:"text"`
		SenderScreenName    string `json:"sender_screen_name"`
		RecipientScreenName string `json:"recipient_screen_name"`
	}
	r.json(t, &dm)
	if dm.Text != "secret plans &lt;3" || dm.SenderScreenName != "alice.test" || dm.RecipientScreenName != "bob.test" {
		t.Fatalf("dm: %+v", dm)
	}
	var list []struct {
		ID               int64  `json:"id"`
		Text             string `json:"text"`
		SenderScreenName string `json:"sender_screen_name"`
	}
	h.get("/direct_messages.json", basic("bob.test", bobDM)).json(t, &list)
	if len(list) != 1 || list[0].ID != dm.ID || list[0].SenderScreenName != "alice.test" {
		t.Fatalf("bob inbox: %+v", list)
	}
	h.get("/direct_messages/sent.json", aliceDMAuth).json(t, &list)
	if len(list) != 1 {
		t.Fatalf("alice sent: %+v", list)
	}
	r = h.get("/direct_messages.xml", basic("bob.test", bobDM))
	if !strings.Contains(string(r.body), `<direct-messages type="array"><direct_message><id>`) {
		t.Fatalf("dm xml: %s", r.body)
	}
	r = h.post(fmt.Sprintf("/direct_messages/destroy/%d.json", dm.ID), nil, basic("bob.test", bobDM))
	if r.code != 200 {
		t.Fatalf("destroy dm: %d %s", r.code, r.body)
	}
	h.get("/direct_messages.json", basic("bob.test", bobDM)).json(t, &list)
	if len(list) != 0 {
		t.Fatalf("deleted DM still listed: %+v", list)
	}
}

func TestSearchHostAndFormats(t *testing.T) {
	h := newHarness(t)
	h.seed()
	r := h.get("/search.json?q=bob&rpp=5", host("search.twitter.com"), alice)
	if r.code != 200 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	var sr struct {
		Results []struct {
			Text            string `json:"text"`
			FromUser        string `json:"from_user"`
			FromUserID      int64  `json:"from_user_id"`
			ToUserID        *int64 `json:"to_user_id"`
			ProfileImageURL string `json:"profile_image_url"`
			CreatedAt       string `json:"created_at"`
			ID              int64  `json:"id"`
			ISOLanguageCode string `json:"iso_language_code"`
			Source          string `json:"source"`
		} `json:"results"`
		MaxID          int64   `json:"max_id"`
		RefreshURL     string  `json:"refresh_url"`
		ResultsPerPage int     `json:"results_per_page"`
		Page           int     `json:"page"`
		Query          string  `json:"query"`
		CompletedIn    float64 `json:"completed_in"`
	}
	r.json(t, &sr)
	if len(sr.Results) != 2 || sr.Results[0].FromUser != "bob.test" || sr.ResultsPerPage != 5 || sr.Page != 1 || sr.Query != "bob" {
		t.Fatalf("search: %s", r.body)
	}
	if _, err := time.Parse("Mon, 02 Jan 2006 15:04:05 -0700", sr.Results[0].CreatedAt); err != nil {
		t.Fatalf("search created_at must be RFC 822 style: %q", sr.Results[0].CreatedAt)
	}
	if !strings.HasPrefix(sr.RefreshURL, "?since_id=") || sr.MaxID != sr.Results[0].ID {
		t.Fatalf("refresh_url / max_id: %s", r.body)
	}
	if strings.Contains(sr.Results[0].Source, "<") {
		t.Fatalf("search source must be entity-escaped: %q", sr.Results[0].Source)
	}
	r = h.get("/search.atom?q=bob", host("search.twitter.com"), alice)
	if r.code != 200 || !strings.Contains(string(r.body), "<feed") {
		t.Fatalf("atom: %d %s", r.code, r.body)
	}
	// JSONP
	r = h.get("/search.json?q=bob&callback=handle_results", alice)
	if !strings.HasPrefix(string(r.body), "/**/handle_results({") || !strings.HasPrefix(r.hdr.Get("Content-Type"), "text/javascript") {
		t.Fatalf("jsonp: %s", r.body)
	}
	r = h.get("/search.json?q=bob&callback=alert(1)//", alice)
	if r.code != 400 || strings.Contains(string(r.body), "alert(1)") {
		t.Fatalf("bad callback accepted: %d %s", r.code, r.body)
	}
	// Anonymous search is allowed.
	r = h.get("/search.json?q=bob", host("search.twitter.com"))
	if r.code != 200 {
		t.Fatalf("anonymous search: %d", r.code)
	}
	r = h.get("/trends.json", host("search.twitter.com"))
	if r.code != 200 || !strings.Contains(string(r.body), `"trends":[{"name":"Bluesky"`) {
		t.Fatalf("trends: %s", r.body)
	}
	r = h.get("/trends/current.json")
	if !strings.Contains(string(r.body), `"as_of":`) || !strings.Contains(string(r.body), `"query":"Bluesky"`) {
		t.Fatalf("trends/current: %s", r.body)
	}
}

func TestMiscEndpoints(t *testing.T) {
	h := newHarness(t)
	if r := h.get("/help/test.json"); string(r.body) != `"ok"` {
		t.Fatalf("help/test json: %s", r.body)
	}
	if r := h.get("/help/test.xml"); !strings.Contains(string(r.body), "<ok>true</ok>") {
		t.Fatalf("help/test xml: %s", r.body)
	}
	r := h.get("/account/rate_limit_status.xml", alice)
	if !strings.Contains(string(r.body), `<hash><hourly-limit type="integer">`) || !strings.Contains(string(r.body), `<reset-time type="datetime">`) {
		t.Fatalf("rate limit xml: %s", r.body)
	}
	r = h.get("/statuses/lists/nonexistent.json", alice)
	if r.code != 404 || !strings.Contains(string(r.body), `"request":"/statuses/lists/nonexistent.json"`) {
		t.Fatalf("unknown endpoint: %d %s", r.code, r.body)
	}
	if n := h.api.Unknown()["endpoint GET /statuses/lists/nonexistent.json"]; n != 1 {
		t.Fatalf("unknown endpoint not recorded: %v", h.api.Unknown())
	}
	r = h.post("/saved_searches/create.json", url.Values{"query": {"bluesky"}}, alice)
	if r.code != 200 {
		t.Fatalf("saved search: %d %s", r.code, r.body)
	}
	r = h.get("/saved_searches.xml", alice)
	if !strings.Contains(string(r.body), "<saved_searches type=\"array\"><saved_search><id>1</id><name>bluesky</name><query>bluesky</query><position/>") {
		t.Fatalf("saved searches xml: %s", r.body)
	}
	// Gzip for clients that accept it.
	r = h.get("/statuses/public_timeline.json", header("Accept-Encoding", "gzip"))
	if r.code != 200 {
		t.Fatalf("public timeline: %d %s", r.code, r.body)
	}
	h.seed()
	r = h.get("/statuses/public_timeline.json", header("Accept-Encoding", "gzip"))
	if r.hdr.Get("Content-Encoding") != "gzip" {
		t.Fatalf("expected gzip: %v", r.hdr)
	}
}

func TestWebPages(t *testing.T) {
	h := newHarness(t)
	h.seed()
	r := h.get("/")
	if r.code != 200 || !strings.Contains(string(r.body), "only accepts Bluesky app passwords") || strings.Contains(string(r.body), "<script") {
		t.Fatalf("home: %d", r.code)
	}
	var sts []tStatus
	h.get("/statuses/home_timeline.json", alice).json(t, &sts)
	r = h.get(fmt.Sprintf("/p/%d", sts[1].ID))
	if r.code != 200 || !strings.Contains(string(r.body), "alice says hi &amp; &lt;waves&gt;") {
		t.Fatalf("post page must escape text: %d %s", r.code, r.body)
	}
	r = h.get(fmt.Sprintf("/bob/status/%d", sts[0].ID), host("twitter.com"))
	if r.code != 302 || r.hdr.Get("Location") != fmt.Sprintf("/p/%d", sts[0].ID) {
		t.Fatalf("twitter.com status link redirect: %d %v", r.code, r.hdr)
	}
	r = h.get("/theme?set=dark&r=//evil.example/")
	if r.hdr.Get("Location") != "/" {
		t.Fatalf("open redirect via theme: %v", r.hdr.Get("Location"))
	}
	r = h.get("/metrics")
	if r.code != 404 {
		t.Fatalf("metrics must not be public: %d", r.code)
	}
}

func TestOAuthFlows(t *testing.T) {
	h := newHarness(t)
	h.seed()
	// xAuth
	r := h.post("/oauth/access_token", url.Values{"x_auth_username": {"alice.test"}, "x_auth_password": {alicePW}, "x_auth_mode": {"client_auth"}},
		header("Authorization", `OAuth oauth_consumer_key="tweetie", oauth_nonce="n", oauth_signature="x", oauth_signature_method="HMAC-SHA1", oauth_timestamp="1", oauth_version="1.0"`))
	if r.code != 200 {
		t.Fatalf("xauth: %d %s", r.code, r.body)
	}
	vals, err := url.ParseQuery(string(r.body))
	if err != nil || vals.Get("screen_name") != "alice.test" || vals.Get("user_id") == "" || !strings.HasPrefix(vals.Get("oauth_token"), vals.Get("user_id")+"-") || vals.Get("oauth_token_secret") == "" {
		t.Fatalf("xauth body: %s", r.body)
	}
	tok := vals.Get("oauth_token")
	r = h.get("/statuses/home_timeline.json", header("Authorization", `OAuth oauth_consumer_key="tweetie", oauth_token="`+tok+`", oauth_signature="whatever"`))
	if r.code != 200 {
		t.Fatalf("bearer oauth token: %d %s", r.code, r.body)
	}
	r = h.get("/statuses/home_timeline.json", header("Authorization", `OAuth oauth_token="`+tok+`x"`))
	if r.code != 401 {
		t.Fatalf("bad token accepted: %d", r.code)
	}

	// Three-legged OAuth 1.0a
	r = h.post("/oauth/request_token", url.Values{"oauth_consumer_key": {"someapp"}, "oauth_callback": {"myapp://done"}})
	vals, _ = url.ParseQuery(string(r.body))
	rt := vals.Get("oauth_token")
	if rt == "" || vals.Get("oauth_callback_confirmed") != "true" {
		t.Fatalf("request_token: %s", r.body)
	}
	r = h.get("/oauth/authorize?oauth_token=" + rt)
	if r.code != 200 || !strings.Contains(string(r.body), `name="password"`) || strings.Contains(string(r.body), "<script") {
		t.Fatalf("authorize form: %d %s", r.code, r.body)
	}
	r = h.post("/oauth/authorize", url.Values{"oauth_token": {rt}, "handle": {"alice.test"}, "password": {"wrong"}})
	if r.code != 200 || !strings.Contains(string(r.body), "did not work") {
		t.Fatalf("authorize bad password: %d %s", r.code, r.body)
	}
	r = h.post("/oauth/authorize", url.Values{"oauth_token": {rt}, "handle": {"alice.test"}, "password": {alicePW}})
	if r.code != 302 {
		t.Fatalf("authorize: %d %s", r.code, r.body)
	}
	loc, _ := url.Parse(r.hdr.Get("Location"))
	if loc.Scheme != "myapp" || loc.Query().Get("oauth_token") != rt || len(loc.Query().Get("oauth_verifier")) != 7 {
		t.Fatalf("callback: %s", r.hdr.Get("Location"))
	}
	r = h.post("/oauth/access_token", url.Values{"oauth_consumer_key": {"someapp"}, "oauth_token": {rt}, "oauth_verifier": {"0000000"}})
	if r.code != 401 {
		t.Fatalf("wrong verifier accepted: %d %s", r.code, r.body)
	}
	r = h.post("/oauth/access_token", url.Values{"oauth_consumer_key": {"someapp"}, "oauth_token": {rt}, "oauth_verifier": {loc.Query().Get("oauth_verifier")}})
	if r.code != 200 || !strings.Contains(string(r.body), "screen_name=alice.test") {
		t.Fatalf("access_token: %d %s", r.code, r.body)
	}
	// Request tokens are single use.
	r = h.post("/oauth/access_token", url.Values{"oauth_consumer_key": {"someapp"}, "oauth_token": {rt}, "oauth_verifier": {loc.Query().Get("oauth_verifier")}})
	if r.code != 401 {
		t.Fatalf("request token reused: %d", r.code)
	}
}

func TestPostPageRespectsLoggedOutVisibility(t *testing.T) {
	h := newHarness(t)
	h.pds.AddAccount(&fakepds.Account{DID: "did:plc:shy", Handle: "shy.test", AppPassword: "shyy-shyy-shyy-shyy", Labels: []string{"!no-unauthenticated"}})
	h.pds.AddPost("did:plc:shy", "only for signed-in readers", time.Now())
	var sts []tStatus
	h.get("/statuses/user_timeline/shy.test.json", alice).json(t, &sts)
	if len(sts) != 1 {
		t.Fatalf("timeline: %+v", sts)
	}
	r := h.get(fmt.Sprintf("/p/%d", sts[0].ID))
	if r.code != 403 || strings.Contains(string(r.body), "only for signed-in readers") {
		t.Fatalf("hidden author's post was shown publicly: %d", r.code)
	}
}

func TestTwitPicUploadThenPost(t *testing.T) {
	h := newHarness(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("username", "alice.test")
	mw.WriteField("password", alicePW)
	fw, _ := mw.CreateFormFile("media", "photo.jpg")
	img := image.NewRGBA(image.Rect(0, 0, 640, 480))
	jpeg.Encode(fw, img, nil)
	mw.Close()
	req, _ := http.NewRequest("POST", h.srv.URL+"/api/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var rsp struct {
		Stat     string `xml:"stat,attr"`
		MediaID  string `xml:"mediaid"`
		MediaURL string `xml:"mediaurl"`
	}
	if err := xml.Unmarshal(body, &rsp); err != nil || rsp.Stat != "ok" || !strings.HasPrefix(rsp.MediaURL, "http://bird.test/m/") {
		t.Fatalf("upload: %d %s", resp.StatusCode, body)
	}
	r := h.post("/statuses/update.json", url.Values{"status": {"look at this " + rsp.MediaURL}}, alice)
	if r.code != 200 {
		t.Fatalf("update: %d %s", r.code, r.body)
	}
	p := h.pds.Posts()[0]
	if p.Text != "look at this" {
		t.Fatalf("upload URL should be stripped from the text: %q", p.Text)
	}
	embed, _ := p.Embed["$type"].(string)
	images, _ := p.Embed["images"].([]any)
	if embed != "app.bsky.embed.images" || len(images) != 1 {
		t.Fatalf("image embed not attached: %+v", p.Embed)
	}
	ar := images[0].(map[string]any)["aspectRatio"].(map[string]any)
	if ar["width"].(float64) != 640 || ar["height"].(float64) != 480 {
		t.Fatalf("aspect ratio: %+v", ar)
	}
	// Someone else cannot attach alice's upload.
	r = h.post("/statuses/update.json", url.Values{"status": {"stolen " + rsp.MediaURL}}, basic("bob.test", bobPW))
	if r.code != 200 || h.pds.Posts()[0].Embed != nil {
		t.Fatalf("another user's upload was attached: %+v", h.pds.Posts()[0].Embed)
	}
	// Bad credentials use TwitPic's error format.
	var buf2 bytes.Buffer
	mw = multipart.NewWriter(&buf2)
	mw.WriteField("username", "alice.test")
	mw.WriteField("password", "wrong")
	mw.Close()
	req, _ = http.NewRequest("POST", h.srv.URL+"/api/upload", &buf2)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, _ = http.DefaultClient.Do(req)
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `<rsp stat="fail"><err code="1001"`) {
		t.Fatalf("twitpic error: %s", body)
	}
}

func TestIdenticalConcurrentRequestsCoalesce(t *testing.T) {
	h := newHarness(t)
	h.seed()
	h.get("/account/verify_credentials.json", alice) // log in first
	h.pds.Latency = 150 * time.Millisecond
	before := h.pds.CallCount("app.bsky.feed.getTimeline")
	done := make(chan int, 5)
	for i := 0; i < 5; i++ {
		go func() { done <- h.get("/statuses/home_timeline.json?count=5", alice).code }()
	}
	for i := 0; i < 5; i++ {
		if code := <-done; code != 200 {
			t.Fatalf("status %d", code)
		}
	}
	if n := h.pds.CallCount("app.bsky.feed.getTimeline") - before; n != 1 {
		t.Fatalf("5 identical concurrent requests made %d upstream timeline calls, want 1", n)
	}
}
