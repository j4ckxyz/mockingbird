package paging

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/jackgilbert/mockingbird/internal/atp"
	"github.com/jackgilbert/mockingbird/internal/translate"
)

var base = time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)

// feed of n posts, newest first, one minute apart; post i is at base+i min.
func makeFeed(n int) []translate.Item {
	var items []translate.Item
	for i := n - 1; i >= 0; i-- {
		at := base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		items = append(items, translate.Item{Post: &atp.PostView{URI: fmt.Sprintf("at://did:plc:a/app.bsky.feed.post/%03d", i), IndexedAt: at}})
	}
	return items
}

func fetcher(items []translate.Item, calls *int) Fetch {
	return func(ctx context.Context, cursor string, limit int) ([]translate.Item, string, error) {
		*calls++
		off, _ := strconv.Atoi(cursor)
		if off >= len(items) {
			return nil, "", nil
		}
		end := min(off+limit, len(items))
		next := ""
		if end < len(items) {
			next = strconv.Itoa(end)
		}
		return items[off:end], next, nil
	}
}

func keys(r Result) []int {
	var out []int
	for _, it := range r.Items {
		var n int
		fmt.Sscanf(translate.RkeyFromURI(it.Key()), "%d", &n)
		out = append(out, n)
	}
	return out
}

func eq(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestFirstPage(t *testing.T) {
	calls := 0
	r, err := Collect(context.Background(), fetcher(makeFeed(100), &calls), Query{Count: 5})
	if err != nil || !eq(keys(r), []int{99, 98, 97, 96, 95}) || calls != 1 {
		t.Fatalf("got %v calls=%d err=%v", keys(r), calls, err)
	}
}

func TestSinceStopsAtReferencedItem(t *testing.T) {
	calls := 0
	feed := makeFeed(300)
	since := feed[3].Post // item 296
	r, _ := Collect(context.Background(), fetcher(feed, &calls), Query{Count: 20, Since: since.SortAt(), SinceKey: since.URI})
	if !eq(keys(r), []int{299, 298, 297}) {
		t.Fatalf("got %v", keys(r))
	}
	if calls != 1 {
		t.Fatalf("polling for 3 new items should take one call, took %d", calls)
	}
}

func TestSinceNothingNew(t *testing.T) {
	calls := 0
	feed := makeFeed(50)
	since := feed[0].Post
	r, _ := Collect(context.Background(), fetcher(feed, &calls), Query{Count: 20, Since: since.SortAt(), SinceKey: since.URI})
	if len(r.Items) != 0 {
		t.Fatalf("got %v", keys(r))
	}
}

func TestMaxIsInclusiveAndWalksCursors(t *testing.T) {
	calls := 0
	feed := makeFeed(500)
	max := feed[250].Post // item 249
	r, _ := Collect(context.Background(), fetcher(feed, &calls), Query{Count: 3, Max: max.SortAt(), MaxKey: max.URI, MaxLimit: 100, MaxCalls: 5})
	if !eq(keys(r), []int{249, 248, 247}) {
		t.Fatalf("got %v", keys(r))
	}
	if calls < 2 {
		t.Fatalf("expected to walk several upstream pages, calls=%d", calls)
	}
}

func TestResumeFromCursor(t *testing.T) {
	calls := 0
	feed := makeFeed(500)
	r1, _ := Collect(context.Background(), fetcher(feed, &calls), Query{Count: 20})
	last := r1.Items[len(r1.Items)-1].Post // item 480
	calls = 0
	r2, _ := Collect(context.Background(), fetcher(feed, &calls), Query{Count: 20, Max: last.SortAt(), MaxKey: last.URI, StartCursor: r1.LastCursor})
	if keys(r2)[0] != 480 || keys(r2)[19] != 461 || calls > 2 {
		t.Fatalf("got %v calls=%d", keys(r2), calls)
	}
}

func TestPageParameter(t *testing.T) {
	calls := 0
	r, _ := Collect(context.Background(), fetcher(makeFeed(100), &calls), Query{Count: 10, Page: 3})
	if !eq(keys(r), []int{79, 78, 77, 76, 75, 74, 73, 72, 71, 70}) {
		t.Fatalf("got %v", keys(r))
	}
}

func TestCallCap(t *testing.T) {
	calls := 0
	feed := makeFeed(10000)
	max := feed[9000].Post
	r, _ := Collect(context.Background(), fetcher(feed, &calls), Query{Count: 20, Max: max.SortAt(), MaxKey: max.URI, MaxCalls: 3})
	if calls != 3 || len(r.Items) != 0 {
		t.Fatalf("calls=%d items=%d", calls, len(r.Items))
	}
}

func TestRepostOrderUsesRepostTime(t *testing.T) {
	old := &atp.PostView{URI: "at://did:plc:a/app.bsky.feed.post/old", IndexedAt: base.Format(time.RFC3339)}
	it := translate.Item{Post: old, Repost: &atp.ReasonRepost{URI: "at://did:plc:b/app.bsky.feed.repost/r", IndexedAt: base.Add(time.Hour).Format(time.RFC3339)}}
	if !it.SortAt().Equal(base.Add(time.Hour)) || it.Key() != "at://did:plc:b/app.bsky.feed.repost/r" {
		t.Fatalf("repost sort/key wrong: %v %v", it.SortAt(), it.Key())
	}
}
