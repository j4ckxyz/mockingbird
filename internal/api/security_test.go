package api_test

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/j4ckxyz/mockingbird/internal/fakepds"
)

// An unverified app's web callback is never followed automatically: the
// user sees where they are being sent and must confirm.
func TestOAuthWebCallbackNeedsConfirmation(t *testing.T) {
	h := newHarness(t)
	r := h.post("/oauth/request_token", url.Values{"oauth_consumer_key": {"someapp"}, "oauth_callback": {"https://evil.example/cb"}})
	vals, _ := url.ParseQuery(string(r.body))
	rt := vals.Get("oauth_token")
	if rt == "" {
		t.Fatalf("request_token: %s", r.body)
	}
	r = h.get("/oauth/authorize?oauth_token=" + rt)
	if r.code != 200 || !strings.Contains(string(r.body), "sent to <b>evil.example</b>") {
		t.Fatalf("authorize form must show the destination: %d %s", r.code, r.body)
	}
	r = h.post("/oauth/authorize", url.Values{"oauth_token": {rt}, "handle": {"alice.test"}, "password": {alicePW}})
	body := string(r.body)
	if r.code != 200 || r.hdr.Get("Location") != "" || !strings.Contains(body, "Continue to evil.example") ||
		!strings.Contains(body, `href="https://evil.example/cb?oauth_token=`) {
		t.Fatalf("unverified web callback redirected without confirmation: %d %s %s", r.code, r.hdr.Get("Location"), body)
	}
	// The token cannot be authorized a second time (which could swap in a
	// different account before the app exchanges it).
	r = h.post("/oauth/authorize", url.Values{"oauth_token": {rt}, "handle": {"bob.test"}, "password": {bobPW}})
	if !strings.Contains(string(r.body), "already been used") {
		t.Fatalf("second authorization allowed: %d %s", r.code, r.body)
	}
}

func TestOAuthRejectsScriptCallbacks(t *testing.T) {
	h := newHarness(t)
	for _, cb := range []string{"javascript:alert(1)", "JavaScript:alert(1)", "data:text/html,x", "vbscript:x", "/relative"} {
		r := h.post("/oauth/request_token", url.Values{"oauth_consumer_key": {"someapp"}, "oauth_callback": {cb}})
		vals, _ := url.ParseQuery(string(r.body))
		r = h.get("/oauth/authorize?oauth_token=" + vals.Get("oauth_token"))
		if !strings.Contains(string(r.body), "invalid callback") || strings.Contains(string(r.body), `name="password"`) {
			t.Fatalf("callback %q accepted: %s", cb, r.body)
		}
	}
}

// Wrong verifiers burn the request token, so the 7-digit PIN cannot be
// brute-forced, and a verifier is always required.
func TestOAuthVerifierGuessingBurnsToken(t *testing.T) {
	h := newHarness(t)
	r := h.post("/oauth/request_token", url.Values{"oauth_consumer_key": {"someapp"}, "oauth_callback": {"oob"}})
	vals, _ := url.ParseQuery(string(r.body))
	rt := vals.Get("oauth_token")
	// Before sign-in there is nothing to exchange, verifier or not.
	r = h.post("/oauth/access_token", url.Values{"oauth_consumer_key": {"someapp"}, "oauth_token": {rt}})
	if r.code != 401 {
		t.Fatalf("unauthorized token exchanged: %d %s", r.code, r.body)
	}
	r = h.post("/oauth/authorize", url.Values{"oauth_token": {rt}, "handle": {"alice.test"}, "password": {alicePW}})
	body := string(r.body)
	i := strings.Index(body, `<div class="pin">`)
	if i < 0 {
		t.Fatalf("no PIN: %s", body)
	}
	pin := body[i+len(`<div class="pin">`) : i+len(`<div class="pin">`)+7]
	for _, v := range []string{"", "0000000", "1111111"} {
		r = h.post("/oauth/access_token", url.Values{"oauth_consumer_key": {"someapp"}, "oauth_token": {rt}, "oauth_verifier": {v}})
		if r.code != 401 {
			t.Fatalf("wrong verifier %q accepted: %d", v, r.code)
		}
	}
	r = h.post("/oauth/access_token", url.Values{"oauth_consumer_key": {"someapp"}, "oauth_token": {rt}, "oauth_verifier": {pin}})
	if r.code != 401 {
		t.Fatalf("token survived %d wrong verifiers: %d %s", 3, r.code, r.body)
	}
}

// A bare name that could mean more than one account is refused for actions
// that reach another account, but still resolves for reading.
func TestAmbiguousBareNames(t *testing.T) {
	h := newHarnessWithAccounts(t, []*fakepds.Account{
		{DID: "did:plc:dave", Handle: "dave.test", DisplayName: "Dave"},
		{DID: "did:plc:mallory", Handle: "dave.evil.example", DisplayName: "Dave"},
	})
	// Alice sees the look-alike, e.g. in a thread.
	if r := h.get("/users/show/dave.evil.example.json", alice); r.code != 200 {
		t.Fatalf("show: %d %s", r.code, r.body)
	}
	var u struct {
		ScreenName string `json:"screen_name"`
	}
	h.get("/users/show/dave.json", alice).json(t, &u)
	if u.ScreenName != "dave.evil.example" {
		t.Fatalf("reading a bare name should use the recent handle: %q", u.ScreenName)
	}
	r := h.post("/friendships/create.json", url.Values{"screen_name": {"dave"}}, alice)
	if r.code != 403 || !strings.Contains(string(r.body), "Use the full handle") {
		t.Fatalf("ambiguous follow allowed: %d %s", r.code, r.body)
	}
	r = h.post("/direct_messages/new.json", url.Values{"user": {"dave"}, "text": {"hi"}}, basic("alice.test", aliceDM))
	if r.code != 403 || !strings.Contains(string(r.body), "Use the full handle") {
		t.Fatalf("ambiguous DM allowed: %d %s", r.code, r.body)
	}
	// Unambiguous: only carol.other.example exists and has been seen.
	h.get("/users/show/carol.other.example.json", alice)
	r = h.post("/friendships/create.json", url.Values{"screen_name": {"carol"}}, alice)
	if r.code != 200 || !strings.Contains(string(r.body), `"screen_name":"carol.other.example"`) {
		t.Fatalf("unambiguous bare name: %d %s", r.code, r.body)
	}
}

// Uploads outlive the server's read timeout: a photo over EDGE takes far
// longer than the 30 seconds allowed for ordinary requests.
func TestSlowUploadOutlivesReadTimeout(t *testing.T) {
	h := newHarness(t)
	srv := httptest.NewUnstartedServer(h.api)
	srv.Config.ReadTimeout = 300 * time.Millisecond
	srv.Start()
	defer srv.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("message", "slow")
	fw, _ := mw.CreateFormFile("media", "a.jpg")
	fw.Write(bytes.Repeat([]byte{0}, 4096))
	mw.Close()
	payload := body.Bytes()

	pr, pw := io.Pipe()
	go func() {
		// Dribble the body out over about a second.
		for i := 0; i < len(payload); i += len(payload)/8 + 1 {
			pw.Write(payload[i:min(i+len(payload)/8+1, len(payload))])
			time.Sleep(125 * time.Millisecond)
		}
		pw.Close()
	}()
	req, _ := http.NewRequest("POST", srv.URL+"/api/upload", pr)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	// The whole body was read, so the failure is about credentials (1001),
	// not a truncated upload (1004).
	if !strings.Contains(string(b), `code="1001"`) {
		t.Fatalf("slow upload was cut off: %s", b)
	}
}

// Posts show the client named in their record's "via" field, and posts made
// through the bridge are marked as coming from Tweetie.
func TestViaSource(t *testing.T) {
	h := newHarness(t)
	p := h.pds.AddPost("did:plc:bob", "posted from elsewhere", time.Now())
	p.Via = "Witchsky Web App"
	h.pds.AddPost("did:plc:bob", "no via here", time.Now().Add(time.Second))

	// Anonymous user timeline.
	var sts []struct {
		Text   string `json:"text"`
		Source string `json:"source"`
	}
	h.get("/statuses/user_timeline/bob.test.json").json(t, &sts)
	got := map[string]string{}
	for _, s := range sts {
		got[s.Text] = s.Source
	}
	if got["posted from elsewhere"] != "Witchsky Web App" || got["no via here"] != `<a href="https://bsky.app" rel="nofollow">Bluesky</a>` {
		t.Fatalf("sources: %v", got)
	}
	r := h.get("/statuses/user_timeline/bob.test.xml")
	if !strings.Contains(string(r.body), "<source>Witchsky Web App</source>") {
		t.Fatalf("xml source: %s", r.body)
	}

	// Posting through the bridge writes via: Tweetie.
	r = h.post("/statuses/update.json", url.Values{"status": {"hello from 2009"}}, alice)
	if r.code != 200 || !strings.Contains(string(r.body), `"source":"Tweetie"`) {
		t.Fatalf("update: %d %s", r.code, r.body)
	}
	var mine *fakepds.Post
	for _, q := range h.pds.Posts() {
		if q.Text == "hello from 2009" {
			mine = q
		}
	}
	if mine == nil || mine.Via != "Tweetie" {
		t.Fatalf("record via not written: %+v", mine)
	}
}
