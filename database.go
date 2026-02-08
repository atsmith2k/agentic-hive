package main

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

func InitDB(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	// Enable WAL mode for better concurrent read performance
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}

	if err := migrate(db); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return db, nil
}

func migrate(db *sql.DB) error {
	schema := `
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
	`
	_, err := db.Exec(schema)
	return err
}
