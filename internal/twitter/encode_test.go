package twitter

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

func TestTimeFormats(t *testing.T) {
	ts := time.Date(2008, 8, 27, 13, 8, 45, 0, time.FixedZone("x", 3600))
	if got := Time(ts).String(); got != "Wed Aug 27 12:08:45 +0000 2008" {
		t.Fatalf("REST time %q", got)
	}
	if got := SearchTime(ts).String(); got != "Wed, 27 Aug 2008 12:08:45 +0000" {
		t.Fatalf("search time %q", got)
	}
	d := time.Date(2009, 4, 5, 1, 2, 3, 0, time.UTC)
	if got := Time(d).String(); got != "Sun Apr 05 01:02:03 +0000 2009" {
		t.Fatalf("zero-padded day %q", got)
	}
}

func TestJSONNoHTMLEscaping(t *testing.T) {
	b, err := EncodeJSON(Status{Text: "a &lt; b", Source: `<a href="x">y</a>`, IDStr: "1"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"source":"<a href=\"x\">y</a>"`) || strings.Contains(s, "\\u003c") {
		t.Fatalf("json: %s", s)
	}
	if !strings.Contains(s, `"in_reply_to_status_id":null`) || !strings.Contains(s, `"geo":null`) {
		t.Fatalf("nullable fields must be present as null: %s", s)
	}
}

func TestXMLDropsForbiddenCharacters(t *testing.T) {
	b := EncodeXML("status", Status{Text: "bell\x07 and nul\x00 and ￾ ok 🐦 <b> & \"q\"", IDStr: "1"})
	var v struct {
		Text string `xml:"text"`
	}
	if err := xml.Unmarshal(b, &v); err != nil {
		t.Fatalf("XML must stay parseable: %v\n%s", err, b)
	}
	if v.Text != "bell and nul and  ok 🐦 <b> & \"q\"" {
		t.Fatalf("text %q", v.Text)
	}
	if strings.Contains(string(b), "&quot;") {
		t.Fatalf("quotes need not be escaped in element text: %s", b)
	}
	if strings.Contains(string(b), "id_str") {
		t.Fatal("id_str is JSON-only")
	}
}

func TestXMLArrays(t *testing.T) {
	b := string(EncodeXMLArray("statuses", "status", []Status{{ID: 1}, {ID: 2}}))
	if !strings.HasPrefix(b, `<?xml version="1.0" encoding="UTF-8"?>`+"\n"+`<statuses type="array"><status><created_at>`) {
		t.Fatalf("array: %s", b)
	}
	b = string(EncodeXML("id_list", IDList{IDs: []int64{3, 4}}))
	if !strings.Contains(b, "<id_list><ids><id>3</id><id>4</id></ids><next_cursor>0</next_cursor><previous_cursor>0</previous_cursor></id_list>") {
		t.Fatalf("ids: %s", b)
	}
}

func TestCallbackValidation(t *testing.T) {
	for _, ok := range []string{"cb", "jQuery123_456", "a.b.c", "$x", "twttr.receiveCount", "cbs[0]"} {
		if !ValidCallback(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "alert(1)", "a;b", "a b", "<script>", "a.", "1abc", "a b", strings.Repeat("a", 200)} {
		if ValidCallback(bad) {
			t.Errorf("%q should be rejected", bad)
		}
	}
	if got := string(WrapJSONP("cb", []byte(`{}`))); got != "/**/cb({});" {
		t.Fatalf("wrap %q", got)
	}
}
