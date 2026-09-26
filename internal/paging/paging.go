// Package paging implements Twitter's since_id / max_id / count / page
// semantics on top of Bluesky's opaque cursors.
//
// Bridge IDs are assigned in first-seen order, so they cannot be compared to
// decide what is newer. Instead the referenced item's timestamp (and
// identity) is looked up and the upstream feed is walked until that point.
package paging

import (
	"context"
	"time"

	"github.com/j4ckxyz/mockingbird/internal/translate"
)

// Fetch returns one upstream page.
type Fetch func(ctx context.Context, cursor string, limit int) (items []translate.Item, next string, err error)

// Query describes a Twitter-style request.
type Query struct {
	Count int // items wanted
	Page  int // 1-based
	// Since: return only items newer than this (exclusive).
	Since    time.Time
	SinceKey string
	// Max: return only items at or older than this (inclusive).
	Max    time.Time
	MaxKey string
	// StartCursor resumes from a cached upstream cursor.
	StartCursor string
	// MaxCalls bounds upstream requests.
	MaxCalls int
	// MaxLimit is the upstream page size cap (100 for most Bluesky feeds).
	MaxLimit int
}

// Result is the collected page.
type Result struct {
	Items []translate.Item
	// LastCursor is the upstream cursor of the page containing the last
	// returned item; resuming from it and filtering by Max continues exactly.
	LastCursor string
	// Exhausted is set when the upstream feed ended.
	Exhausted bool
	Calls     int
}

// Collect walks the feed.
func Collect(ctx context.Context, fetch Fetch, q Query) (Result, error) {
	var res Result
	if q.Count <= 0 {
		q.Count = 20
	}
	if q.Page < 1 {
		q.Page = 1
	}
	if q.MaxCalls <= 0 {
		q.MaxCalls = 5
	}
	if q.MaxLimit <= 0 {
		q.MaxLimit = 100
	}
	skip := (q.Page - 1) * q.Count
	cursor := q.StartCursor
	// Polling with since_id usually needs only a handful of new items.
	limit := q.Count + skip
	if !q.Since.IsZero() && skip == 0 {
		limit = min(q.Count, 30)
	}
	seen := map[string]bool{}
	for res.Calls < q.MaxCalls {
		items, next, err := fetch(ctx, cursor, clamp(limit, 1, q.MaxLimit))
		res.Calls++
		if err != nil {
			if len(res.Items) > 0 {
				return res, nil // partial page beats an error mid-walk
			}
			return res, err
		}
		done := false
		for _, it := range items {
			key := it.Key()
			if seen[key] {
				continue
			}
			seen[key] = true
			t := it.SortAt()
			if !q.Since.IsZero() && (key == q.SinceKey || !t.After(q.Since)) {
				done = true
				break
			}
			if !q.Max.IsZero() && key != q.MaxKey && t.After(q.Max) {
				continue
			}
			if skip > 0 {
				skip--
				continue
			}
			res.Items = append(res.Items, it)
			res.LastCursor = cursor
			if len(res.Items) >= q.Count {
				done = true
				break
			}
		}
		if done {
			break
		}
		if next == "" || len(items) == 0 {
			res.Exhausted = true
			break
		}
		cursor = next
		// Later pages: fetch big pages, we are skipping or walking.
		limit = q.MaxLimit
	}
	return res, nil
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
