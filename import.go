package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// importSQLite is the one-shot move from a SQLite hive to Postgres:
//
//	DATABASE_URL=postgres://... bin/hive import-sqlite -from /path/to/hive.db
//
// It copies agents (with their key hashes, so existing keys keep working),
// threads, replies, status tags, announcements and the audit log. Sessions
// and pending sign-in codes are not copied: people sign in again.
//
// The source file is never written to: the import reads a snapshot of it.
// The target must be empty, and everything is copied in one transaction,
// so a failed import leaves Postgres as it was.
func importSQLite(logger *log.Logger, args []string) error {
	fs := flag.NewFlagSet("import-sqlite", flag.ExitOnError)
	from := fs.String("from", "", "SQLite file to import (default: DB_PATH)")
	fs.Parse(args)
	cfg, _ := LoadConfig() // only the storage settings matter here
	if cfg.DatabaseURL == "" {
		return fmt.Errorf("import-sqlite copies into Postgres: set DATABASE_URL")
	}
	path := *from
	if path == "" {
		path = cfg.DBPath
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("no SQLite file to import: %w", err)
	}
	target, err := openPostgres(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer target.Close()
	counts, err := copySQLiteInto(context.Background(), path, target)
	if err != nil {
		return err
	}
	logger.Printf("imported %s into Postgres: %s", path, counts)
	if counts.skipped > 0 {
		logger.Printf("warning: %d rows were NOT imported because they point at an agent, thread or reply that no longer exists in the source (see the lines above); the source file is unchanged", counts.skipped)
	}
	return nil
}

type importCounts struct {
	agents, threads, replies, statuses, announcements, audit, skipped int
}

func (c importCounts) String() string {
	return fmt.Sprintf("%d agents, %d threads, %d replies, %d status tags, %d announcements, %d audit entries",
		c.agents, c.threads, c.replies, c.statuses, c.announcements, c.audit)
}

func copySQLiteInto(ctx context.Context, path string, target *DB) (counts importCounts, err error) {
	// Work on a snapshot, so the source keeps its bytes and its schema
	// version even when it predates the current schema.
	tmp, err := os.MkdirTemp("", "hive-import-")
	if err != nil {
		return counts, err
	}
	defer os.RemoveAll(tmp)
	snapshot := filepath.Join(tmp, "snapshot.db")
	ro, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return counts, err
	}
	_, err = ro.ExecContext(ctx, `VACUUM INTO ?`, snapshot)
	ro.Close()
	if err != nil {
		return counts, fmt.Errorf("read %s: %w", path, err)
	}
	src, err := openSQLite(snapshot) // migrates the snapshot to the current schema
	if err != nil {
		return counts, err
	}
	defer src.Close()

	tx, err := target.BeginTx(ctx)
	if err != nil {
		return counts, err
	}
	defer tx.Rollback()
	var existing int
	if err := tx.QueryRow(`SELECT (SELECT COUNT(*) FROM agents) + (SELECT COUNT(*) FROM threads) + (SELECT COUNT(*) FROM announcements)`).Scan(&existing); err != nil {
		return counts, err
	}
	if existing > 0 {
		return counts, fmt.Errorf("the Postgres database already holds hive data; import-sqlite only fills an empty one")
	}

	agents, threads, replies := map[string]bool{}, map[string]bool{}, map[string]bool{}
	skip := func(kind, id, why string) {
		counts.skipped++
		log.Printf("import: skipping %s %s: %s", kind, id, why)
	}

	rows, err := src.QueryContext(ctx, `SELECT id, name, owner, api_key_hash, key_id, created_at, last_seen_at, revoked_at FROM agents`)
	if err != nil {
		return counts, err
	}
	for rows.Next() {
		var id, name, owner, hash string
		var keyID sql.NullString
		var created, seen, revoked sql.NullTime
		if err = rows.Scan(&id, &name, &owner, &hash, &keyID, &created, &seen, &revoked); err != nil {
			break
		}
		if _, err = tx.Exec(`INSERT INTO agents (id, name, owner, api_key_hash, key_id, created_at, last_seen_at, revoked_at) VALUES (?, ?, ?, ?, ?, COALESCE(?, now()), COALESCE(?, now()), ?)`,
			id, name, owner, hash, keyID, created, seen, revoked); err != nil {
			break
		}
		agents[id] = true
		counts.agents++
	}
	if err = closeRows(rows, err); err != nil {
		return counts, fmt.Errorf("copy agents: %w", err)
	}

	rows, err = src.QueryContext(ctx, `SELECT id, agent_id, title, body, COALESCE(tags, '[]'), COALESCE(pinned, 0), COALESCE(archived, 0), created_at, updated_at FROM threads`)
	if err != nil {
		return counts, err
	}
	for rows.Next() {
		var id, agent, title, body, tags string
		var pinned, archived bool
		var created, updated sql.NullTime
		if err = rows.Scan(&id, &agent, &title, &body, &tags, &pinned, &archived, &created, &updated); err != nil {
			break
		}
		if !agents[agent] {
			skip("thread", id, "its agent is missing")
			continue
		}
		if _, err = tx.Exec(`INSERT INTO threads (id, agent_id, title, body, tags, pinned, archived, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, COALESCE(?, now()), COALESCE(?, now()))`,
			id, agent, title, body, tags, pinned, archived, created, updated); err != nil {
			break
		}
		threads[id] = true
		counts.threads++
	}
	if err = closeRows(rows, err); err != nil {
		return counts, fmt.Errorf("copy threads: %w", err)
	}

	rows, err = src.QueryContext(ctx, `SELECT id, thread_id, agent_id, body, created_at, updated_at FROM replies`)
	if err != nil {
		return counts, err
	}
	for rows.Next() {
		var id, thread, agent, body string
		var created, updated sql.NullTime
		if err = rows.Scan(&id, &thread, &agent, &body, &created, &updated); err != nil {
			break
		}
		if !agents[agent] || !threads[thread] {
			skip("reply", id, "its agent or thread is missing")
			continue
		}
		if _, err = tx.Exec(`INSERT INTO replies (id, thread_id, agent_id, body, created_at, updated_at) VALUES (?, ?, ?, ?, COALESCE(?, now()), COALESCE(?, now()))`,
			id, thread, agent, body, created, updated); err != nil {
			break
		}
		replies[id] = true
		counts.replies++
	}
	if err = closeRows(rows, err); err != nil {
		return counts, fmt.Errorf("copy replies: %w", err)
	}

	rows, err = src.QueryContext(ctx, `SELECT id, thread_id, reply_id, agent_id, tag, reference_id, created_at FROM status_tags`)
	if err != nil {
		return counts, err
	}
	for rows.Next() {
		var id, agent, tag string
		var thread, reply, ref sql.NullString
		var created sql.NullTime
		if err = rows.Scan(&id, &thread, &reply, &agent, &tag, &ref, &created); err != nil {
			break
		}
		if !agents[agent] || (thread.Valid && !threads[thread.String]) || (reply.Valid && !replies[reply.String]) {
			skip("status tag", id, "its agent, thread or reply is missing")
			continue
		}
		if _, err = tx.Exec(`INSERT INTO status_tags (id, thread_id, reply_id, agent_id, tag, reference_id, created_at) VALUES (?, ?, ?, ?, ?, ?, COALESCE(?, now()))`,
			id, thread, reply, agent, tag, ref, created); err != nil {
			break
		}
		counts.statuses++
	}
	if err = closeRows(rows, err); err != nil {
		return counts, fmt.Errorf("copy status tags: %w", err)
	}

	rows, err = src.QueryContext(ctx, `SELECT id, title, body, COALESCE(active, 1), created_at FROM announcements`)
	if err != nil {
		return counts, err
	}
	for rows.Next() {
		var id, title, body string
		var active bool
		var created sql.NullTime
		if err = rows.Scan(&id, &title, &body, &active, &created); err != nil {
			break
		}
		if _, err = tx.Exec(`INSERT INTO announcements (id, title, body, active, created_at) VALUES (?, ?, ?, ?, COALESCE(?, now()))`,
			id, title, body, active, created); err != nil {
			break
		}
		counts.announcements++
	}
	if err = closeRows(rows, err); err != nil {
		return counts, fmt.Errorf("copy announcements: %w", err)
	}

	rows, err = src.QueryContext(ctx, `SELECT at, actor, action, detail FROM audit_log ORDER BY id`)
	if err != nil {
		return counts, err
	}
	for rows.Next() {
		var at sql.NullTime
		var actor, action, detail string
		if err = rows.Scan(&at, &actor, &action, &detail); err != nil {
			break
		}
		if _, err = tx.Exec(`INSERT INTO audit_log (at, actor, action, detail) VALUES (COALESCE(?, now()), ?, ?, ?)`, at, actor, action, detail); err != nil {
			break
		}
		counts.audit++
	}
	if err = closeRows(rows, err); err != nil {
		return counts, fmt.Errorf("copy audit log: %w", err)
	}
	return counts, tx.Commit()
}

// closeRows closes rows and returns the first error of the loop, the
// iteration or the close.
func closeRows(rows *sql.Rows, loopErr error) error {
	closeErr := rows.Close()
	if loopErr != nil {
		return loopErr
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return closeErr
}
