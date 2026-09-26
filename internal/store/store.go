// Package store is the bridge's SQLite persistence: numeric ID maps, sessions,
// saved searches and uploads. All SQL is parameterised.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, registers "sqlite"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("store: not found")

// Store wraps separate read and write pools. SQLite allows one writer at a
// time; a single-connection write pool avoids SQLITE_BUSY churn under load.
type Store struct {
	r *sql.DB
	w *sql.DB
}

var migrations = []string{
	// 1: initial schema
	`CREATE TABLE status_ids (
		id      INTEGER PRIMARY KEY AUTOINCREMENT,
		uri     TEXT NOT NULL UNIQUE,
		sort_at INTEGER -- unix milliseconds, NULL until the item is seen
	);
	CREATE TABLE user_ids (
		id     INTEGER PRIMARY KEY AUTOINCREMENT,
		did    TEXT NOT NULL UNIQUE,
		handle TEXT
	);
	CREATE INDEX user_ids_handle ON user_ids(handle);
	CREATE TABLE dm_ids (
		id       INTEGER PRIMARY KEY AUTOINCREMENT,
		convo_id TEXT NOT NULL,
		msg_id   TEXT NOT NULL,
		sort_at  INTEGER,
		UNIQUE (convo_id, msg_id)
	);
	CREATE TABLE sessions (
		key            BLOB PRIMARY KEY, -- HMAC of the credential; never the credential
		kind           TEXT NOT NULL,    -- basic | oauth
		did            TEXT NOT NULL,
		handle         TEXT NOT NULL,
		pds            TEXT NOT NULL,
		scope          TEXT NOT NULL,
		access_enc     BLOB,
		refresh_enc    BLOB NOT NULL,
		token_secret_enc BLOB,          -- OAuth token secret, for signature checks
		consumer_key   TEXT,
		created_at     INTEGER NOT NULL,
		last_used_at   INTEGER NOT NULL
	);
	CREATE INDEX sessions_did ON sessions(did);
	CREATE INDEX sessions_last_used ON sessions(last_used_at);
	CREATE TABLE saved_searches (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		did        TEXT NOT NULL,
		query      TEXT NOT NULL,
		created_at INTEGER NOT NULL
	);
	CREATE INDEX saved_searches_did ON saved_searches(did);
	CREATE TABLE uploads (
		token      TEXT PRIMARY KEY,
		did        TEXT NOT NULL,
		blob       TEXT NOT NULL, -- JSON blob ref from uploadBlob
		mime       TEXT NOT NULL,
		width      INTEGER NOT NULL,
		height     INTEGER NOT NULL,
		created_at INTEGER NOT NULL
	);`,
}

// Open opens (creating if needed) the database under dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "mockingbird.db")
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_txlock=immediate"
	w, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	r, err := sql.Open("sqlite", dsn+"&mode=ro")
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(8)
	s := &Store{r: r, w: w}
	if err := s.migrate(context.Background()); err != nil {
		s.Close()
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	return s, nil
}

// Close closes both pools.
func (s *Store) Close() error {
	return errors.Join(s.r.Close(), s.w.Close())
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.w.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	var v int
	err := s.w.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := s.w.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (0)`); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.w.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE schema_version SET version = ?`, i+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Ping checks the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.r.PingContext(ctx) }

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func ms(t time.Time) int64 { return t.UnixMilli() }
