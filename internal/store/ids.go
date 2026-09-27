package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"
)

// Twitter clients need small numeric IDs. Each namespace (statuses, users,
// DMs) is a separate AUTOINCREMENT table, so IDs start at 1, never repeat and
// stay far below 2^31 for years. That matters: Twitterrific for iPhone parsed
// IDs as signed 32-bit integers and broke at the 2009 "Twitpocalypse".

// StatusRef is a status to be assigned an ID. SortAt may be zero if unknown.
type StatusRef struct {
	URI    string
	SortAt time.Time
}

// MaxSafeID is the largest ID handed out before we log loudly; 32-bit clients
// break above it.
const MaxSafeID = 1<<31 - 1

// StatusIDs assigns IDs for refs, oldest first so that a batch seen together
// sorts correctly by ID, and returns uri -> id for all of them.
func (s *Store) StatusIDs(ctx context.Context, refs []StatusRef) (map[string]int64, error) {
	out := make(map[string]int64, len(refs))
	if len(refs) == 0 {
		return out, nil
	}
	// De-duplicate first (keeping any known time): an upsert that hits a
	// conflict still consumes an AUTOINCREMENT value, leaving gaps.
	byURI := make(map[string]int, len(refs))
	sorted := make([]StatusRef, 0, len(refs))
	for _, r := range refs {
		if i, ok := byURI[r.URI]; ok {
			if sorted[i].SortAt.IsZero() {
				sorted[i].SortAt = r.SortAt
			}
			continue
		}
		byURI[r.URI] = len(sorted)
		sorted = append(sorted, r)
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i].SortAt, sorted[j].SortAt
		if a.IsZero() != b.IsZero() {
			return a.IsZero() // unknown times first: they are usually older parents
		}
		return a.Before(b)
	})
	// Fast path: everything already mapped and timed.
	if err := s.lookupStatuses(ctx, sorted, out); err != nil {
		return nil, err
	}
	var missing []StatusRef
	for _, r := range sorted {
		if _, ok := out[r.URI]; !ok {
			missing = append(missing, r)
		}
	}
	if len(missing) == 0 {
		return out, nil
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO status_ids (uri, sort_at) VALUES (?, ?)
		ON CONFLICT(uri) DO UPDATE SET sort_at = COALESCE(status_ids.sort_at, excluded.sort_at)
		RETURNING id`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	for _, r := range missing {
		var id int64
		if err := stmt.QueryRowContext(ctx, r.URI, nullMS(r.SortAt)).Scan(&id); err != nil {
			return nil, err
		}
		out[r.URI] = id
	}
	return out, tx.Commit()
}

// lookupStatuses fills out with existing IDs, and fixes up rows whose sort_at
// was unknown when first assigned.
func (s *Store) lookupStatuses(ctx context.Context, refs []StatusRef, out map[string]int64) error {
	const chunk = 400
	var fix []StatusRef
	for i := 0; i < len(refs); i += chunk {
		part := refs[i:min(i+chunk, len(refs))]
		args := make([]any, len(part))
		want := make(map[string]time.Time, len(part))
		for j, r := range part {
			args[j] = r.URI
			want[r.URI] = r.SortAt
		}
		rows, err := s.r.QueryContext(ctx, `SELECT id, uri, sort_at FROM status_ids WHERE uri IN (`+placeholders(len(part))+`)`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			var uri string
			var sortAt sql.NullInt64
			if err := rows.Scan(&id, &uri, &sortAt); err != nil {
				rows.Close()
				return err
			}
			out[uri] = id
			if !sortAt.Valid && !want[uri].IsZero() {
				fix = append(fix, StatusRef{URI: uri, SortAt: want[uri]})
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	for _, f := range fix {
		if _, err := s.w.ExecContext(ctx, `UPDATE status_ids SET sort_at = ? WHERE uri = ? AND sort_at IS NULL`, ms(f.SortAt), f.URI); err != nil {
			return err
		}
	}
	return nil
}

// StatusByID returns the URI and (possibly zero) sort time for a status ID.
func (s *Store) StatusByID(ctx context.Context, id int64) (string, time.Time, error) {
	var uri string
	var sortAt sql.NullInt64
	err := s.r.QueryRowContext(ctx, `SELECT uri, sort_at FROM status_ids WHERE id = ?`, id).Scan(&uri, &sortAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, ErrNotFound
	}
	if err != nil {
		return "", time.Time{}, err
	}
	var t time.Time
	if sortAt.Valid {
		t = time.UnixMilli(sortAt.Int64).UTC()
	}
	return uri, t, nil
}

// UserRef is a user to be assigned an ID.
type UserRef struct {
	DID    string
	Handle string
}

// UserIDs assigns IDs for users and records their latest handle.
func (s *Store) UserIDs(ctx context.Context, refs []UserRef) (map[string]int64, error) {
	out := make(map[string]int64, len(refs))
	if len(refs) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(refs))
	seen := map[string]string{}
	for _, r := range refs {
		if _, ok := seen[r.DID]; !ok {
			args = append(args, r.DID)
		}
		if r.Handle != "" || seen[r.DID] == "" {
			seen[r.DID] = r.Handle
		}
	}
	type row struct {
		id     int64
		handle string
	}
	have := map[string]row{}
	for i := 0; i < len(args); i += 400 {
		part := args[i:min(i+400, len(args))]
		rows, err := s.r.QueryContext(ctx, `SELECT id, did, COALESCE(handle, '') FROM user_ids WHERE did IN (`+placeholders(len(part))+`)`, part...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var did, h string
			if err := rows.Scan(&id, &did, &h); err != nil {
				rows.Close()
				return nil, err
			}
			have[did] = row{id, h}
		}
		rows.Close()
	}
	var todo []UserRef
	for did, h := range seen {
		r, ok := have[did]
		if ok {
			out[did] = r.id
		}
		if !ok || (h != "" && h != r.handle) {
			todo = append(todo, UserRef{DID: did, Handle: h})
		}
	}
	if len(todo) == 0 {
		return out, nil
	}
	sort.Slice(todo, func(i, j int) bool { return todo[i].DID < todo[j].DID })
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO user_ids (did, handle) VALUES (?, NULLIF(?, ''))
		ON CONFLICT(did) DO UPDATE SET handle = COALESCE(NULLIF(excluded.handle, ''), user_ids.handle)
		RETURNING id`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	for _, r := range todo {
		var id int64
		if err := stmt.QueryRowContext(ctx, r.DID, r.Handle).Scan(&id); err != nil {
			return nil, err
		}
		out[r.DID] = id
	}
	return out, tx.Commit()
}

// UserByID returns the DID and last known handle for a user ID.
func (s *Store) UserByID(ctx context.Context, id int64) (did, handle string, err error) {
	err = s.r.QueryRowContext(ctx, `SELECT did, COALESCE(handle, '') FROM user_ids WHERE id = ?`, id).Scan(&did, &handle)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return did, handle, err
}

// DMRef identifies a chat message.
type DMRef struct {
	ConvoID string
	MsgID   string
	SortAt  time.Time
}

// DMIDs assigns IDs for chat messages, oldest first.
func (s *Store) DMIDs(ctx context.Context, refs []DMRef) (map[DMRef]int64, error) {
	out := make(map[DMRef]int64, len(refs))
	if len(refs) == 0 {
		return out, nil
	}
	seen := map[[2]string]bool{}
	var sorted []DMRef
	for _, r := range refs {
		k := [2]string{r.ConvoID, r.MsgID}
		if !seen[k] {
			seen[k] = true
			sorted = append(sorted, r)
		}
	}
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].SortAt.Before(sorted[j].SortAt) })
	// Look up known messages first. Every listing repeats most messages, and
	// an upsert that hits a conflict still consumes an AUTOINCREMENT value,
	// which would push new DM IDs past 2^31 within months.
	if err := s.lookupDMs(ctx, sorted, out); err != nil {
		return nil, err
	}
	missing := sorted[:0:0]
	for _, r := range sorted {
		if _, ok := out[DMRef{ConvoID: r.ConvoID, MsgID: r.MsgID}]; !ok {
			missing = append(missing, r)
		}
	}
	if len(missing) == 0 {
		return out, nil
	}
	sorted = missing
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO dm_ids (convo_id, msg_id, sort_at) VALUES (?, ?, ?)
		ON CONFLICT(convo_id, msg_id) DO UPDATE SET sort_at = COALESCE(dm_ids.sort_at, excluded.sort_at)
		RETURNING id`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	for _, r := range sorted {
		var id int64
		if err := stmt.QueryRowContext(ctx, r.ConvoID, r.MsgID, nullMS(r.SortAt)).Scan(&id); err != nil {
			return nil, err
		}
		key := r
		key.SortAt = time.Time{}
		out[key] = id
	}
	return out, tx.Commit()
}

// lookupDMs fills out with existing DM IDs.
func (s *Store) lookupDMs(ctx context.Context, refs []DMRef, out map[DMRef]int64) error {
	const chunk = 400
	for i := 0; i < len(refs); i += chunk {
		part := refs[i:min(i+chunk, len(refs))]
		args := make([]any, 0, 2*len(part))
		timed := make(map[DMRef]bool, len(part))
		for _, r := range part {
			args = append(args, r.ConvoID, r.MsgID)
			timed[DMRef{ConvoID: r.ConvoID, MsgID: r.MsgID}] = !r.SortAt.IsZero()
		}
		values := strings.TrimSuffix(strings.Repeat("(?,?),", len(part)), ",")
		rows, err := s.r.QueryContext(ctx, `SELECT id, convo_id, msg_id, sort_at IS NOT NULL FROM dm_ids WHERE (convo_id, msg_id) IN (VALUES `+values+`)`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			var k DMRef
			var hasTime bool
			if err := rows.Scan(&id, &k.ConvoID, &k.MsgID, &hasTime); err != nil {
				rows.Close()
				return err
			}
			if !hasTime && timed[k] {
				continue // let the upsert backfill the time
			}
			out[k] = id
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

// DMByID returns the conversation and message IDs for a DM ID.
func (s *Store) DMByID(ctx context.Context, id int64) (DMRef, error) {
	var r DMRef
	var sortAt sql.NullInt64
	err := s.r.QueryRowContext(ctx, `SELECT convo_id, msg_id, sort_at FROM dm_ids WHERE id = ?`, id).Scan(&r.ConvoID, &r.MsgID, &sortAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if sortAt.Valid {
		r.SortAt = time.UnixMilli(sortAt.Int64).UTC()
	}
	return r, err
}

func nullMS(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return ms(t)
}
