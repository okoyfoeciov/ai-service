// Package store is ai-service's cache: one on-disk SQLite file of key/value
// rows, each with an optional expiry. internal/mw keeps Merriam-Webster lookups
// under "mw:<word>" for 30 days; main keeps the tray's /api/transcribe provider
// choice under "transcribe:provider" with no expiry. Pure-Go driver
// (modernc.org/sqlite), so Linux's binary is static (CGO disabled).
package store

import (
	"database/sql"
	"log"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS cache (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  expires_at INTEGER          -- unix seconds; NULL = never expires
);
CREATE INDEX IF NOT EXISTS idx_cache_expires ON cache(expires_at);
`

type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite cache at path. A single connection
// is used so writes serialise cleanly.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, err
		}
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db}
	go s.sweeper()
	return s, nil
}

// Get returns the value for key. A missing key, an expired row, or any error is
// reported as a miss (best-effort).
func (s *Store) Get(key string) (string, bool) {
	var value string
	var expires sql.NullInt64
	err := s.db.QueryRow(`SELECT value, expires_at FROM cache WHERE key = ?`, key).
		Scan(&value, &expires)
	if err != nil {
		return "", false
	}
	if expires.Valid && expires.Int64 <= time.Now().Unix() {
		return "", false
	}
	return value, true
}

// Set writes key=value with an optional TTL in seconds (<=0 means no expiry).
// Errors are swallowed: a failed cache write must never break serving a fresh
// result.
func (s *Store) Set(key, value string, ttlSeconds int64) {
	var expires interface{}
	if ttlSeconds > 0 {
		expires = time.Now().Unix() + ttlSeconds
	}
	_, err := s.db.Exec(
		`INSERT INTO cache(key, value, expires_at) VALUES(?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, expires_at = excluded.expires_at`,
		key, value, expires,
	)
	if err != nil {
		log.Printf("store: set %q failed: %v", key, err)
	}
}

func (s *Store) Close() error { return s.db.Close() }

// sweeper periodically deletes expired rows so the file doesn't grow forever.
func (s *Store) sweeper() {
	tick := time.NewTicker(6 * time.Hour)
	defer tick.Stop()
	for range tick.C {
		if _, err := s.db.Exec(
			`DELETE FROM cache WHERE expires_at IS NOT NULL AND expires_at <= ?`,
			time.Now().Unix(),
		); err != nil {
			log.Printf("store: sweep failed: %v", err)
		}
	}
}
