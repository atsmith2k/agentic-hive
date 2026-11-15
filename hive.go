package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	maxTitleLen = 300
	maxBodyLen  = 100_000
	maxTags     = 20
	maxTagLen   = 64
)

// Hive is the application: every operation an agent or a person can
// perform. REST, MCP and the web UI are thin layers over these methods.
type Hive struct {
	db     *DB
	cfg    Config
	mail   Mailer
	log    *log.Logger
	now    func() time.Time
	limits *limiter

	legacyMu sync.Mutex
	legacy   map[string]string // digest of a verified legacy key -> agent id
}

func NewHive(db *DB, cfg Config, mail Mailer, logger *log.Logger) *Hive {
	now := func() time.Time { return time.Now().UTC() }
	return &Hive{db: db, cfg: cfg, mail: mail, log: logger, now: now, limits: newLimiter(now), legacy: map[string]string{}}
}

func (h *Hive) internal(what string, err error) *Error {
	h.log.Printf("error: %s: %v", what, err)
	return errInternal
}

func (h *Hive) audit(ctx context.Context, actor, action, detail string) {
	if _, err := h.db.ExecContext(ctx, `INSERT INTO audit_log (at, actor, action, detail) VALUES (?, ?, ?, ?)`,
		h.now(), actor, action, detail); err != nil {
		h.log.Printf("error: audit %s: %v", action, err)
	}
}

// PurgeExpired removes dead sessions and codes and trims the audit log.
func (h *Hive) PurgeExpired(ctx context.Context) {
	now := h.now()
	h.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, now)
	h.db.ExecContext(ctx, `DELETE FROM login_codes WHERE expires_at < ?`, now)
	h.db.ExecContext(ctx, `DELETE FROM audit_log WHERE at < ?`, now.AddDate(0, 0, -90))
}

// ---- Validation ----

func cleanText(field, s string, max int) (string, *Error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "", errInvalid("%s is required", field)
	case !utf8.ValidString(s):
		return "", errInvalid("%s must be valid UTF-8", field)
	case len(s) > max:
		return "", errInvalid("%s is longer than %d bytes", field, max)
	}
	return s, nil
}

func cleanTags(tags []string) ([]string, *Error) {
	out := []string{}
	seen := map[string]bool{}
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		if len(t) > maxTagLen || !utf8.ValidString(t) {
			return nil, errInvalid("each tag must be at most %d bytes", maxTagLen)
		}
		seen[t] = true
		out = append(out, t)
	}
	if len(out) > maxTags {
		return nil, errInvalid("at most %d tags", maxTags)
	}
	return out, nil
}

// ---- Threads ----

const threadSelect = `SELECT t.id, t.agent_id, a.name, t.title, t.body, t.tags, t.pinned, t.archived,
	t.created_at, t.updated_at, (SELECT COUNT(*) FROM replies r WHERE r.thread_id = t.id)
	FROM threads t JOIN agents a ON t.agent_id = a.id `

func scanThread(row interface{ Scan(...any) error }) (Thread, error) {
	var t Thread
	var tags string
	err := row.Scan(&t.ID, &t.AgentID, &t.AgentName, &t.Title, &t.Body, &tags, &t.Pinned, &t.Archived,
		&t.CreatedAt, &t.UpdatedAt, &t.ReplyCount)
	if json.Unmarshal([]byte(tags), &t.Tags) != nil || t.Tags == nil {
		t.Tags = []string{}
	}
	t.Statuses = []StatusTag{}
	return t, err
}

// queryThreads runs a thread query and attaches each thread's status tags.
func (h *Hive) queryThreads(ctx context.Context, query string, args ...any) ([]Thread, error) {
	rows, err := h.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, h.internal("query threads", err)
	}
	defer rows.Close()
	threads := []Thread{}
	index := map[string]int{}
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, h.internal("scan thread", err)
		}
		index[t.ID] = len(threads)
		threads = append(threads, t)
	}
	if err := rows.Err(); err != nil {
		return nil, h.internal("iterate threads", err)
	}
	if len(threads) == 0 {
		return threads, nil
	}
	ids := make([]any, 0, len(threads))
	for _, t := range threads {
		ids = append(ids, t.ID)
	}
	statuses, err := h.queryStatuses(ctx, `WHERE s.thread_id IN (?`+strings.Repeat(",?", len(ids)-1)+`) ORDER BY s.created_at`, ids...)
	if err != nil {
		return nil, err
	}
	for _, s := range statuses {
		i := index[*s.ThreadID]
		threads[i].Statuses = append(threads[i].Statuses, s)
	}
	return threads, nil
}

type ThreadInput struct {
	Title string   `json:"title"`
	Body  string   `json:"body"`
	Tags  []string `json:"tags"`
}

func (h *Hive) CreateThread(ctx context.Context, actor *Agent, in ThreadInput) (*Thread, error) {
	title, e := cleanText("title", in.Title, maxTitleLen)
	if e != nil {
		return nil, e
	}
	body, e := cleanText("body", in.Body, maxBodyLen)
	if e != nil {
		return nil, e
	}
	tags, e := cleanTags(in.Tags)
	if e != nil {
		return nil, e
	}
	tagsJSON, _ := json.Marshal(tags)
	id, now := uuid.NewString(), h.now()
	if _, err := h.db.ExecContext(ctx,
		`INSERT INTO threads (id, agent_id, title, body, tags, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, actor.ID, title, body, string(tagsJSON), now, now); err != nil {
		return nil, h.internal("create thread", err)
	}
	return h.threadSummary(ctx, id)
}

func (h *Hive) threadSummary(ctx context.Context, id string) (*Thread, error) {
	threads, err := h.queryThreads(ctx, threadSelect+`WHERE t.id = ?`, id)
	if err != nil {
		return nil, err
	}
	if len(threads) == 0 {
		return nil, errNotFound("thread")
	}
	return &threads[0], nil
}

type ThreadFilter struct {
	Tag, Agent, Status, Query string
	Pinned, Archived          *bool
	Since                     time.Time
	SortActivity              bool
	Page, PerPage             int
}

func (h *Hive) ListThreads(ctx context.Context, f ThreadFilter) ([]Thread, int, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PerPage < 1 {
		f.PerPage = 20
	}
	if f.PerPage > 100 {
		f.PerPage = 100
	}
	var conds []string
	var args []any
	add := func(cond string, a ...any) { conds = append(conds, cond); args = append(args, a...) }
	if f.Tag != "" {
		add(h.db.tagFilter(), f.Tag)
	}
	if f.Agent != "" {
		add("(a.name = ? OR a.id = ?)", f.Agent, f.Agent)
	}
	if f.Status != "" {
		if !validStatusTag(f.Status) {
			return nil, 0, errInvalid("status must be one of: %s", strings.Join(statusTags, ", "))
		}
		add("EXISTS (SELECT 1 FROM status_tags st WHERE st.thread_id = t.id AND st.tag = ?)", f.Status)
	}
	if f.Query != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(f.Query) + "%"
		add(`(t.title `+h.db.like()+` ? ESCAPE '\' OR t.body `+h.db.like()+` ? ESCAPE '\')`, like, like)
	}
	if f.Pinned != nil {
		add("t.pinned = ?", *f.Pinned)
	}
	if f.Archived != nil {
		add("t.archived = ?", *f.Archived)
	}
	if !f.Since.IsZero() {
		add("t.updated_at > ?", f.Since.UTC())
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	var total int
	if err := h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM threads t JOIN agents a ON t.agent_id = a.id `+where, args...).Scan(&total); err != nil {
		return nil, 0, h.internal("count threads", err)
	}
	order := " ORDER BY t.created_at DESC"
	if f.SortActivity {
		order = " ORDER BY t.updated_at DESC"
	}
	threads, err := h.queryThreads(ctx, threadSelect+where+order+" LIMIT ? OFFSET ?",
		append(args, f.PerPage, (f.Page-1)*f.PerPage)...)
	return threads, total, err
}

// GetThread returns a thread with its replies and every status tag.
func (h *Hive) GetThread(ctx context.Context, id string) (*Thread, error) {
	t, err := h.threadSummary(ctx, id)
	if err != nil {
		return nil, err
	}
	rows, err := h.db.QueryContext(ctx,
		`SELECT r.id, r.thread_id, r.agent_id, a.name, r.body, r.created_at, r.updated_at
		 FROM replies r JOIN agents a ON r.agent_id = a.id WHERE r.thread_id = ? ORDER BY r.created_at`, id)
	if err != nil {
		return nil, h.internal("query replies", err)
	}
	defer rows.Close()
	t.Replies = []Reply{}
	index := map[string]int{}
	for rows.Next() {
		r := Reply{Statuses: []StatusTag{}}
		if err := rows.Scan(&r.ID, &r.ThreadID, &r.AgentID, &r.AgentName, &r.Body, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, h.internal("scan reply", err)
		}
		index[r.ID] = len(t.Replies)
		t.Replies = append(t.Replies, r)
	}
	if err := rows.Err(); err != nil {
		return nil, h.internal("iterate replies", err)
	}
	statuses, err := h.queryStatuses(ctx,
		`WHERE s.reply_id IN (SELECT id FROM replies WHERE thread_id = ?) ORDER BY s.created_at`, id)
	if err != nil {
		return nil, err
	}
	for _, s := range statuses {
		i := index[*s.ReplyID]
		t.Replies[i].Statuses = append(t.Replies[i].Statuses, s)
	}
	return t, nil
}

type ThreadPatch struct {
	Title *string   `json:"title"`
	Body  *string   `json:"body"`
	Tags  *[]string `json:"tags"`
}

func (h *Hive) UpdateThread(ctx context.Context, actor *Agent, id string, p ThreadPatch) (*Thread, error) {
	if err := h.requireOwner(ctx, actor, "threads", "thread", id); err != nil {
		return nil, err
	}
	var sets []string
	var args []any
	if p.Title != nil {
		v, e := cleanText("title", *p.Title, maxTitleLen)
		if e != nil {
			return nil, e
		}
		sets, args = append(sets, "title = ?"), append(args, v)
	}
	if p.Body != nil {
		v, e := cleanText("body", *p.Body, maxBodyLen)
		if e != nil {
			return nil, e
		}
		sets, args = append(sets, "body = ?"), append(args, v)
	}
	if p.Tags != nil {
		v, e := cleanTags(*p.Tags)
		if e != nil {
			return nil, e
		}
		j, _ := json.Marshal(v)
		sets, args = append(sets, "tags = ?"), append(args, string(j))
	}
	if len(sets) == 0 {
		return nil, errInvalid("nothing to update: send title, body or tags")
	}
	sets, args = append(sets, "updated_at = ?"), append(args, h.now(), id)
	if _, err := h.db.ExecContext(ctx, `UPDATE threads SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
		return nil, h.internal("update thread", err)
	}
	return h.threadSummary(ctx, id)
}

func (h *Hive) DeleteThread(ctx context.Context, actor *Agent, id string) error {
	if err := h.requireOwner(ctx, actor, "threads", "thread", id); err != nil {
		return err
	}
	return h.deleteThread(ctx, id)
}

// deleteThread removes a thread, its replies and their status tags.
// Explicit rather than relying on ON DELETE CASCADE alone.
func (h *Hive) deleteThread(ctx context.Context, id string) error {
	tx, err := h.db.BeginTx(ctx)
	if err != nil {
		return h.internal("begin", err)
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM status_tags WHERE thread_id = ?1 OR reply_id IN (SELECT id FROM replies WHERE thread_id = ?1)`,
		`DELETE FROM replies WHERE thread_id = ?1`,
		`DELETE FROM threads WHERE id = ?1`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return h.internal("delete thread", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return h.internal("commit", err)
	}
	return nil
}

// requireOwner checks that a row exists and was written by actor.
// table is one of our own constants, never caller input.
func (h *Hive) requireOwner(ctx context.Context, actor *Agent, table, what, id string) error {
	var owner string
	err := h.db.QueryRowContext(ctx, `SELECT agent_id FROM `+table+` WHERE id = ?`, id).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return errNotFound(what)
	}
	if err != nil {
		return h.internal("check owner", err)
	}
	if owner != actor.ID {
		return errForbidden("only the agent that wrote this " + what + " can change it")
	}
	return nil
}

// ---- Replies ----

func (h *Hive) getReply(ctx context.Context, id string) (*Reply, error) {
	r := Reply{Statuses: []StatusTag{}}
	err := h.db.QueryRowContext(ctx,
		`SELECT r.id, r.thread_id, r.agent_id, a.name, r.body, r.created_at, r.updated_at
		 FROM replies r JOIN agents a ON r.agent_id = a.id WHERE r.id = ?`, id).
		Scan(&r.ID, &r.ThreadID, &r.AgentID, &r.AgentName, &r.Body, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound("reply")
	}
	if err != nil {
		return nil, h.internal("read reply", err)
	}
	return &r, nil
}

func (h *Hive) CreateReply(ctx context.Context, actor *Agent, threadID, rawBody string) (*Reply, error) {
	body, e := cleanText("body", rawBody, maxBodyLen)
	if e != nil {
		return nil, e
	}
	var archived bool
	err := h.db.QueryRowContext(ctx, `SELECT archived FROM threads WHERE id = ?`, threadID).Scan(&archived)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound("thread")
	}
	if err != nil {
		return nil, h.internal("read thread", err)
	}
	if archived {
		return nil, errConflict("this thread is archived and closed to replies", "start a new thread and reference this one")
	}
	id, now := uuid.NewString(), h.now()
	if _, err := h.db.ExecContext(ctx,
		`INSERT INTO replies (id, thread_id, agent_id, body, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, threadID, actor.ID, body, now, now); err != nil {
		return nil, h.internal("create reply", err)
	}
	h.touchThread(ctx, threadID)
	return h.getReply(ctx, id)
}

func (h *Hive) UpdateReply(ctx context.Context, actor *Agent, id, rawBody string) (*Reply, error) {
	if err := h.requireOwner(ctx, actor, "replies", "reply", id); err != nil {
		return nil, err
	}
	body, e := cleanText("body", rawBody, maxBodyLen)
	if e != nil {
		return nil, e
	}
	if _, err := h.db.ExecContext(ctx, `UPDATE replies SET body = ?, updated_at = ? WHERE id = ?`, body, h.now(), id); err != nil {
		return nil, h.internal("update reply", err)
	}
	r, err := h.getReply(ctx, id)
	if err != nil {
		return nil, err
	}
	if r.Statuses, err = h.queryStatuses(ctx, `WHERE s.reply_id = ? ORDER BY s.created_at`, id); err != nil {
		return nil, err
	}
	return r, nil
}

func (h *Hive) DeleteReply(ctx context.Context, actor *Agent, id string) error {
	if err := h.requireOwner(ctx, actor, "replies", "reply", id); err != nil {
		return err
	}
	if _, err := h.db.ExecContext(ctx, `DELETE FROM status_tags WHERE reply_id = ?`, id); err != nil {
		return h.internal("delete reply statuses", err)
	}
	if _, err := h.db.ExecContext(ctx, `DELETE FROM replies WHERE id = ?`, id); err != nil {
		return h.internal("delete reply", err)
	}
	return nil
}

func (h *Hive) touchThread(ctx context.Context, id string) {
	h.db.ExecContext(ctx, `UPDATE threads SET updated_at = ? WHERE id = ?`, h.now(), id)
}

// ---- Status tags ----

func (h *Hive) queryStatuses(ctx context.Context, where string, args ...any) ([]StatusTag, error) {
	rows, err := h.db.QueryContext(ctx,
		`SELECT s.id, s.thread_id, s.reply_id, s.agent_id, a.name, s.tag, s.reference_id, s.created_at
		 FROM status_tags s JOIN agents a ON s.agent_id = a.id `+where, args...)
	if err != nil {
		return nil, h.internal("query status tags", err)
	}
	defer rows.Close()
	out := []StatusTag{}
	for rows.Next() {
		var s StatusTag
		if err := rows.Scan(&s.ID, &s.ThreadID, &s.ReplyID, &s.AgentID, &s.AgentName, &s.Tag, &s.ReferenceID, &s.CreatedAt); err != nil {
			return nil, h.internal("scan status tag", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, h.internal("iterate status tags", err)
	}
	return out, nil
}

type StatusInput struct {
	Tag         string  `json:"tag"`
	ReferenceID *string `json:"reference_id"`
}

// AddStatus tags a thread (onReply=false) or a reply. Repeating a tag the
// same agent already set returns the existing one, so retries are safe.
func (h *Hive) AddStatus(ctx context.Context, actor *Agent, onReply bool, targetID string, in StatusInput) (*StatusTag, bool, error) {
	if !validStatusTag(in.Tag) {
		return nil, false, errInvalid("tag must be one of: %s", strings.Join(statusTags, ", "))
	}
	var threadID string
	var err error
	if onReply {
		err = h.db.QueryRowContext(ctx, `SELECT thread_id FROM replies WHERE id = ?`, targetID).Scan(&threadID)
	} else {
		err = h.db.QueryRowContext(ctx, `SELECT id FROM threads WHERE id = ?`, targetID).Scan(&threadID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		if onReply {
			return nil, false, errNotFound("reply")
		}
		return nil, false, errNotFound("thread")
	}
	if err != nil {
		return nil, false, h.internal("read status target", err)
	}
	var ref any
	if in.ReferenceID != nil && *in.ReferenceID != "" {
		var n int
		h.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM threads WHERE id = ?1) + (SELECT COUNT(*) FROM replies WHERE id = ?1)`, *in.ReferenceID).Scan(&n)
		if n == 0 {
			return nil, false, errInvalid("reference_id is not the id of a thread or reply")
		}
		if *in.ReferenceID == targetID {
			return nil, false, errInvalid("reference_id cannot be the item being tagged")
		}
		ref = *in.ReferenceID
	} else if in.Tag == "depends-on" {
		return nil, false, errInvalid("depends-on needs reference_id: the thread or reply this depends on")
	}
	col := "thread_id"
	if onReply {
		col = "reply_id"
	}
	existing, err := h.queryStatuses(ctx, `WHERE s.`+col+` = ? AND s.agent_id = ? AND s.tag = ? AND s.reference_id IS NOT DISTINCT FROM ?`,
		targetID, actor.ID, in.Tag, ref)
	if err != nil {
		return nil, false, err
	}
	if len(existing) > 0 {
		return &existing[0], false, nil
	}
	id := uuid.NewString()
	if _, err := h.db.ExecContext(ctx,
		`INSERT INTO status_tags (id, `+col+`, agent_id, tag, reference_id, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, targetID, actor.ID, in.Tag, ref, h.now()); err != nil {
		return nil, false, h.internal("create status tag", err)
	}
	h.touchThread(ctx, threadID)
	created, err := h.queryStatuses(ctx, `WHERE s.id = ?`, id)
	if err != nil || len(created) == 0 {
		return nil, false, errInternal
	}
	return &created[0], true, nil
}

func (h *Hive) DeleteStatus(ctx context.Context, actor *Agent, id string) error {
	if err := h.requireOwner(ctx, actor, "status_tags", "status tag", id); err != nil {
		return err
	}
	if _, err := h.db.ExecContext(ctx, `DELETE FROM status_tags WHERE id = ?`, id); err != nil {
		return h.internal("delete status tag", err)
	}
	return nil
}

func (h *Hive) FindStatuses(ctx context.Context, tag string) ([]StatusTag, error) {
	if !validStatusTag(tag) {
		return nil, errInvalid("tag must be one of: %s", strings.Join(statusTags, ", "))
	}
	return h.queryStatuses(ctx, `WHERE s.tag = ? ORDER BY s.created_at DESC LIMIT 500`, tag)
}

// ---- Shared context ----

func (h *Hive) ListAgents(ctx context.Context, where string, args ...any) ([]Agent, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT `+agentCols+` FROM agents `+where, args...)
	if err != nil {
		return nil, h.internal("query agents", err)
	}
	defer rows.Close()
	out := []Agent{}
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, h.internal("scan agent", err)
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func (h *Hive) Announcements(ctx context.Context, activeOnly bool) ([]Announcement, error) {
	q := `SELECT id, title, body, active, created_at FROM announcements`
	if activeOnly {
		q += ` WHERE active`
	}
	rows, err := h.db.QueryContext(ctx, q+` ORDER BY created_at DESC`)
	if err != nil {
		return nil, h.internal("query announcements", err)
	}
	defer rows.Close()
	out := []Announcement{}
	for rows.Next() {
		var a Announcement
		if err := rows.Scan(&a.ID, &a.Title, &a.Body, &a.Active, &a.CreatedAt); err != nil {
			return nil, h.internal("scan announcement", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

type ActiveContext struct {
	Announcements []Announcement `json:"announcements"`
	InProgress    []Thread       `json:"in_progress"`
	NeedsReview   []Thread       `json:"needs_review"`
	Blocked       []Thread       `json:"blocked"`
	RecentThreads []Thread       `json:"recent_threads"`
}

// ActiveContext is the one call an agent makes to learn what is going on.
// Archived threads are left out of the status lists.
func (h *Hive) ActiveContext(ctx context.Context) (*ActiveContext, error) {
	var c ActiveContext
	var err error
	if c.Announcements, err = h.Announcements(ctx, true); err != nil {
		return nil, err
	}
	byStatus := func(tag string) ([]Thread, error) {
		return h.queryThreads(ctx, threadSelect+`WHERE NOT t.archived AND EXISTS
			(SELECT 1 FROM status_tags s WHERE s.thread_id = t.id AND s.tag = ?) ORDER BY t.updated_at DESC LIMIT 50`, tag)
	}
	if c.InProgress, err = byStatus("in-progress"); err != nil {
		return nil, err
	}
	if c.NeedsReview, err = byStatus("needs-review"); err != nil {
		return nil, err
	}
	if c.Blocked, err = byStatus("blocked"); err != nil {
		return nil, err
	}
	if c.RecentThreads, err = h.queryThreads(ctx, threadSelect+`ORDER BY t.updated_at DESC LIMIT 20`); err != nil {
		return nil, err
	}
	return &c, nil
}

type AgentContext struct {
	Agent          *Agent      `json:"agent"`
	RecentThreads  []Thread    `json:"recent_threads"`
	RecentReplies  []Reply     `json:"recent_replies"`
	ActiveStatuses []StatusTag `json:"active_statuses"`
}

// AgentContext describes what one agent, found by id or name, has been doing.
func (h *Hive) AgentContext(ctx context.Context, idOrName string) (*AgentContext, error) {
	a, err := scanAgent(h.db.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id = ?1 OR name = ?1`, idOrName))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound("agent")
	}
	if err != nil {
		return nil, h.internal("read agent", err)
	}
	c := AgentContext{Agent: a, RecentReplies: []Reply{}}
	if c.RecentThreads, err = h.queryThreads(ctx, threadSelect+`WHERE t.agent_id = ? ORDER BY t.created_at DESC LIMIT 10`, a.ID); err != nil {
		return nil, err
	}
	rows, err := h.db.QueryContext(ctx,
		`SELECT r.id, r.thread_id, t.title, r.body, r.created_at, r.updated_at
		 FROM replies r JOIN threads t ON r.thread_id = t.id WHERE r.agent_id = ? ORDER BY r.created_at DESC LIMIT 10`, a.ID)
	if err != nil {
		return nil, h.internal("query agent replies", err)
	}
	defer rows.Close()
	for rows.Next() {
		r := Reply{AgentID: a.ID, AgentName: a.Name, Statuses: []StatusTag{}}
		if err := rows.Scan(&r.ID, &r.ThreadID, &r.ThreadTitle, &r.Body, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, h.internal("scan agent reply", err)
		}
		c.RecentReplies = append(c.RecentReplies, r)
	}
	if c.ActiveStatuses, err = h.queryStatuses(ctx, `WHERE s.agent_id = ? ORDER BY s.created_at DESC LIMIT 100`, a.ID); err != nil {
		return nil, err
	}
	return &c, nil
}

// Dependencies lists every depends-on or blocked tag that points at
// another thread or reply.
func (h *Hive) Dependencies(ctx context.Context) ([]DependencyEdge, error) {
	rows, err := h.db.QueryContext(ctx, `
		SELECT s.tag,
			COALESCE(s.thread_id, s.reply_id),
			COALESCE(ts.title, trs.title, ''), COALESCE(as1.name, ars.name, ''),
			s.reference_id,
			COALESCE(tr.title, trr.title, ''), COALESCE(ar.name, arr.name, '')
		FROM status_tags s
		LEFT JOIN threads ts ON s.thread_id = ts.id
		LEFT JOIN agents as1 ON ts.agent_id = as1.id
		LEFT JOIN replies rs ON s.reply_id = rs.id
		LEFT JOIN threads trs ON rs.thread_id = trs.id
		LEFT JOIN agents ars ON rs.agent_id = ars.id
		LEFT JOIN threads tr ON s.reference_id = tr.id
		LEFT JOIN agents ar ON tr.agent_id = ar.id
		LEFT JOIN replies rr ON s.reference_id = rr.id
		LEFT JOIN threads trr ON rr.thread_id = trr.id
		LEFT JOIN agents arr ON rr.agent_id = arr.id
		WHERE s.tag IN ('depends-on', 'blocked') AND s.reference_id IS NOT NULL
		ORDER BY s.created_at DESC LIMIT 500`)
	if err != nil {
		return nil, h.internal("query dependencies", err)
	}
	defer rows.Close()
	out := []DependencyEdge{}
	for rows.Next() {
		var e DependencyEdge
		if err := rows.Scan(&e.Status, &e.Source.ID, &e.Source.Title, &e.Source.AgentName,
			&e.DependsOn.ID, &e.DependsOn.Title, &e.DependsOn.AgentName); err != nil {
			return nil, h.internal("scan dependency", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
