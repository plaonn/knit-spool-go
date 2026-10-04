package spool

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"
)

type scopeState struct {
	ID         []byte
	MaxFrames  int
	TTLMS      int64
	MaxBlob    int
	Commons    bool
	CreatedAt  int64
	LastActive int64
}

type storedFrame struct {
	ID        []byte
	Data      []byte
	ArrivedAt int64
}

type Store struct {
	db     *sql.DB
	mu     sync.Mutex
	closed bool
}

// OpenStore opens a store with SQLite synchronous=FULL.
func OpenStore(path string) (*Store, error) {
	return openStore(path, "FULL")
}

func openStore(path, synchronous string) (*Store, error) {
	mode, err := sqliteSynchronousMode(synchronous)
	if err != nil {
		return nil, err
	}
	// Validate before creating files, changing permissions, or opening SQLite.
	if path != ":memory:" {
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create data directory: %w", err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, fmt.Errorf("protect data directory: %w", err)
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, fmt.Errorf("open database file: %w", err)
		}
		if err := f.Chmod(0o600); err != nil {
			f.Close()
			return nil, fmt.Errorf("protect database file: %w", err)
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	s := &Store{db: db}
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
		db.Close()
		return nil, err
	}
	if path != ":memory:" {
		if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
			db.Close()
			return nil, err
		}
	}
	// Use only fixed statements, never configuration text as SQL.
	synchronousPragma := "PRAGMA synchronous=FULL"
	if mode == "NORMAL" {
		synchronousPragma = "PRAGMA synchronous=NORMAL"
	}
	if _, err := db.ExecContext(ctx, synchronousPragma); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.createSchema(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if path != ":memory:" {
		if err := os.Chmod(path, 0o600); err != nil {
			db.Close()
			return nil, fmt.Errorf("protect database file: %w", err)
		}
	}
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		db.Close()
		if err != nil {
			return nil, fmt.Errorf("SQLite integrity check: %w", err)
		}
		return nil, errors.New("SQLite integrity check failed")
	}
	return s, nil
}

func (s *Store) createSchema(ctx context.Context) error {
	statements := []string{
		"CREATE TABLE IF NOT EXISTS scopes (scope BLOB PRIMARY KEY, max_frames INTEGER NOT NULL, ttl_ms INTEGER NOT NULL, max_blob INTEGER NOT NULL, is_commons INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL, last_active INTEGER NOT NULL)",
		"CREATE TABLE IF NOT EXISTS frames (scope BLOB NOT NULL, blob_id BLOB NOT NULL, data BLOB NOT NULL, arrived_at INTEGER NOT NULL, PRIMARY KEY(scope,blob_id), FOREIGN KEY(scope) REFERENCES scopes(scope) ON DELETE CASCADE)",
		"CREATE INDEX IF NOT EXISTS frames_arrival ON frames(scope,arrived_at,blob_id)",
		"CREATE TABLE IF NOT EXISTS tombstones (scope BLOB NOT NULL, blob_id BLOB NOT NULL, expires_at INTEGER NOT NULL, sequence INTEGER NOT NULL, PRIMARY KEY(scope,blob_id), FOREIGN KEY(scope) REFERENCES scopes(scope) ON DELETE CASCADE)",
		"CREATE INDEX IF NOT EXISTS tombstones_sequence ON tombstones(scope,sequence)",
		"CREATE TABLE IF NOT EXISTS attachments (scope BLOB NOT NULL, aid BLOB NOT NULL, total INTEGER NOT NULL, arrived_at INTEGER NOT NULL, PRIMARY KEY(scope,aid), FOREIGN KEY(scope) REFERENCES scopes(scope) ON DELETE CASCADE)",
		"CREATE TABLE IF NOT EXISTS chunks (scope BLOB NOT NULL, aid BLOB NOT NULL, idx INTEGER NOT NULL, cid BLOB NOT NULL, data BLOB NOT NULL, charged_bytes INTEGER NOT NULL, PRIMARY KEY(scope,aid,idx), FOREIGN KEY(scope,aid) REFERENCES attachments(scope,aid) ON DELETE CASCADE)",
		"CREATE INDEX IF NOT EXISTS chunks_aid ON chunks(scope,aid,idx)",
		"CREATE TABLE IF NOT EXISTS attachment_tombstones (scope BLOB NOT NULL, aid BLOB NOT NULL, expires_at INTEGER NOT NULL, sequence INTEGER NOT NULL, PRIMARY KEY(scope,aid), FOREIGN KEY(scope) REFERENCES scopes(scope) ON DELETE CASCADE)",
		"CREATE INDEX IF NOT EXISTS attachment_tombstones_sequence ON attachment_tombstones(scope,sequence)",
		"CREATE TABLE IF NOT EXISTS pow_cache (scope BLOB NOT NULL, day INTEGER NOT NULL, accepted_at INTEGER NOT NULL, PRIMARY KEY(scope,day))",
		"CREATE TABLE IF NOT EXISTS sequence (id INTEGER PRIMARY KEY CHECK(id=1), next_value INTEGER NOT NULL)",
		"INSERT OR IGNORE INTO sequence(id,next_value) VALUES(1,1)",
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize SQLite schema: %w", err)
		}
	}
	return nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.db == nil {
		return nil
	}
	if _, err := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil && err != sql.ErrConnDone {
		_ = s.db.Close()
		return err
	}
	return s.db.Close()
}
