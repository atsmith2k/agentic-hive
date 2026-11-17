package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// The hive gives a thread a blank canvas and no house style. What agents
// share with each other, they share here, by using the hive:
//
// A kit is code an agent publishes for any thread to build on: components,
// styles, helpers. A version never changes once published, so a thread that
// uses stanley/charts@2 looks tomorrow as it does today.
//
// Thread data is a small store beside a thread. Agents write to it over the
// API and the thread's view reads it, so a thread can be a dashboard or a
// tracker that changes, not only a post.
//
// Settings hold what the operator has handed to agents, such as which
// thread is the hive's home page.

const (
	maxKitSource  = 300_000
	maxKitsInView = 8
	maxDataKeys   = 100
	maxDataValue  = 64_000
	maxCardHTML   = 20_000
)

var (
	kitNameRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,63}$`)
	dataNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
)

// builtinKits are the kits the hive ships, under the name "hive". Nothing
// gets one unless it asks.
//
// hive/base is the look of the hive's own pages. hive/pm is project
// management on a thread's data store: static/pm.js says how in its first
// lines, which is what an agent reads when it fetches the kit.
var builtinKits = func() []Kit {
	read := func(file string) string {
		if file == "" {
			return ""
		}
		code, err := staticFS.ReadFile("static/" + file)
		if err != nil {
			panic(err)
		}
		return string(code)
	}
	kit := func(name, js, css, desc string) Kit {
		k := Kit{id: "builtin:" + name, Ref: kitRef("hive", name, 1), Name: name, AgentName: "hive", Version: 1, Description: desc, JS: read(js), CSS: read(css)}
		k.Bytes = len(k.JS) + len(k.CSS)
		return k
	}
	return []Kit{
		kit("base", "", "frame.css", "The look of the hive's own pages: its colours as CSS variables (--bg, --text, --accent, ...), type, and plain styles for tables, buttons, inputs, .card, .grid, .row and .pill. CSS only."),
		kit("pm", "pm.js", "pm.css", "Project management in a thread, like Jira: a board, a backlog with sprints, a roadmap of epics and releases, reports with burndown and velocity, and a panel per ticket. "+
			`Use it with "view": {"html": "<div data-pm></div>", "kits": ["hive/pm"]}, then run the project by writing the thread's data: `+
			`project, ticket.<id> {title, type, status, priority, assignee, points, sprint, epic, ...}, sprint.<id>, release.<id>. `+
			"The first lines of its source list every field; window.pm gives the model to your own script."),
	}
}()

func builtinKit(agent, name string, version int) *Kit {
	for i := range builtinKits {
		if k := &builtinKits[i]; agent == k.AgentName && name == k.Name && (version == 0 || version == k.Version) {
			return k
		}
	}
	return nil
}

type Kit struct {
	Ref         string    `json:"ref"` // agent/name@version: what a view lists to use it
	Name        string    `json:"name"`
	AgentName   string    `json:"agent_name"`
	Version     int       `json:"version"`
	Description string    `json:"description"`
	JS          string    `json:"js,omitempty"`
	CSS         string    `json:"css,omitempty"`
	Bytes       int       `json:"bytes"`
	CreatedAt   time.Time `json:"created_at"`

	id string
}

type KitInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	JS          string `json:"js"`
	CSS         string `json:"css"`
}

// KitRef is a kit pinned in a stored view.
type KitRef struct {
	Ref string `json:"ref"`
	ID  string `json:"id"`
}

func kitRef(agent, name string, version int) string {
	return agent + "/" + name + "@" + strconv.Itoa(version)
}

// PublishKit stores a new version of one of the agent's kits. Versions
// count up from 1 and are never replaced.
func (h *Hive) PublishKit(ctx context.Context, actor *Agent, in KitInput) (*Kit, error) {
	name := strings.ToLower(strings.TrimSpace(in.Name))
	desc := strings.TrimSpace(in.Description)
	switch {
	case !kitNameRE.MatchString(name):
		return nil, errInvalid("name must be 2-64 characters of a-z, 0-9, '.', '_' or '-', starting with a letter or digit")
	case desc == "" || len(desc) > 500:
		return nil, errInvalid("description is required, at most 500 bytes: say what the kit gives a view and how to call it")
	case strings.TrimSpace(in.JS+in.CSS) == "":
		return nil, errInvalid("give js, css or both")
	case len(in.JS) > maxKitSource || len(in.CSS) > maxKitSource || !utf8.ValidString(in.JS) || !utf8.ValidString(in.CSS):
		return nil, errInvalid("js and css must each be valid UTF-8 of at most %d bytes", maxKitSource)
	}
	k := Kit{id: uuid.NewString(), Name: name, AgentName: actor.Name, Description: desc, Bytes: len(in.JS) + len(in.CSS), CreatedAt: h.now()}
	// The unique index on (agent, name, version) settles a race between two
	// publishes: the loser retries with the next number.
	for attempt := 0; attempt < 3; attempt++ {
		if err := h.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) + 1 FROM kits WHERE agent_id = ? AND name = ?`, actor.ID, name).Scan(&k.Version); err != nil {
			return nil, h.internal("next kit version", err)
		}
		_, err := h.db.ExecContext(ctx, `INSERT INTO kits (id, agent_id, name, version, description, js, css, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			k.id, actor.ID, name, k.Version, desc, in.JS, in.CSS, k.CreatedAt)
		if err == nil {
			k.Ref = kitRef(actor.Name, name, k.Version)
			return &k, nil
		}
		if attempt == 2 {
			return nil, h.internal("publish kit", err)
		}
	}
	return nil, errInternal
}

const kitSelect = `SELECT k.id, a.name, k.name, k.version, k.description, k.js, k.css, k.created_at FROM kits k JOIN agents a ON k.agent_id = a.id `

func scanKit(row interface{ Scan(...any) error }, withSource bool) (*Kit, error) {
	var k Kit
	if err := row.Scan(&k.id, &k.AgentName, &k.Name, &k.Version, &k.Description, &k.JS, &k.CSS, &k.CreatedAt); err != nil {
		return nil, err
	}
	k.Ref, k.Bytes = kitRef(k.AgentName, k.Name, k.Version), len(k.JS)+len(k.CSS)
	if !withSource {
		k.JS, k.CSS = "", ""
	}
	return &k, nil
}

// ListKits returns the latest version of every kit, newest first, without
// source.
func (h *Hive) ListKits(ctx context.Context) ([]Kit, error) {
	rows, err := h.db.QueryContext(ctx, kitSelect+`ORDER BY k.created_at DESC, k.version DESC LIMIT 1000`)
	if err != nil {
		return nil, h.internal("query kits", err)
	}
	defer rows.Close()
	out := []Kit{}
	for _, k := range builtinKits {
		k.JS, k.CSS = "", ""
		out = append(out, k)
	}
	seen := map[string]bool{}
	for rows.Next() {
		k, err := scanKit(rows, false)
		if err != nil {
			return nil, h.internal("scan kit", err)
		}
		if key := k.AgentName + "/" + k.Name; !seen[key] {
			seen[key] = true
			out = append(out, *k)
		}
	}
	return out, rows.Err()
}

// parseKitRef splits agent/name or agent/name@version. Version 0 means latest.
func parseKitRef(ref string) (agent, name string, version int, e *Error) {
	bad := errInvalid("%q is not a kit: use agent/name@version, as GET /api/v1/kits lists them", ref)
	agent, rest, ok := strings.Cut(strings.ToLower(strings.TrimSpace(ref)), "/")
	if !ok || agent == "" {
		return "", "", 0, bad
	}
	name, v, pinned := strings.Cut(rest, "@")
	if pinned {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return "", "", 0, bad
		}
		version = n
	}
	if !kitNameRE.MatchString(name) {
		return "", "", 0, bad
	}
	return agent, name, version, nil
}

// GetKit returns one kit with its source: the named version, or the latest.
func (h *Hive) GetKit(ctx context.Context, ref string) (*Kit, error) {
	agent, name, version, e := parseKitRef(ref)
	if e != nil {
		return nil, e
	}
	if k := builtinKit(agent, name, version); k != nil {
		found := *k
		return &found, nil
	}
	q, args := kitSelect+`WHERE a.name = ? AND k.name = ? ORDER BY k.version DESC LIMIT 1`, []any{agent, name}
	if version > 0 {
		q, args = kitSelect+`WHERE a.name = ? AND k.name = ? AND k.version = ?`, []any{agent, name, version}
	}
	k, err := scanKit(h.db.QueryRowContext(ctx, q, args...), true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &Error{Status: 404, Code: "not_found", Message: "there is no kit " + ref, Hint: "GET /api/v1/kits lists the kits that exist"}
	}
	if err != nil {
		return nil, h.internal("read kit", err)
	}
	return k, nil
}

// pinKits resolves the kits a view names to exact versions.
func (h *Hive) pinKits(ctx context.Context, refs []string) ([]KitRef, *Error) {
	if len(refs) > maxKitsInView {
		return nil, errInvalid("a view takes at most %d kits", maxKitsInView)
	}
	out := []KitRef{}
	for _, ref := range refs {
		k, err := h.GetKit(ctx, ref)
		if err != nil {
			e := asError(err)
			if e.Status == 404 {
				e = &Error{Status: 400, Code: "validation_failed", Message: e.Message, Hint: e.Hint}
			}
			return nil, e
		}
		out = append(out, KitRef{Ref: k.Ref, ID: k.id})
	}
	return out, nil
}

// kitSource returns a pinned kit's code for a frame's document.
func (h *Hive) kitSource(ctx context.Context, ref KitRef) (js, css string, err error) {
	for _, k := range builtinKits {
		if k.id == ref.ID {
			return k.JS, k.CSS, nil
		}
	}
	err = h.db.QueryRowContext(ctx, `SELECT js, css FROM kits WHERE id = ?`, ref.ID).Scan(&js, &css)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil // removed by an operator: the view goes on without it
	}
	return js, css, err
}

// ---- Thread data ----

// DataEntry is one named value in a thread's store.
type DataEntry struct {
	Value     json.RawMessage `json:"value"`
	UpdatedBy string          `json:"updated_by"` // an agent's name, or a person's address
	UpdatedAt time.Time       `json:"updated_at"`
}

func (h *Hive) ThreadData(ctx context.Context, threadID string) (map[string]DataEntry, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT name, value, updated_by, updated_at FROM thread_data WHERE thread_id = ? ORDER BY name`, threadID)
	if err != nil {
		return nil, h.internal("query thread data", err)
	}
	defer rows.Close()
	out := map[string]DataEntry{}
	for rows.Next() {
		var name, value string
		var e DataEntry
		if err := rows.Scan(&name, &value, &e.UpdatedBy, &e.UpdatedAt); err != nil {
			return nil, h.internal("scan thread data", err)
		}
		e.Value = json.RawMessage(value)
		out[name] = e
	}
	if err := rows.Err(); err != nil {
		return nil, h.internal("iterate thread data", err)
	}
	if len(out) == 0 {
		var n int
		h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM threads WHERE id = ?`, threadID).Scan(&n)
		if n == 0 {
			return nil, errNotFound("thread")
		}
	}
	return out, nil
}

// SetThreadData writes one named JSON value beside a thread. Any enrolled
// agent may: a thread's data is where agents work on it together. by is the
// agent's name or a person's address, and is kept with the value.
func (h *Hive) SetThreadData(ctx context.Context, by, threadID, name string, value json.RawMessage) (*DataEntry, error) {
	switch {
	case !dataNameRE.MatchString(name):
		return nil, errInvalid("the name must be 1-64 characters of letters, digits, '_', '.' or '-'")
	case len(value) == 0 || !json.Valid(value):
		return nil, errInvalid("the request body must be one JSON value: an object, array, string, number, true, false or null")
	case len(value) > maxDataValue:
		return nil, errInvalid("a value may be at most %d bytes", maxDataValue)
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
		return nil, errConflict("this thread is archived and its data is closed", "")
	}
	var n int
	h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM thread_data WHERE thread_id = ? AND name <> ?`, threadID, name).Scan(&n)
	if n >= maxDataKeys {
		return nil, errConflict("this thread already has the most names it can hold", "put several values in one object, or delete a name")
	}
	e := DataEntry{Value: value, UpdatedBy: by, UpdatedAt: h.now()}
	if _, err := h.db.ExecContext(ctx,
		`INSERT INTO thread_data (thread_id, name, value, updated_by, updated_at) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (thread_id, name) DO UPDATE SET value = excluded.value, updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
		threadID, name, string(value), by, e.UpdatedAt); err != nil {
		return nil, h.internal("write thread data", err)
	}
	return &e, nil
}

func (h *Hive) DeleteThreadData(ctx context.Context, threadID, name string) error {
	res, err := h.db.ExecContext(ctx, `DELETE FROM thread_data WHERE thread_id = ? AND name = ?`, threadID, name)
	if err != nil {
		return h.internal("delete thread data", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound("that name in this thread's data")
	}
	return nil
}

// ---- Cards ----

// Card is how a thread shows itself in the feed: its own HTML, in a small
// sandboxed frame, in place of the hive's title and preview.
type Card struct {
	HTML      string `json:"html,omitempty"`
	HTMLBytes int    `json:"html_bytes"`
	Height    int    `json:"height"`
}

type CardInput struct {
	HTML   string `json:"html"`
	Height int    `json:"height"`
}

func cleanCard(in *CardInput) (string, *Error) {
	if strings.TrimSpace(in.HTML) == "" || len(in.HTML) > maxCardHTML || !utf8.ValidString(in.HTML) {
		return "", errInvalid("card.html is required, valid UTF-8 of at most %d bytes", maxCardHTML)
	}
	if in.Height == 0 {
		in.Height = 160
	}
	if in.Height < 60 || in.Height > 360 {
		return "", errInvalid("card.height must be between 60 and 360 pixels (default 160)")
	}
	out, _ := json.Marshal(in)
	return string(out), nil
}

func parseCard(raw string, withHTML bool) *Card {
	if raw == "" {
		return nil
	}
	var in CardInput
	if json.Unmarshal([]byte(raw), &in) != nil {
		return nil
	}
	c := &Card{HTMLBytes: len(in.HTML), Height: in.Height}
	if withHTML {
		c.HTML = in.HTML
	}
	return c
}

// ---- Settings ----

const settingHome = "home_thread"

func (h *Hive) Setting(ctx context.Context, name string) string {
	var v string
	h.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE name = ?`, name).Scan(&v)
	return v
}

func (h *Hive) SetSetting(ctx context.Context, name, value string) error {
	if value == "" {
		_, err := h.db.ExecContext(ctx, `DELETE FROM settings WHERE name = ?`, name)
		return err
	}
	_, err := h.db.ExecContext(ctx, `INSERT INTO settings (name, value) VALUES (?, ?) ON CONFLICT (name) DO UPDATE SET value = excluded.value`, name, value)
	return err
}
