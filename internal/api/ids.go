package api

import (
	"context"

	"github.com/jackgilbert/mockingbird/internal/cache"
	"github.com/jackgilbert/mockingbird/internal/store"
)

// idCache fronts the store's ID tables with in-memory LRUs; nearly every
// response maps dozens of URIs and DIDs, and most were seen moments ago.
type idCache struct {
	st      *store.Store
	status  *cache.LRU[string, statusEntry]
	statusR *cache.LRU[int64, string]
	users   *cache.LRU[string, int64]
	handles *cache.LRU[string, string] // did -> handle last written
}

type statusEntry struct {
	id    int64
	timed bool // the store has this status's sort time
}

func newIDCache(st *store.Store) *idCache {
	return &idCache{
		st:      st,
		status:  cache.New[string, statusEntry](200_000),
		statusR: cache.New[int64, string](100_000),
		users:   cache.New[string, int64](100_000),
		handles: cache.New[string, string](100_000),
	}
}

func (c *idCache) StatusIDs(ctx context.Context, refs []store.StatusRef) (map[string]int64, error) {
	out := make(map[string]int64, len(refs))
	var miss []store.StatusRef
	for _, r := range refs {
		e, ok := c.status.Get(r.URI)
		if ok && (e.timed || r.SortAt.IsZero()) {
			out[r.URI] = e.id
			continue
		}
		// Unknown, or known without a time that we can now supply.
		miss = append(miss, r)
	}
	if len(miss) == 0 {
		return out, nil
	}
	m, err := c.st.StatusIDs(ctx, miss)
	if err != nil {
		return nil, err
	}
	for _, r := range miss {
		id := m[r.URI]
		out[r.URI] = id
		c.status.Add(r.URI, statusEntry{id: id, timed: !r.SortAt.IsZero()})
		c.statusR.Add(id, r.URI)
	}
	return out, nil
}

func (c *idCache) UserIDs(ctx context.Context, refs []store.UserRef) (map[string]int64, error) {
	out := make(map[string]int64, len(refs))
	var miss []store.UserRef
	for _, r := range refs {
		id, ok := c.users.Get(r.DID)
		if ok {
			out[r.DID] = id
			if r.Handle == "" {
				continue
			}
			if h, ok := c.handles.Get(r.DID); ok && h == r.Handle {
				continue
			}
		}
		miss = append(miss, r)
	}
	if len(miss) == 0 {
		return out, nil
	}
	m, err := c.st.UserIDs(ctx, miss)
	if err != nil {
		return nil, err
	}
	for _, r := range miss {
		out[r.DID] = m[r.DID]
		c.users.Add(r.DID, m[r.DID])
		if r.Handle != "" {
			c.handles.Add(r.DID, r.Handle)
		}
	}
	return out, nil
}

// StatusURI resolves a status ID.
func (c *idCache) StatusURI(ctx context.Context, id int64) (string, error) {
	if uri, ok := c.statusR.Get(id); ok {
		return uri, nil
	}
	uri, _, err := c.st.StatusByID(ctx, id)
	if err == nil {
		c.statusR.Add(id, uri)
	}
	return uri, err
}
