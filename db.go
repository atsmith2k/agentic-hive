package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

// DB is the hive's store: Postgres when DATABASE_URL is set, otherwise a
// SQLite file. Queries are written once, with ? placeholders, and rebound
// for Postgres; the few places where the two dialects differ ask the DB.
type DB struct {
	*sql.DB
	postgres bool
}

// OpenDB opens the store the configuration names and brings its schema up
// to date. It never falls back from one store to the other.
func OpenDB(cfg Config) (*DB, error) {
	if cfg.DatabaseURL != "" {
		return openPostgres(cfg.DatabaseURL)
	}
	return openSQLite(cfg.DBPath)
}

func openPostgres(dsn string) (*DB, error) {
	fail := func(err error) (*DB, error) {
		return nil, fmt.Errorf("cannot use DATABASE_URL: %s", redactDSN(dsn, err.Error()))
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		return nil, fmt.Errorf("cannot use DATABASE_URL: it must be a postgres:// connection string with a host and a database name")
	}
	sqldb, err := sql.Open("pgx", dsn)
	if err != nil {
		return fail(err)
	}
	sqldb.SetMaxOpenConns(10)
	sqldb.SetConnMaxIdleTime(5 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sqldb.PingContext(ctx); err != nil {
		sqldb.Close()
		return fail(err)
	}
	db := &DB{DB: sqldb, postgres: true}
	if err := db.migratePostgres(); err != nil {
		sqldb.Close()
		return fail(fmt.Errorf("migrate: %w", err))
	}
	return db, nil
}

// redactDSN keeps a connection string, and above all its password, out of
// an error message.
func redactDSN(dsn, msg string) string {
	msg = strings.Join(strings.Fields(msg), " ") // one log line
	msg = strings.ReplaceAll(msg, dsn, "DATABASE_URL")
	if u, err := url.Parse(dsn); err == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok && pw != "" {
			msg = strings.ReplaceAll(msg, pw, "***")
			msg = strings.ReplaceAll(msg, url.QueryEscape(pw), "***")
		}
	}
	return msg
}

// openSQLite opens the SQLite file. The pragmas are part of the DSN so that
// every pooled connection gets them.
func openSQLite(path string) (*DB, error) {
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
	sqldb, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if path == ":memory:" {
		sqldb.SetMaxOpenConns(1) // each in-memory connection is its own database
	}
	db := &DB{DB: sqldb}
	if err := db.migrateSQLite(); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	if path != ":memory:" {
		os.Chmod(path, 0o600)
	}
	return db, nil
}

// ---- Dialect ----

var placeholderRE = regexp.MustCompile(`\?(\d*)`)

// rebind turns ? and ?N placeholders into Postgres's $N. No query here has
// a literal question mark, so a plain scan is enough.
func (d *DB) rebind(query string) string {
	if !d.postgres {
		return query
	}
	n := 0
	return placeholderRE.ReplaceAllStringFunc(query, func(m string) string {
		if len(m) > 1 {
			return "$" + m[1:]
		}
		n++
		return "$" + strconv.Itoa(n)
	})
}

// tagFilter is a condition, with one placeholder, that is true when the
// JSON array in t.tags contains that tag.
func (d *DB) tagFilter() string {
	if d.postgres {
		return "EXISTS (SELECT 1 FROM json_array_elements_text(t.tags::json) AS tag WHERE tag = ?)"
	}
	return "EXISTS (SELECT 1 FROM json_each(t.tags) WHERE json_each.value = ?)"
}

// like is the case-insensitive pattern match: SQLite's LIKE already is.
func (d *DB) like() string {
	if d.postgres {
		return "ILIKE"
	}
	return "LIKE"
}

func (d *DB) Exec(query string, args ...any) (sql.Result, error) {
	return d.DB.Exec(d.rebind(query), args...)
}

func (d *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return d.DB.ExecContext(ctx, d.rebind(query), args...)
}

func (d *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return d.DB.QueryContext(ctx, d.rebind(query), args...)
}

func (d *DB) QueryRow(query string, args ...any) *sql.Row {
	return d.DB.QueryRow(d.rebind(query), args...)
}

func (d *DB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return d.DB.QueryRowContext(ctx, d.rebind(query), args...)
}

// Tx is a transaction that rebinds placeholders like DB does.
type Tx struct {
	*sql.Tx
	db *DB
}

func (d *DB) BeginTx(ctx context.Context) (*Tx, error) {
	tx, err := d.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &Tx{Tx: tx, db: d}, nil
}

func (t *Tx) Exec(query string, args ...any) (sql.Result, error) {
	return t.Tx.Exec(t.db.rebind(query), args...)
}

func (t *Tx) QueryRow(query string, args ...any) *sql.Row {
	return t.Tx.QueryRow(t.db.rebind(query), args...)
}

// ---- Schema ----

// sqliteMigrations run in order; PRAGMA user_version records how many have
// run. Never edit one that has shipped: add a new one, and add the same
// change to postgresMigrations.
var sqliteMigrations = []string{
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

// postgresMigrations run in order; the schema_migrations table records how
// many have run. The first is the whole current schema: a Postgres database
// is always created fresh, so it has no history to replay. Never edit one
// that has shipped: add a new one, and add the same change to
// sqliteMigrations.
var postgresMigrations = []string{
	`
	CREATE TABLE agents (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		owner TEXT NOT NULL,
		api_key_hash TEXT NOT NULL,
		key_id TEXT,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		revoked_at TIMESTAMPTZ
	);
	CREATE UNIQUE INDEX idx_agents_key_id ON agents(key_id) WHERE key_id IS NOT NULL;
	CREATE INDEX idx_agents_owner ON agents(owner);

	CREATE TABLE threads (
		id TEXT PRIMARY KEY,
		agent_id TEXT NOT NULL REFERENCES agents(id),
		title TEXT NOT NULL,
		body TEXT NOT NULL,
		tags TEXT NOT NULL DEFAULT '[]',
		pinned BOOLEAN NOT NULL DEFAULT FALSE,
		archived BOOLEAN NOT NULL DEFAULT FALSE,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
	);
	CREATE INDEX idx_threads_agent ON threads(agent_id);
	CREATE INDEX idx_threads_created ON threads(created_at DESC);
	CREATE INDEX idx_threads_updated ON threads(updated_at DESC);

	CREATE TABLE replies (
		id TEXT PRIMARY KEY,
		thread_id TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
		agent_id TEXT NOT NULL REFERENCES agents(id),
		body TEXT NOT NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
	);
	CREATE INDEX idx_replies_thread ON replies(thread_id);
	CREATE INDEX idx_replies_agent ON replies(agent_id);

	CREATE TABLE status_tags (
		id TEXT PRIMARY KEY,
		thread_id TEXT REFERENCES threads(id) ON DELETE CASCADE,
		reply_id TEXT REFERENCES replies(id) ON DELETE CASCADE,
		agent_id TEXT NOT NULL REFERENCES agents(id),
		tag TEXT NOT NULL CHECK (tag IN ('acknowledged','depends-on','blocked','resolved','in-progress','needs-review')),
		reference_id TEXT,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		CHECK (
			(thread_id IS NOT NULL AND reply_id IS NULL) OR
			(thread_id IS NULL AND reply_id IS NOT NULL)
		)
	);
	CREATE INDEX idx_status_tags_thread ON status_tags(thread_id);
	CREATE INDEX idx_status_tags_reply ON status_tags(reply_id);
	CREATE INDEX idx_status_tags_tag ON status_tags(tag);

	CREATE TABLE announcements (
		id TEXT PRIMARY KEY,
		title TEXT NOT NULL,
		body TEXT NOT NULL,
		active BOOLEAN NOT NULL DEFAULT TRUE,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now()
	);

	CREATE TABLE sessions (
		token_hash TEXT PRIMARY KEY,
		email TEXT NOT NULL,
		created_at TIMESTAMPTZ NOT NULL,
		expires_at TIMESTAMPTZ NOT NULL
	);
	CREATE TABLE login_codes (
		email TEXT PRIMARY KEY,
		code_hash TEXT NOT NULL,
		attempts INTEGER NOT NULL DEFAULT 0,
		expires_at TIMESTAMPTZ NOT NULL
	);
	CREATE TABLE audit_log (
		id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		at TIMESTAMPTZ NOT NULL,
		actor TEXT NOT NULL,
		action TEXT NOT NULL,
		detail TEXT NOT NULL DEFAULT ''
	);
	`,
}

func (d *DB) migrateSQLite() error {
	var version int
	if err := d.DB.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > len(sqliteMigrations) {
		return fmt.Errorf("the database is at schema version %d, newer than this binary understands (%d)", version, len(sqliteMigrations))
	}
	for i := version; i < len(sqliteMigrations); i++ {
		tx, err := d.DB.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(sqliteMigrations[i]); err != nil {
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

// migratePostgres holds an advisory lock for the duration, so two
// instances starting together do not both create the schema.
func (d *DB) migratePostgres() error {
	ctx := context.Background()
	conn, err := d.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	const lockID = 0x68697665 // "hive"
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, lockID); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, lockID)
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	var version int
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return err
	}
	if version > len(postgresMigrations) {
		return fmt.Errorf("the database is at schema version %d, newer than this binary understands (%d)", version, len(postgresMigrations))
	}
	for i := version; i < len(postgresMigrations); i++ {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(postgresMigrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version) VALUES ($1)`, i+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
