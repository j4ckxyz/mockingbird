package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// SessionRow is a persisted session. Token fields are AES-GCM ciphertexts
// sealed by the caller; the store never sees plaintext credentials.
type SessionRow struct {
	Key            []byte
	Kind           string
	DID            string
	Handle         string
	PDS            string
	Scope          string
	AccessEnc      []byte
	RefreshEnc     []byte
	TokenSecretEnc []byte
	ConsumerKey    string
	CreatedAt      time.Time
	LastUsedAt     time.Time
}

// PutSession inserts or replaces a session.
func (s *Store) PutSession(ctx context.Context, r *SessionRow) error {
	_, err := s.w.ExecContext(ctx, `INSERT INTO sessions
		(key, kind, did, handle, pds, scope, access_enc, refresh_enc, token_secret_enc, consumer_key, created_at, last_used_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			did = excluded.did, handle = excluded.handle, pds = excluded.pds, scope = excluded.scope,
			access_enc = excluded.access_enc, refresh_enc = excluded.refresh_enc,
			token_secret_enc = COALESCE(excluded.token_secret_enc, sessions.token_secret_enc),
			consumer_key = COALESCE(excluded.consumer_key, sessions.consumer_key),
			last_used_at = excluded.last_used_at`,
		r.Key, r.Kind, r.DID, r.Handle, r.PDS, r.Scope, r.AccessEnc, r.RefreshEnc, r.TokenSecretEnc, r.ConsumerKey,
		ms(r.CreatedAt), ms(r.LastUsedAt))
	return err
}

// GetSession loads a session by key.
func (s *Store) GetSession(ctx context.Context, key []byte) (*SessionRow, error) {
	r := &SessionRow{Key: key}
	var created, used int64
	var consumer sql.NullString
	err := s.r.QueryRowContext(ctx, `SELECT kind, did, handle, pds, scope, access_enc, refresh_enc, token_secret_enc, consumer_key, created_at, last_used_at
		FROM sessions WHERE key = ?`, key).Scan(&r.Kind, &r.DID, &r.Handle, &r.PDS, &r.Scope, &r.AccessEnc, &r.RefreshEnc, &r.TokenSecretEnc, &consumer, &created, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.ConsumerKey = consumer.String
	r.CreatedAt, r.LastUsedAt = time.UnixMilli(created), time.UnixMilli(used)
	return r, nil
}

// UpdateSessionTokens stores refreshed tokens.
func (s *Store) UpdateSessionTokens(ctx context.Context, key []byte, handle, pds string, accessEnc, refreshEnc []byte, now time.Time) error {
	_, err := s.w.ExecContext(ctx, `UPDATE sessions SET handle = ?, pds = ?, access_enc = ?, refresh_enc = ?, last_used_at = ? WHERE key = ?`,
		handle, pds, accessEnc, refreshEnc, ms(now), key)
	return err
}

// TouchSession records use, at most as often as the caller chooses to call it.
func (s *Store) TouchSession(ctx context.Context, key []byte, now time.Time) error {
	_, err := s.w.ExecContext(ctx, `UPDATE sessions SET last_used_at = ? WHERE key = ?`, ms(now), key)
	return err
}

// DeleteSession removes a session.
func (s *Store) DeleteSession(ctx context.Context, key []byte) error {
	_, err := s.w.ExecContext(ctx, `DELETE FROM sessions WHERE key = ?`, key)
	return err
}

// PurgeSessions deletes sessions idle since before cutoff.
func (s *Store) PurgeSessions(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.w.ExecContext(ctx, `DELETE FROM sessions WHERE last_used_at < ?`, ms(cutoff))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CountSessions returns the number of stored sessions.
func (s *Store) CountSessions(ctx context.Context) (int64, error) {
	var n int64
	err := s.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&n)
	return n, err
}

// SavedSearch is a locally stored saved search.
type SavedSearch struct {
	ID        int64
	Query     string
	CreatedAt time.Time
}

// SavedSearches lists a user's saved searches.
func (s *Store) SavedSearches(ctx context.Context, did string) ([]SavedSearch, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT id, query, created_at FROM saved_searches WHERE did = ? ORDER BY id`, did)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SavedSearch
	for rows.Next() {
		var ss SavedSearch
		var c int64
		if err := rows.Scan(&ss.ID, &ss.Query, &c); err != nil {
			return nil, err
		}
		ss.CreatedAt = time.UnixMilli(c).UTC()
		out = append(out, ss)
	}
	return out, rows.Err()
}

// SavedSearch returns one saved search owned by did.
func (s *Store) SavedSearch(ctx context.Context, did string, id int64) (SavedSearch, error) {
	var ss SavedSearch
	var c int64
	err := s.r.QueryRowContext(ctx, `SELECT id, query, created_at FROM saved_searches WHERE did = ? AND id = ?`, did, id).Scan(&ss.ID, &ss.Query, &c)
	if errors.Is(err, sql.ErrNoRows) {
		return ss, ErrNotFound
	}
	ss.CreatedAt = time.UnixMilli(c).UTC()
	return ss, err
}

// CreateSavedSearch stores a query, capping each user at 25 like Twitter did.
func (s *Store) CreateSavedSearch(ctx context.Context, did, query string, now time.Time) (SavedSearch, error) {
	var n int
	if err := s.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM saved_searches WHERE did = ?`, did).Scan(&n); err != nil {
		return SavedSearch{}, err
	}
	if n >= 25 {
		return SavedSearch{}, errors.New("store: saved search limit reached")
	}
	var id int64
	err := s.w.QueryRowContext(ctx, `INSERT INTO saved_searches (did, query, created_at) VALUES (?, ?, ?) RETURNING id`, did, query, ms(now)).Scan(&id)
	return SavedSearch{ID: id, Query: query, CreatedAt: now.UTC()}, err
}

// DeleteSavedSearch removes a saved search owned by did.
func (s *Store) DeleteSavedSearch(ctx context.Context, did string, id int64) (SavedSearch, error) {
	ss, err := s.SavedSearch(ctx, did, id)
	if err != nil {
		return ss, err
	}
	_, err = s.w.ExecContext(ctx, `DELETE FROM saved_searches WHERE did = ? AND id = ?`, did, id)
	return ss, err
}

// Upload is an image uploaded through the TwitPic-compatible endpoint.
type Upload struct {
	Token     string
	DID       string
	BlobJSON  string
	MIME      string
	Width     int
	Height    int
	CreatedAt time.Time
}

// PutUpload stores an upload.
func (s *Store) PutUpload(ctx context.Context, u *Upload) error {
	_, err := s.w.ExecContext(ctx, `INSERT INTO uploads (token, did, blob, mime, width, height, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		u.Token, u.DID, u.BlobJSON, u.MIME, u.Width, u.Height, ms(u.CreatedAt))
	return err
}

// GetUpload loads an upload by token.
func (s *Store) GetUpload(ctx context.Context, token string) (*Upload, error) {
	u := &Upload{Token: token}
	var c int64
	err := s.r.QueryRowContext(ctx, `SELECT did, blob, mime, width, height, created_at FROM uploads WHERE token = ?`, token).
		Scan(&u.DID, &u.BlobJSON, &u.MIME, &u.Width, &u.Height, &c)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	u.CreatedAt = time.UnixMilli(c).UTC()
	return u, err
}

// PurgeUploads deletes uploads older than cutoff. Unused blobs are garbage
// collected by the PDS on its own.
func (s *Store) PurgeUploads(ctx context.Context, cutoff time.Time) error {
	_, err := s.w.ExecContext(ctx, `DELETE FROM uploads WHERE created_at < ?`, ms(cutoff))
	return err
}
