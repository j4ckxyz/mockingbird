// Package integration runs the full bridge against a real Bluesky account.
//
// Set MB_TEST_HANDLE and MB_TEST_APP_PASSWORD (an app password, never the
// account password) to enable. MB_TEST_ALLOW_WRITES=1 additionally posts,
// likes and deletes a test post. MB_TEST_DM_APP_PASSWORD (a DM-enabled app
// password) enables the DM read test.
package integration

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackgilbert/mockingbird/internal/app"
	"github.com/jackgilbert/mockingbird/internal/config"
)

type env struct {
	t      *testing.T
	srv    *httptest.Server
	handle string
	auth   string
}

func setup(t *testing.T) *env {
	handle, pw := os.Getenv("MB_TEST_HANDLE"), os.Getenv("MB_TEST_APP_PASSWORD")
	if handle == "" || pw == "" {
		t.Skip("MB_TEST_HANDLE / MB_TEST_APP_PASSWORD not set")
	}
	t.Setenv("MB_SECRET_KEY", strings.Repeat("5a", 32))
	t.Setenv("MB_ENCRYPTION_KEY", strings.Repeat("a5", 32))
	t.Setenv("MB_DATA_DIR", t.TempDir())
	t.Setenv("MB_PUBLIC_URL", "http://bird.integration.test")
	t.Setenv("MB_ADMIN_ADDR", "127.0.0.1:0")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.New(cfg, app.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	srv := httptest.NewServer(a.Handler)
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, handle: handle, auth: "Basic " + base64.StdEncoding.EncodeToString([]byte(handle+":"+pw))}
}

func (e *env) call(method, path string, form url.Values, auth string) (int, []byte) {
	e.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (e *env) ok(path string, out any) []byte {
	e.t.Helper()
	code, b := e.call("GET", path, nil, e.auth)
	if code != 200 {
		e.t.Fatalf("GET %s: %d %s", path, code, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			e.t.Fatalf("GET %s: bad JSON: %v", path, err)
		}
	}
	return b
}

func TestIntegrationReadOnly(t *testing.T) {
	e := setup(t)
	var me struct {
		ID         int64  `json:"id"`
		ScreenName string `json:"screen_name"`
	}
	e.ok("/account/verify_credentials.json", &me)
	if !strings.EqualFold(me.ScreenName, strings.TrimPrefix(e.handle, "@")) && !strings.HasPrefix(e.handle, "did:") {
		t.Fatalf("screen_name %q for handle %q", me.ScreenName, e.handle)
	}
	var home []struct {
		ID   int64  `json:"id"`
		Text string `json:"text"`
	}
	e.ok("/statuses/home_timeline.json?count=20", &home)
	t.Logf("home timeline: %d statuses", len(home))
	if len(home) > 0 {
		var since []json.RawMessage
		e.ok(fmt.Sprintf("/statuses/home_timeline.json?since_id=%d", home[0].ID), &since)
		t.Logf("since_id poll: %d new", len(since))
		e.ok(fmt.Sprintf("/statuses/show/%d.json", home[0].ID), nil)
		e.ok(fmt.Sprintf("/statuses/home_timeline.json?max_id=%d&count=5", home[len(home)-1].ID-1), nil)
	}
	xml := e.ok("/1/statuses/home_timeline.xml?count=5", nil)
	if !strings.Contains(string(xml), `<statuses type="array">`) {
		t.Fatalf("xml: %.300s", xml)
	}
	e.ok("/statuses/mentions.json", nil)
	e.ok("/statuses/user_timeline/bsky.app.json?count=5", nil)
	e.ok("/users/show/bsky.app.json", nil)
	e.ok("/friendships/show.json?target_screen_name=bsky.app", nil)
	e.ok("/friends/ids.json?cursor=-1", nil)
	e.ok("/search.json?q=bluesky&rpp=5", nil)
	e.ok("/trends/current.json", nil)
	e.ok("/favorites.json", nil)
	e.ok("/direct_messages.json", nil) // empty without a DM-enabled app password
	e.ok("/account/rate_limit_status.json", nil)

	// xAuth then OAuth-bearer access.
	code, body := e.call("POST", "/oauth/access_token", url.Values{"x_auth_mode": {"client_auth"},
		"x_auth_username": {e.handle}, "x_auth_password": {os.Getenv("MB_TEST_APP_PASSWORD")}}, "")
	if code != 200 {
		t.Fatalf("xauth: %d %s", code, body)
	}
	vals, _ := url.ParseQuery(string(body))
	code, body = e.call("GET", "/account/verify_credentials.json", nil, `OAuth oauth_consumer_key="integration", oauth_token="`+vals.Get("oauth_token")+`"`)
	if code != 200 {
		t.Fatalf("oauth bearer: %d %s", code, body)
	}
}

func TestIntegrationDMs(t *testing.T) {
	dm := os.Getenv("MB_TEST_DM_APP_PASSWORD")
	if dm == "" {
		t.Skip("MB_TEST_DM_APP_PASSWORD not set")
	}
	e := setup(t)
	e.auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(e.handle+":"+dm))
	e.ok("/direct_messages.json", nil)
	e.ok("/direct_messages/sent.json", nil)
}

func TestIntegrationWrites(t *testing.T) {
	if os.Getenv("MB_TEST_ALLOW_WRITES") != "1" {
		t.Skip("MB_TEST_ALLOW_WRITES != 1")
	}
	e := setup(t)
	text := fmt.Sprintf("mockingbird integration test %d, will be deleted #mockingbirdtest https://example.com", time.Now().Unix())
	code, body := e.call("POST", "/statuses/update.json", url.Values{"status": {text}}, e.auth)
	if code != 200 {
		t.Fatalf("update: %d %s", code, body)
	}
	var st struct {
		ID   int64  `json:"id"`
		Text string `json:"text"`
	}
	json.Unmarshal(body, &st)
	defer func() {
		if code, body := e.call("POST", fmt.Sprintf("/statuses/destroy/%d.json", st.ID), nil, e.auth); code != 200 {
			t.Errorf("destroy: %d %s", code, body)
		}
	}()
	if code, body := e.call("POST", fmt.Sprintf("/favorites/create/%d.json", st.ID), nil, e.auth); code != 200 {
		t.Fatalf("favorite: %d %s", code, body)
	}
	if code, body := e.call("POST", fmt.Sprintf("/favorites/destroy/%d.json", st.ID), nil, e.auth); code != 200 {
		t.Fatalf("unfavorite: %d %s", code, body)
	}
	code, body = e.call("POST", "/statuses/update.json", url.Values{"status": {"reply from mockingbird integration test"},
		"in_reply_to_status_id": {fmt.Sprint(st.ID)}}, e.auth)
	if code != 200 {
		t.Fatalf("reply: %d %s", code, body)
	}
	var reply struct {
		ID                int64  `json:"id"`
		InReplyToStatusID *int64 `json:"in_reply_to_status_id"`
	}
	json.Unmarshal(body, &reply)
	defer e.call("POST", fmt.Sprintf("/statuses/destroy/%d.json", reply.ID), nil, e.auth)
	if reply.InReplyToStatusID == nil || *reply.InReplyToStatusID != st.ID {
		t.Fatalf("reply linkage: %s", body)
	}
}
