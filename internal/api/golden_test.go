package api_test

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackgilbert/mockingbird/internal/fakepds"
)

var update = flag.Bool("update", false, "rewrite testdata/golden snapshots")

// Golden tests compare the bridge's output with responses archived from
// the 2010 Twitter API documentation (testdata/reference, extracted by
// scripts/extract_refs.py):
//
//   - JSON: every key the reference has must exist with the same JSON type
//     (null in either side is accepted, since nullability is per value);
//     nested objects and the first array element are compared recursively.
//   - XML: at each level, the reference's element names must appear in our
//     output in the same relative order; the first child of each name is
//     compared recursively.
//
// Fields that were opt-in or out of scope in 2010 are listed in optional.
// A second layer snapshots our exact bytes in testdata/golden to catch
// unintended changes (go test ./internal/api -run Golden -update).

var optional = map[string]bool{
	"entities": true, "annotations": true, // include_entities / annotations were opt-in
	"georss:point":            true, // geo points: Bluesky has none
	"coordinates.coordinates": true, "coordinates.type": true, "geo.coordinates": true, "geo.type": true,
	"retweeted_status": true, // present only on retweets
	"status":           true, // embedded latest status is omitted when a user has not posted
	// Search API fields that appear only for some results or pages.
	"next_page": true, "to_user": true, "metadata.recent_retweets": true,
}

func refPath(name string) string { return filepath.Join("..", "..", "testdata", "reference", name) }

func loadRef(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(refPath(name))
	if err != nil {
		t.Skipf("reference %s not available: %v", name, err)
	}
	return b
}

func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "?"
}

func compareJSON(t *testing.T, path string, ref, got any) {
	t.Helper()
	rk, gk := jsonKind(ref), jsonKind(got)
	if rk == "null" || gk == "null" {
		return
	}
	if rk != gk {
		t.Errorf("%s: type %s in reference, %s in bridge output", path, rk, gk)
		return
	}
	switch r := ref.(type) {
	case map[string]any:
		g := got.(map[string]any)
		if datedKeys(r) && datedKeys(g) {
			// trends/current etc. key their lists by timestamp.
			compareJSON(t, path+"[date]", firstValue(r), firstValue(g))
			return
		}
		for k, rv := range r {
			key := strings.TrimPrefix(path+"."+k, ".")
			short := k
			if i := strings.LastIndexByte(key, '.'); i >= 0 {
				parent := key[:i]
				if j := strings.LastIndexByte(parent, '.'); j >= 0 {
					parent = parent[j+1:]
				}
				short = parent + "." + k
			}
			gv, ok := g[k]
			if !ok {
				if !optional[k] && !optional[short] {
					t.Errorf("%s: missing from bridge output", key)
				}
				continue
			}
			compareJSON(t, key, rv, gv)
		}
	case []any:
		g := got.([]any)
		if len(r) > 0 && len(g) > 0 {
			compareJSON(t, path+"[0]", r[0], g[0])
		}
	}
}

var dateKey = regexp.MustCompile(`^\d{4}-\d\d-\d\d( \d\d:\d\d(:\d\d)?)?$`)

func datedKeys(m map[string]any) bool {
	if len(m) == 0 {
		return false
	}
	for k := range m {
		if !dateKey.MatchString(k) {
			return false
		}
	}
	return true
}

func firstValue(m map[string]any) any {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return m[keys[0]]
}

type xnode struct {
	name     string
	children []*xnode
}

func parseXML(t *testing.T, b []byte) *xnode {
	t.Helper()
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = false
	root := &xnode{}
	stack := []*xnode{root}
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("XML parse: %v\n%s", err, b)
		}
		switch e := tok.(type) {
		case xml.StartElement:
			name := e.Name.Local
			if e.Name.Space != "" && !strings.HasPrefix(e.Name.Space, "http") {
				name = e.Name.Space + ":" + name
			}
			n := &xnode{name: name}
			top := stack[len(stack)-1]
			top.children = append(top.children, n)
			stack = append(stack, n)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		}
	}
	if len(root.children) != 1 {
		t.Fatalf("XML must have one root, got %d", len(root.children))
	}
	return root.children[0]
}

func uniqueNames(n *xnode) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range n.children {
		if !seen[c.name] {
			seen[c.name] = true
			out = append(out, c.name)
		}
	}
	return out
}

func firstChild(n *xnode, name string) *xnode {
	for _, c := range n.children {
		if c.name == name {
			return c
		}
	}
	return nil
}

// compareXML checks reference element order is a subsequence of ours.
func compareXML(t *testing.T, path string, ref, got *xnode, orderMatters bool) {
	t.Helper()
	if ref.name != got.name {
		t.Errorf("%s: element <%s> in reference, <%s> in bridge output", path, ref.name, got.name)
		return
	}
	gnames := uniqueNames(got)
	pos := map[string]int{}
	for i, n := range gnames {
		pos[n] = i
	}
	last := -1
	for _, rn := range uniqueNames(ref) {
		p, ok := pos[rn]
		if !ok {
			if !optional[rn] {
				t.Errorf("%s/%s: missing from bridge output", path, rn)
			}
			continue
		}
		if orderMatters && p < last {
			t.Errorf("%s/%s: out of order (reference order %v, bridge order %v)", path, rn, uniqueNames(ref), gnames)
		}
		last = max(last, p)
		rc, gc := firstChild(ref, rn), firstChild(got, rn)
		if len(rc.children) > 0 && len(gc.children) > 0 {
			compareXML(t, path+"/"+rn, rc, gc, orderMatters)
		}
	}
}

// goldenHarness builds a deterministic world: fixed times and accounts.
func goldenHarness(t *testing.T) *harness {
	fixed := time.Date(2010, 7, 15, 22, 31, 11, 0, time.UTC)
	harnessNow = func() time.Time { return fixed }
	t.Cleanup(func() { harnessNow = nil })
	h := newHarness(t)
	base := time.Date(2010, 6, 22, 17, 48, 26, 0, time.UTC)
	p1 := h.pds.AddPost("did:plc:bob", "sure thing. Meet you at the mall around 7? https://example.com/mall", base)
	h.pds.AddPost("did:plc:alice", "got a lovely surprise from @bob.test. She sent me the best tshirt ever.", base.Add(time.Hour))
	h.pds.AddPost("did:plc:bob", "replying to myself", base.Add(2*time.Hour))
	_ = p1
	return h
}

func (h *harness) fetch(t *testing.T, path string, auth reqOpt) []byte {
	t.Helper()
	r := h.get(path, auth)
	if r.code != 200 {
		t.Fatalf("%s: %d %s", path, r.code, r.body)
	}
	return r.body
}

var dynamic = regexp.MustCompile(`(reset_time_in_seconds"?:\s*|reset-time-in-seconds type="integer">|"completed_in":)[0-9.]+|("reset_time":|<reset-time type="datetime">)[^<,}]+|(Sat|Sun|Mon|Tue|Wed|Thu|Fri) [A-Z][a-z]{2} \d\d \d\d:\d\d:\d\d \+0000 20[2-9]\d|"as_of":[^,}]+`)

func snapshot(t *testing.T, name string, body []byte) {
	t.Helper()
	body = dynamic.ReplaceAll(body, []byte("${1}${2}<dynamic>"))
	p := filepath.Join("..", "..", "testdata", "golden", name)
	if *update {
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("missing snapshot %s (run with -update): %v", name, err)
	}
	if !bytes.Equal(want, body) {
		t.Errorf("snapshot %s differs.\nwant: %s\ngot:  %s", name, want, body)
	}
}

type goldenCase struct {
	ref    string
	path   string
	method string
	form   url.Values
	auth   func(h *harness) reqOpt
	setup  func(t *testing.T, h *harness)
	order  bool
}

func TestGoldenAgainstArchivedReferences(t *testing.T) {
	aliceAuth := func(*harness) reqOpt { return alice }
	dmAuth := func(*harness) reqOpt { return basic("bob.test", bobDM) }
	sendDM := func(t *testing.T, h *harness) {
		r := h.post("/direct_messages/new.json", url.Values{"user": {"bob.test"}, "text": {"sure thing. Meet you at the mall around 7?"}}, basic("alice.test", aliceDM))
		if r.code != 200 {
			t.Fatalf("send DM: %d %s", r.code, r.body)
		}
	}
	block := func(t *testing.T, h *harness) {
		if r := h.post("/blocks/create/carol.other.example.json", nil, alice); r.code != 200 {
			t.Fatalf("block: %d %s", r.code, r.body)
		}
	}
	retweet := func(t *testing.T, h *harness) {
		var sts []struct{ ID int64 }
		json.Unmarshal(h.fetch(t, "/statuses/user_timeline/bob.test.json", alice), &sts)
		if r := h.post(fmt.Sprintf("/statuses/retweet/%d.json", sts[0].ID), nil, alice); r.code != 200 {
			t.Fatalf("retweet: %d %s", r.code, r.body)
		}
	}
	saved := func(t *testing.T, h *harness) {
		h.post("/saved_searches/create.json", url.Values{"query": {"bluesky"}}, alice)
	}
	cases := []goldenCase{
		{ref: "get-account-verify_credentials.json", path: "/account/verify_credentials.json", auth: aliceAuth},
		{ref: "get-account-verify_credentials.xml", path: "/account/verify_credentials.xml", auth: aliceAuth, order: true},
		{ref: "get-statuses-home_timeline.json", path: "/statuses/home_timeline.json", auth: aliceAuth},
		{ref: "get-statuses-home_timeline.xml", path: "/statuses/home_timeline.xml", auth: aliceAuth, order: true},
		{ref: "get-statuses-friends_timeline.json", path: "/statuses/friends_timeline.json", auth: aliceAuth},
		{ref: "get-statuses-friends_timeline.xml", path: "/statuses/friends_timeline.xml", auth: aliceAuth, order: true},
		{ref: "get-direct_messages.json", path: "/direct_messages.json", auth: dmAuth, setup: sendDM},
		{ref: "get-direct_messages.xml", path: "/direct_messages.xml", auth: dmAuth, setup: sendDM, order: true},
		{ref: "get-direct_messages-sent.json", path: "/direct_messages/sent.json", auth: func(*harness) reqOpt { return basic("alice.test", aliceDM) }, setup: sendDM},
		{ref: "get-direct_messages-sent.xml", path: "/direct_messages/sent.xml", auth: func(*harness) reqOpt { return basic("alice.test", aliceDM) }, setup: sendDM, order: true},
		{ref: "get-friends-ids.json", path: "/friends/ids.json?cursor=-1", auth: aliceAuth},
		{ref: "get-friends-ids.xml", path: "/friends/ids.xml?cursor=-1", auth: aliceAuth, order: true},
		{ref: "get-followers-ids.json", path: "/followers/ids.json?cursor=-1", auth: aliceAuth},
		{ref: "get-followers-ids.xml", path: "/followers/ids.xml?cursor=-1", auth: aliceAuth, order: true},
		{ref: "get-account-rate_limit_status.json", path: "/account/rate_limit_status.json", auth: aliceAuth},
		{ref: "get-account-rate_limit_status.xml", path: "/account/rate_limit_status.xml", auth: aliceAuth, order: true},
		{ref: "get-help-test.json", path: "/help/test.json", auth: aliceAuth},
		{ref: "get-help-test.xml", path: "/help/test.xml", auth: aliceAuth},
		{ref: "get-saved_searches.xml", path: "/saved_searches.xml", auth: aliceAuth, setup: saved, order: true},
		{ref: "get-saved_searches-show-id.xml", path: "/saved_searches/show/1.xml", auth: aliceAuth, setup: saved, order: true},
		{ref: "get-blocks-blocking.json", path: "/blocks/blocking.json", auth: aliceAuth, setup: block},
		{ref: "get-blocks-blocking.xml", path: "/blocks/blocking.xml", auth: aliceAuth, setup: block, order: true},
		{ref: "get-blocks-blocking-ids.json", path: "/blocks/blocking/ids.json", auth: aliceAuth, setup: block},
		{ref: "get-blocks-blocking-ids.xml", path: "/blocks/blocking/ids.xml", auth: aliceAuth, setup: block, order: true},
		{ref: "get-statuses-show-id.json", path: "/statuses/show/%d.json", auth: aliceAuth},
		{ref: "get-statuses-show-id.xml", path: "/statuses/show/%d.xml", auth: aliceAuth, order: true},
		{ref: "get-statuses-user_timeline.json", path: "/statuses/user_timeline.json", auth: aliceAuth},
		{ref: "get-statuses-user_timeline.xml", path: "/statuses/user_timeline.xml", auth: aliceAuth, order: true},
		{ref: "get-statuses-mentions.json", path: "/statuses/mentions.json", auth: aliceAuth},
		{ref: "get-statuses-mentions.xml", path: "/statuses/mentions.xml", auth: aliceAuth, order: true},
		{ref: "get-users-show.json", path: "/users/show/bob.test.json", auth: aliceAuth},
		{ref: "get-users-show.xml", path: "/users/show/bob.test.xml", auth: aliceAuth, order: true},
		{ref: "get-users-lookup.json", path: "/users/lookup.json?screen_name=bob.test", auth: aliceAuth},
		{ref: "get-users-lookup.xml", path: "/users/lookup.xml?screen_name=bob.test", auth: aliceAuth, order: true},
		{ref: "get-users-search.json", path: "/users/search.json?q=bob", auth: aliceAuth},
		{ref: "get-friendships-show.json", path: "/friendships/show.json?target_screen_name=bob.test", auth: aliceAuth},
		{ref: "get-friendships-show.xml", path: "/friendships/show.xml?target_screen_name=bob.test", auth: aliceAuth, order: true},
		{ref: "get-favorites.json", path: "/favorites.json", auth: aliceAuth},
		{ref: "get-search.json", path: "/search.json?q=mall", auth: aliceAuth},
		{ref: "wiki-search.json", path: "/search.json?q=mall", auth: aliceAuth},
		{ref: "wiki-friendships-show.json", path: "/friendships/show.json?target_screen_name=bob.test", auth: aliceAuth},
		{ref: "wiki-friendships-show.xml", path: "/friendships/show.xml?target_screen_name=bob.test", auth: aliceAuth},
		{ref: "wiki-statuses-show.xml", path: "/statuses/show/%d.xml", auth: aliceAuth},
		{ref: "get-trends.json", path: "/trends.json", auth: aliceAuth},
		{ref: "get-trends-current.json", path: "/trends/current.json", auth: aliceAuth},
		{ref: "get-trends-daily.json", path: "/trends/daily.json", auth: aliceAuth},
		{ref: "get-trends-weekly.json", path: "/trends/weekly.json", auth: aliceAuth},
		{ref: "get-trends-available.json", path: "/trends/available.json", auth: aliceAuth},
		{ref: "get-trends-available.xml", path: "/trends/available.xml", auth: aliceAuth, order: true},
		{ref: "get-trends-woeid.json", path: "/trends/1.json", auth: aliceAuth},
		{ref: "get-trends-woeid.xml", path: "/trends/1.xml", auth: aliceAuth, order: true},
		{ref: "get-statuses-public_timeline.json", path: "/statuses/public_timeline.json", auth: aliceAuth},
		{ref: "get-statuses-public_timeline.xml", path: "/statuses/public_timeline.xml", auth: aliceAuth, order: true},
		{ref: "get-statuses-retweeted_by_me.json", path: "/statuses/retweeted_by_me.json", auth: aliceAuth, setup: retweet},
		{ref: "get-statuses-retweeted_by_me.xml", path: "/statuses/retweeted_by_me.xml", auth: aliceAuth, setup: retweet, order: true},
		{ref: "get-statuses-retweeted_to_me.xml", path: "/statuses/retweeted_to_me.xml", auth: func(*harness) reqOpt { return basic("bob.test", bobPW) }, setup: retweet, order: true},
		{ref: "get-statuses-retweets_of_me.json", path: "/statuses/retweets_of_me.json", auth: func(*harness) reqOpt { return basic("bob.test", bobPW) }, setup: retweet},
		{ref: "get-statuses-retweets_of_me.xml", path: "/statuses/retweets_of_me.xml", auth: func(*harness) reqOpt { return basic("bob.test", bobPW) }, setup: retweet, order: true},
		{ref: "get-users-search.xml", path: "/users/search.xml?q=bob", auth: aliceAuth, order: true},
		{ref: "post-account-end_session.json", path: "/account/end_session.json", method: "POST", auth: aliceAuth},
		{ref: "post-account-end_session.xml", path: "/account/end_session.xml", method: "POST", auth: aliceAuth, order: true},
		{ref: "post-account-update_profile.json", path: "/account/update_profile.json", method: "POST", auth: aliceAuth},
		{ref: "post-account-update_profile.xml", path: "/account/update_profile.xml", method: "POST", auth: aliceAuth, order: true},
		{ref: "post-account-update_profile_colors.json", path: "/account/update_profile_colors.json", method: "POST", auth: aliceAuth},
		{ref: "post-account-update_delivery_device.json", path: "/account/update_delivery_device.json", method: "POST", auth: aliceAuth},
		{ref: "post-blocks-create.json", path: "/blocks/create/carol.other.example.json", method: "POST", auth: aliceAuth},
		{ref: "post-blocks-create.xml", path: "/blocks/create/carol.other.example.xml", method: "POST", auth: aliceAuth, order: true},
		{ref: "post-blocks-destroy.json", path: "/blocks/destroy/carol.other.example.json", method: "POST", auth: aliceAuth, setup: block},
		{ref: "post-blocks-destroy.xml", path: "/blocks/destroy/carol.other.example.xml", method: "POST", auth: aliceAuth, setup: block, order: true},
		{ref: "post-report_spam.json", path: "/report_spam.json?screen_name=carol.other.example", method: "POST", auth: aliceAuth},
		{ref: "post-report_spam.xml", path: "/report_spam.xml?screen_name=carol.other.example", method: "POST", auth: aliceAuth, order: true},
		{ref: "post-direct_messages-new.json", path: "/direct_messages/new.json", method: "POST", form: url.Values{"user": {"bob.test"}, "text": {"hello there"}}, auth: func(*harness) reqOpt { return basic("alice.test", aliceDM) }},
		{ref: "post-direct_messages-new.xml", path: "/direct_messages/new.xml", method: "POST", form: url.Values{"user": {"bob.test"}, "text": {"hello there"}}, auth: func(*harness) reqOpt { return basic("alice.test", aliceDM) }, order: true},
		{ref: "post-saved_searches-create.xml", path: "/saved_searches/create.xml", method: "POST", form: url.Values{"query": {"bluesky"}}, auth: aliceAuth, order: true},
		{ref: "post-saved_searches-destroy-id.xml", path: "/saved_searches/destroy/1.xml", method: "POST", auth: aliceAuth, setup: saved, order: true},
	}
	ran := 0
	for _, tc := range cases {
		t.Run(tc.ref, func(t *testing.T) {
			ref := loadRef(t, tc.ref)
			h := goldenHarness(t)
			if tc.setup != nil {
				tc.setup(t, h)
			}
			auth := tc.auth(h)
			path := tc.path
			if strings.Contains(path, "%d") {
				// statuses/show: use the newest status's ID.
				var sts []struct{ ID int64 }
				json.Unmarshal(h.fetch(t, "/statuses/home_timeline.json", auth), &sts)
				path = fmt.Sprintf(path, sts[0].ID)
			}
			if strings.Contains(tc.ref, "favorites") {
				var sts []struct{ ID int64 }
				json.Unmarshal(h.fetch(t, "/statuses/home_timeline.json", auth), &sts)
				h.post(fmt.Sprintf("/favorites/create/%d.json", sts[0].ID), nil, auth)
			}
			if strings.Contains(tc.ref, "mentions") {
				h.post("/statuses/update.json", url.Values{"status": {"hey @alice.test"}}, basic("bob.test", bobPW))
			}
			var got []byte
			if tc.method == "POST" {
				r := h.post(path, tc.form, auth)
				if r.code != 200 {
					t.Fatalf("POST %s: %d %s", path, r.code, r.body)
				}
				got = r.body
			} else {
				got = h.fetch(t, path, auth)
			}
			ran++
			if strings.HasSuffix(tc.ref, ".json") {
				var rv, gv any
				if err := json.Unmarshal(ref, &rv); err != nil {
					t.Fatalf("reference JSON: %v", err)
				}
				if err := json.Unmarshal(got, &gv); err != nil {
					t.Fatalf("bridge JSON: %v\n%s", err, got)
				}
				compareJSON(t, "", rv, gv)
			} else {
				compareXML(t, "", parseXML(t, ref), parseXML(t, got), tc.order)
			}
			snapshot(t, strings.TrimPrefix(tc.ref, "get-"), got)
		})
	}
	if ran == 0 {
		t.Fatal("no reference samples found; run scripts/fetch_wayback.py and scripts/extract_refs.py")
	}
}

// TestGoldenLegacyWikiFieldSets checks field presence (not order) against
// the hand-written 2009 apiwiki XML samples, which differ from real output
// in element order and contain typos.
func TestGoldenLegacyWikiFieldSets(t *testing.T) {
	h := goldenHarness(t)
	h.post("/statuses/update.json", url.Values{"status": {"hey @alice.test"}}, basic("bob.test", bobPW))
	got := parseXML(t, h.fetch(t, "/users/show/bob.test.xml", alice))
	have := map[string]bool{}
	for _, n := range uniqueNames(got) {
		have[n] = true
	}
	wiki := loadRef(t, "../archived/rest-users-show.txt")
	var missing []string
	for _, m := range regexp.MustCompile(`<([a-z_]+)>`).FindAllStringSubmatch(string(wiki), -1) {
		n := m[1]
		if n == "user" || n == "status" || have[n] || optional[n] {
			continue
		}
		// Status-level fields appear inside the nested status.
		if st := firstChild(got, "status"); st != nil && firstChild(st, n) != nil {
			continue
		}
		missing = append(missing, n)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("fields from the 2009 users/show sample missing: %v", missing)
	}
}

var _ = fakepds.New
