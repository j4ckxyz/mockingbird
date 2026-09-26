package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStatusIDsOldestFirst(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	base := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	// Timeline order is newest first; IDs must still ascend with time.
	refs := []StatusRef{
		{URI: "at://did:plc:a/app.bsky.feed.post/3", SortAt: base.Add(3 * time.Minute)},
		{URI: "at://did:plc:a/app.bsky.feed.post/2", SortAt: base.Add(2 * time.Minute)},
		{URI: "at://did:plc:a/app.bsky.feed.post/1", SortAt: base.Add(1 * time.Minute)},
	}
	ids, err := s.StatusIDs(ctx, refs)
	if err != nil {
		t.Fatal(err)
	}
	if !(ids[refs[2].URI] < ids[refs[1].URI] && ids[refs[1].URI] < ids[refs[0].URI]) {
		t.Fatalf("IDs not chronological: %v", ids)
	}
	if ids[refs[2].URI] != 1 {
		t.Fatalf("first ID should be 1, got %d", ids[refs[2].URI])
	}
	// Stable on repeat.
	again, err := s.StatusIDs(ctx, refs)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range ids {
		if again[k] != v {
			t.Fatalf("ID for %s changed %d -> %d", k, v, again[k])
		}
	}
	uri, at, err := s.StatusByID(ctx, ids[refs[0].URI])
	if err != nil || uri != refs[0].URI || !at.Equal(refs[0].SortAt) {
		t.Fatalf("reverse lookup: %q %v %v", uri, at, err)
	}
	if _, _, err := s.StatusByID(ctx, 999); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestStatusSortAtBackfill(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	uri := "at://did:plc:a/app.bsky.feed.post/parent"
	ids, _ := s.StatusIDs(ctx, []StatusRef{{URI: uri}})
	when := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	ids2, _ := s.StatusIDs(ctx, []StatusRef{{URI: uri, SortAt: when}})
	if ids[uri] != ids2[uri] {
		t.Fatal("ID changed")
	}
	_, at, _ := s.StatusByID(ctx, ids[uri])
	if !at.Equal(when) {
		t.Fatalf("sort_at not backfilled: %v", at)
	}
}

func TestSeparateIDSpaces(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	st, _ := s.StatusIDs(ctx, []StatusRef{{URI: "at://did:plc:a/app.bsky.feed.post/x"}})
	us, _ := s.UserIDs(ctx, []UserRef{{DID: "did:plc:a", Handle: "a.test"}})
	dm, _ := s.DMIDs(ctx, []DMRef{{ConvoID: "c", MsgID: "m"}})
	if st["at://did:plc:a/app.bsky.feed.post/x"] != 1 || us["did:plc:a"] != 1 || dm[DMRef{ConvoID: "c", MsgID: "m"}] != 1 {
		t.Fatal("each namespace should start at 1")
	}
}

func TestUserHandleUpdate(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	ids, _ := s.UserIDs(ctx, []UserRef{{DID: "did:plc:a", Handle: "old.test"}})
	s.UserIDs(ctx, []UserRef{{DID: "did:plc:a", Handle: "new.test"}})
	s.UserIDs(ctx, []UserRef{{DID: "did:plc:a"}}) // unknown handle must not clear it
	did, h, err := s.UserByID(ctx, ids["did:plc:a"])
	if err != nil || did != "did:plc:a" || h != "new.test" {
		t.Fatalf("got %q %q %v", did, h, err)
	}
}

func TestConcurrentAssignment(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make([]map[string]int64, 8)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			var refs []StatusRef
			for i := 0; i < 50; i++ {
				refs = append(refs, StatusRef{URI: fmt.Sprintf("at://did:plc:a/app.bsky.feed.post/%d", i)})
			}
			m, err := s.StatusIDs(ctx, refs)
			if err != nil {
				t.Error(err)
			}
			results[g] = m
		}(g)
	}
	wg.Wait()
	for g := 1; g < 8; g++ {
		for k, v := range results[0] {
			if results[g][k] != v {
				t.Fatalf("goroutine %d got %d for %s, want %d", g, results[g][k], k, v)
			}
		}
	}
}

func TestSessions(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	row := &SessionRow{Key: []byte("k"), Kind: "basic", DID: "did:plc:a", Handle: "a.test", PDS: "https://pds.test",
		Scope: "com.atproto.appPass", RefreshEnc: []byte("r"), CreatedAt: now, LastUsedAt: now}
	if err := s.PutSession(ctx, row); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSession(ctx, []byte("k"))
	if err != nil || got.DID != "did:plc:a" || string(got.RefreshEnc) != "r" {
		t.Fatalf("got %+v %v", got, err)
	}
	n, _ := s.PurgeSessions(ctx, now.Add(time.Hour))
	if n != 1 {
		t.Fatalf("purged %d", n)
	}
}
