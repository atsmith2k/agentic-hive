package main

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// OpenDB opens the SQLite database and brings its schema up to date.
// The pragmas are part of the DSN so that every pooled connection gets them.
func OpenDB(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "." && path != ":memory:" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create data directory: %w", err)
		}
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if path == ":memory:" {
		db.SetMaxOpenConns(1) // each in-memory connection is its own database
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if path != ":memory:" {
		os.Chmod(path, 0o600)
	}
	return db, nil
}

// migrations run in order; PRAGMA user_version records how many have run.
// Never edit one that has shipped: add a new one.
var migrations = []string{
	// 1: the schema as it stood before versioning. IF NOT EXISTS lets this
	// adopt a database created by the earlier, unversioned releases.
	`
	CREATE TABLE IF NOT EXISTS agents (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		owner TEXT NOT NULL,
		api_key_hash TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		last_seen_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE IF NOT EXISTS threads (
		id TEXT PRIMARY KEY,
		agent_id TEXT NOT NULL REFERENCES agents(id),
		title TEXT NOT NULL,
		body TEXT NOT NULL,
		tags TEXT DEFAULT '[]',
		pinned INTEGER DEFAULT 0,
		archived INTEGER DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE IF NOT EXISTS replies (
		id TEXT PRIMARY KEY,
		thread_id TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
		agent_id TEXT NOT NULL REFERENCES agents(id),
		body TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE IF NOT EXISTS status_tags (
		id TEXT PRIMARY KEY,
		thread_id TEXT REFERENCES threads(id) ON DELETE CASCADE,
		reply_id TEXT REFERENCES replies(id) ON DELETE CASCADE,
		agent_id TEXT NOT NULL REFERENCES agents(id),
		tag TEXT NOT NULL CHECK(tag IN ('acknowledged','depends-on','blocked','resolved','in-progress','needs-review')),
		reference_id TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		CHECK(
			(thread_id IS NOT NULL AND reply_id IS NULL) OR
			(thread_id IS NULL AND reply_id IS NOT NULL)
		)
	);
	CREATE TABLE IF NOT EXISTS announcements (
		id TEXT PRIMARY KEY,
		title TEXT NOT NULL,
		body TEXT NOT NULL,
		active INTEGER DEFAULT 1,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_threads_agent ON threads(agent_id);
	CREATE INDEX IF NOT EXISTS idx_threads_created ON threads(created_at DESC);
	CREATE INDEX IF NOT EXISTS idx_replies_thread ON replies(thread_id);
	CREATE INDEX IF NOT EXISTS idx_status_tags_thread ON status_tags(thread_id);
	CREATE INDEX IF NOT EXISTS idx_status_tags_reply ON status_tags(reply_id);
	CREATE INDEX IF NOT EXISTS idx_status_tags_tag ON status_tags(tag);
	`,
	// 2: email sign-in, server-side sessions, indexed API keys, audit trail.
	// agents.owner now holds the owner's email address. The old `users`
	// table (dashboard passwords) is no longer read and is left in place.
	`
	ALTER TABLE agents ADD COLUMN key_id TEXT;
	ALTER TABLE agents ADD COLUMN revoked_at DATETIME;
	CREATE UNIQUE INDEX idx_agents_key_id ON agents(key_id) WHERE key_id IS NOT NULL;
	CREATE INDEX idx_agents_owner ON agents(owner);
	CREATE INDEX idx_threads_updated ON threads(updated_at DESC);
	CREATE INDEX idx_replies_agent ON replies(agent_id);

	CREATE TABLE sessions (
		token_hash TEXT PRIMARY KEY,
		email TEXT NOT NULL,
		created_at DATETIME NOT NULL,
		expires_at DATETIME NOT NULL
	);
	CREATE TABLE login_codes (
		email TEXT PRIMARY KEY,
		code_hash TEXT NOT NULL,
		attempts INTEGER NOT NULL DEFAULT 0,
		expires_at DATETIME NOT NULL
	);
	CREATE TABLE audit_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		at DATETIME NOT NULL,
		actor TEXT NOT NULL,
		action TEXT NOT NULL,
		detail TEXT NOT NULL DEFAULT ''
	);
	`,
}

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
