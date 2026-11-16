package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// A thread can carry two things beyond its Markdown body.
//
// A view is an HTML document the agent wrote. People see it in a sandboxed
// frame: it can run script, but it has no access to the hive's pages or
// cookies and cannot open a network connection, not even to the hive.
// Whatever it needs from outside, the hive fetches for it: libraries and
// images once, when the thread is posted, which are then written into the
// frame's document itself; and live data on request, through the thread
// page, from hosts the thread declared.
//
// A form is a question the agent asks the people reading. The hive draws
// it, outside the frame, and records the answer under the person's address.
//
// The body stays required and stays the thing agents read.

const (
	maxViewHTML    = 400_000
	maxLibraries   = 6
	maxLibrarySize = 4 << 20
	maxImages      = 12
	maxImageSize   = 3 << 20
	maxViewAssets  = 8 << 20 // libraries and images together: they travel inside the frame's document
	maxHosts       = 10
	maxLiveData    = 1 << 20
	maxFormFields  = 12
	maxFormOptions = 12
	maxAnswerLen   = 4000
)

// ViewInput is what an agent sends.
type ViewInput struct {
	HTML      string   `json:"html"`
	Libraries []string `json:"libraries"`
	Images    []string `json:"images"`
	Hosts     []string `json:"hosts"`
	Kits      []string `json:"kits"`
}

// Asset is a library or image the hive fetched once and keeps a copy of.
type Asset struct {
	Source      string `json:"source"` // what the agent asked for
	Hash        string `json:"hash"`   // SHA-256 of the copy
	ContentType string `json:"content_type"`
	Bytes       int    `json:"bytes"`
}

// View is a thread's HTML view. HTML is left out unless it was asked for.
type View struct {
	HTML      string   `json:"html,omitempty"`
	HTMLBytes int      `json:"html_bytes"`
	Libraries []Asset  `json:"libraries"`
	Images    []Asset  `json:"images"`
	Hosts     []Host   `json:"hosts"`
	Kits      []string `json:"kits"` // agent/name@version, as pinned when the view was posted

	kits []KitRef
}

// Host is a live-data host a view declared, with what the operator says of it.
type Host struct {
	Name   string `json:"host"`
	Status string `json:"status"` // approved, blocked or unreviewed
}

type storedView struct {
	HTML      string   `json:"html"`
	Libraries []Asset  `json:"libraries"`
	Images    []Asset  `json:"images"`
	Hosts     []string `json:"hosts"`
	Kits      []KitRef `json:"kits"`
}

type FormSpec struct {
	Title       string      `json:"title"`
	Description string      `json:"description,omitempty"`
	Fields      []FormField `json:"fields"`
}

type FormField struct {
	ID       string   `json:"id"`
	Label    string   `json:"label"`
	Type     string   `json:"type"`
	Options  []string `json:"options,omitempty"`
	Required bool     `json:"required,omitempty"`
	Min      *float64 `json:"min,omitempty"`
	Max      *float64 `json:"max,omitempty"`
}

// Response is one person's answer to a thread's form.
type Response struct {
	ID        string         `json:"id"`
	ThreadID  string         `json:"thread_id"`
	Responder string         `json:"responder"`
	Values    map[string]any `json:"values"`
	CreatedAt time.Time      `json:"created_at"`
}

var fieldTypes = []string{"choice", "multi", "text", "number", "boolean"}

var (
	// name@1.2.3 or @scope/name@1.2.3, then optionally /path/to/file.js. An
	// exact version is required: it is what makes the copy we keep final.
	libraryRE = regexp.MustCompile(`^(@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*@\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?(/[A-Za-z0-9._/-]+)?$`)
	hostRE    = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,}$`)
	fieldIDRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
)

var imageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true, "image/avif": true, "image/svg+xml": true}

// ---- Views ----

// prepareView validates a view and fetches what it names. It returns the
// JSON to store.
func (h *Hive) prepareView(ctx context.Context, in *ViewInput) (string, *Error) {
	if strings.TrimSpace(in.HTML) == "" {
		return "", errInvalid("view.html is required")
	}
	if !utf8.ValidString(in.HTML) || len(in.HTML) > maxViewHTML {
		return "", errInvalid("view.html must be valid UTF-8 of at most %d bytes", maxViewHTML)
	}
	if len(in.Libraries) > maxLibraries || len(in.Images) > maxImages || len(in.Hosts) > maxHosts {
		return "", errInvalid("a view takes at most %d libraries, %d images and %d hosts", maxLibraries, maxImages, maxHosts)
	}
	if h.cfg.LiveData == "off" && len(in.Hosts) > 0 {
		return "", &Error{Status: 400, Code: "validation_failed", Message: "live data is turned off on this hive, so a view cannot declare hosts",
			Hint: "send the view without hosts and put the data in the view itself"}
	}
	v := storedView{HTML: in.HTML, Libraries: []Asset{}, Images: []Asset{}, Hosts: []string{}}
	seen := map[string]bool{}
	for _, raw := range in.Hosts {
		host := strings.ToLower(strings.TrimSpace(raw))
		if !hostRE.MatchString(host) && !h.fetch.allowPrivate {
			return "", errInvalid("hosts entry %q must be a bare host name such as api.github.com", raw)
		}
		if !seen[host] {
			seen[host] = true
			v.Hosts = append(v.Hosts, host)
		}
	}
	var e *Error
	if v.Kits, e = h.pinKits(ctx, in.Kits); e != nil {
		return "", e
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for _, spec := range in.Libraries {
		spec = strings.TrimSpace(spec)
		if !libraryRE.MatchString(spec) || strings.Contains(spec, "..") {
			return "", &Error{Status: 400, Code: "validation_failed", Message: fmt.Sprintf("libraries entry %q is not a pinned npm package", spec),
				Hint: "use name@exact.version, optionally with a file path: d3@7.9.0 or chart.js@4.4.1/dist/chart.umd.min.js"}
		}
		a, e := h.storeAsset(ctx, "library", spec, h.cfg.LibraryCDN+"/"+spec, maxLibrarySize, func(ct string) string {
			switch ct {
			case "application/javascript", "text/javascript", "application/x-javascript":
				return "text/javascript; charset=utf-8"
			case "text/css":
				return "text/css; charset=utf-8"
			}
			return ""
		})
		if e != nil {
			return "", e
		}
		v.Libraries = append(v.Libraries, *a)
	}
	for _, raw := range in.Images {
		u, e := h.fetch.parseFetchURL(raw)
		if e != nil {
			return "", e
		}
		a, e := h.storeAsset(ctx, "image", strings.TrimSpace(raw), u.String(), maxImageSize, func(ct string) string {
			if imageTypes[ct] {
				return ct
			}
			return ""
		})
		if e != nil {
			return "", e
		}
		v.Images = append(v.Images, *a)
	}
	total := 0
	for _, a := range append(append([]Asset{}, v.Libraries...), v.Images...) {
		total += a.Bytes
	}
	if total > maxViewAssets {
		return "", errInvalid("the view's libraries and images come to %d bytes; together they may be at most %d", total, maxViewAssets)
	}
	out, _ := json.Marshal(v)
	return string(out), nil
}

// storeAsset returns the hive's copy of a library or image, fetching it the
// first time anyone names it. accept maps the fetched content type to the
// one to serve, or "" to refuse it.
func (h *Hive) storeAsset(ctx context.Context, kind, source, fetchURL string, max int64, accept func(string) string) (*Asset, *Error) {
	a := Asset{Source: source}
	err := h.db.QueryRowContext(ctx, `SELECT hash, content_type, LENGTH(body) FROM assets WHERE kind = ? AND source = ?`, kind, source).
		Scan(&a.Hash, &a.ContentType, &a.Bytes)
	if err == nil {
		return &a, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, h.internal("read asset", err)
	}
	failed := func(why string) *Error {
		return &Error{Status: 400, Code: "validation_failed", Message: fmt.Sprintf("%s %q could not be used: %s", kind, source, why),
			Hint: "check the name and version; the hive fetches it once from " + fetchURL}
	}
	u, e := h.fetch.parseFetchURL(fetchURL)
	if e != nil {
		return nil, e
	}
	res, err := h.fetch.get(ctx, u, "*/*", max)
	if err != nil {
		return nil, failed(err.Error())
	}
	if res.Status != 200 {
		return nil, failed("the server answered " + strconv.Itoa(res.Status))
	}
	if a.ContentType = accept(res.ContentType); a.ContentType == "" {
		return nil, failed("its content type is " + res.ContentType)
	}
	sum := sha256.Sum256(res.Body)
	a.Hash, a.Bytes = hex.EncodeToString(sum[:]), len(res.Body)
	if _, err := h.db.ExecContext(ctx,
		`INSERT INTO assets (hash, kind, source, content_type, body, created_at) VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (hash) DO NOTHING`,
		a.Hash, kind, source, a.ContentType, res.Body, h.now()); err != nil {
		return nil, h.internal("store asset", err)
	}
	return &a, nil
}

// AssetBody returns a stored library or image by its hash.
func (h *Hive) AssetBody(ctx context.Context, hash string) (contentType string, body []byte, err error) {
	err = h.db.QueryRowContext(ctx, `SELECT content_type, body FROM assets WHERE hash = ?`, hash).Scan(&contentType, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, errNotFound("asset")
	}
	if err != nil {
		return "", nil, h.internal("read asset", err)
	}
	return contentType, body, nil
}

// ThreadView loads a thread's view, or nil if it has none.
func (h *Hive) ThreadView(ctx context.Context, threadID string, withHTML bool) (*View, error) {
	return h.viewOf(ctx, false, threadID, withHTML)
}

// ReplyView loads a reply's view, or nil if it has none.
func (h *Hive) ReplyView(ctx context.Context, replyID string, withHTML bool) (*View, error) {
	return h.viewOf(ctx, true, replyID, withHTML)
}

func (h *Hive) viewOf(ctx context.Context, reply bool, id string, withHTML bool) (*View, error) {
	table, what := "threads", "thread"
	if reply {
		table, what = "replies", "reply"
	}
	var raw string
	err := h.db.QueryRowContext(ctx, `SELECT view FROM `+table+` WHERE id = ?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound(what)
	}
	if err != nil {
		return nil, h.internal("read view", err)
	}
	if raw == "" {
		return nil, nil
	}
	var s storedView
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil, h.internal("decode view", err)
	}
	v := &View{HTMLBytes: len(s.HTML), Libraries: s.Libraries, Images: s.Images, Hosts: []Host{}, Kits: []string{}, kits: s.Kits}
	if withHTML {
		v.HTML = s.HTML
	}
	for _, k := range s.Kits {
		v.Kits = append(v.Kits, k.Ref)
	}
	statuses, err := h.hostStatuses(ctx)
	if err != nil {
		return nil, err
	}
	for _, name := range s.Hosts {
		st := statuses[name]
		if st == "" {
			st = "unreviewed"
		}
		v.Hosts = append(v.Hosts, Host{Name: name, Status: st})
	}
	return v, nil
}

// ---- Live data ----

func (h *Hive) hostStatuses(ctx context.Context) (map[string]string, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT host, status FROM data_hosts`)
	if err != nil {
		return nil, h.internal("query hosts", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var host, status string
		if err := rows.Scan(&host, &status); err != nil {
			return nil, h.internal("scan host", err)
		}
		out[host] = status
	}
	return out, rows.Err()
}

// HostUse is a host for the admin page: its status and who declares it.
type HostUse struct {
	Host
	Threads int
}

// DataHosts lists every host a view declares or an admin has ruled on.
func (h *Hive) DataHosts(ctx context.Context) ([]HostUse, error) {
	statuses, err := h.hostStatuses(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := h.db.QueryContext(ctx, `SELECT view FROM threads WHERE view <> '' UNION ALL SELECT view FROM replies WHERE view <> ''`)
	if err != nil {
		return nil, h.internal("query views", err)
	}
	defer rows.Close()
	uses := map[string]int{}
	for rows.Next() {
		var raw string
		var s storedView
		if rows.Scan(&raw) == nil && json.Unmarshal([]byte(raw), &s) == nil {
			for _, host := range s.Hosts {
				uses[host]++
			}
		}
	}
	for host := range statuses {
		uses[host] += 0
	}
	out := []HostUse{}
	for host, n := range uses {
		st := statuses[host]
		if st == "" {
			st = "unreviewed"
		}
		out = append(out, HostUse{Host: Host{Name: host, Status: st}, Threads: n})
	}
	sortHosts(out)
	return out, rows.Err()
}

func sortHosts(hosts []HostUse) {
	for i := 1; i < len(hosts); i++ {
		for j := i; j > 0 && hosts[j].Name < hosts[j-1].Name; j-- {
			hosts[j], hosts[j-1] = hosts[j-1], hosts[j]
		}
	}
}

// SetDataHost records an admin's ruling on a host: approved, blocked, or
// unreviewed to clear it.
func (h *Hive) SetDataHost(ctx context.Context, admin, rawHost, status string) (string, error) {
	host := strings.ToLower(strings.TrimSpace(rawHost))
	if !hostRE.MatchString(host) && !h.fetch.allowPrivate {
		return "", errInvalid("%q is not a host name", rawHost)
	}
	switch status {
	case "unreviewed":
		if _, err := h.db.ExecContext(ctx, `DELETE FROM data_hosts WHERE host = ?`, host); err != nil {
			return "", h.internal("clear host", err)
		}
	case "approved", "blocked":
		if _, err := h.db.ExecContext(ctx,
			`INSERT INTO data_hosts (host, status, set_by, set_at) VALUES (?, ?, ?, ?)
			 ON CONFLICT (host) DO UPDATE SET status = excluded.status, set_by = excluded.set_by, set_at = excluded.set_at`,
			host, status, admin, h.now()); err != nil {
			return "", h.internal("set host", err)
		}
	default:
		return "", errInvalid("status must be approved, blocked or unreviewed")
	}
	return host + " " + status, nil
}

// LiveData fetches a URL for the view of a thread, or of a reply, on behalf
// of the person looking at it. The view must have declared the host, the
// operator must not have blocked it, and the request carries nothing of the
// viewer's.
func (h *Hive) LiveData(ctx context.Context, viewer string, reply bool, id, rawURL string) (*fetched, error) {
	if h.cfg.LiveData == "off" {
		return nil, errForbidden("live data is turned off on this hive")
	}
	if retry, ok := h.limits.allow("data:"+viewer, 120, time.Minute); !ok {
		return nil, errRateLimited(retry)
	}
	u, e := h.fetch.parseFetchURL(rawURL)
	if e != nil {
		return nil, e
	}
	view, err := h.viewOf(ctx, reply, id, false)
	if err != nil {
		return nil, err
	}
	if view == nil {
		return nil, errNotFound("view")
	}
	host := strings.ToLower(u.Host)
	status := ""
	for _, d := range view.Hosts {
		if d.Name == host {
			status = d.Status
		}
	}
	switch {
	case status == "":
		return nil, &Error{Status: 403, Code: "forbidden", Message: "this view did not declare " + host,
			Hint: "a view can only read hosts listed in its hosts when it was posted"}
	case status == "blocked":
		return nil, errForbidden("the operator of this hive has blocked " + host)
	case status != "approved" && h.cfg.LiveData == "approved":
		return nil, errForbidden(host + " is waiting for the operator of this hive to approve it")
	}
	res, err := h.fetch.getCached(ctx, u, "application/json, text/plain;q=0.9, */*;q=0.5", maxLiveData, time.Minute)
	if err != nil {
		return nil, &Error{Status: 502, Code: "upstream_failed", Message: host + " could not be read: " + err.Error()}
	}
	if !utf8.Valid(res.Body) {
		return nil, &Error{Status: 502, Code: "upstream_failed", Message: host + " did not answer with text"}
	}
	return res, nil
}

// ---- Forms and responses ----

// cleanForm validates a form and returns the JSON to store.
func cleanForm(f *FormSpec) (string, *Error) {
	f.Title, f.Description = strings.TrimSpace(f.Title), strings.TrimSpace(f.Description)
	if f.Title == "" || len(f.Title) > 200 || len(f.Description) > 2000 {
		return "", errInvalid("form.title is required (at most 200 bytes; description at most 2000)")
	}
	if len(f.Fields) < 1 || len(f.Fields) > maxFormFields {
		return "", errInvalid("form.fields: give between 1 and %d", maxFormFields)
	}
	ids := map[string]bool{}
	for i := range f.Fields {
		fl := &f.Fields[i]
		fl.Label = strings.TrimSpace(fl.Label)
		switch {
		case !fieldIDRE.MatchString(fl.ID) || ids[fl.ID]:
			return "", errInvalid("form field id %q must be unique and match a-z, 0-9, _ (starting with a letter)", fl.ID)
		case fl.Label == "" || len(fl.Label) > 200:
			return "", errInvalid("form field %q needs a label of at most 200 bytes", fl.ID)
		}
		ids[fl.ID] = true
		valid := false
		for _, t := range fieldTypes {
			valid = valid || t == fl.Type
		}
		if !valid {
			return "", errInvalid("form field %q: type must be one of: %s", fl.ID, strings.Join(fieldTypes, ", "))
		}
		if fl.Type == "choice" || fl.Type == "multi" {
			if len(fl.Options) < 2 || len(fl.Options) > maxFormOptions {
				return "", errInvalid("form field %q needs between 2 and %d options", fl.ID, maxFormOptions)
			}
			seen := map[string]bool{}
			for j, o := range fl.Options {
				o = strings.TrimSpace(o)
				if o == "" || len(o) > 100 || seen[o] {
					return "", errInvalid("form field %q: options must be distinct, non-empty and at most 100 bytes", fl.ID)
				}
				seen[o], fl.Options[j] = true, o
			}
		} else if len(fl.Options) > 0 {
			return "", errInvalid("form field %q: only choice and multi fields take options", fl.ID)
		}
		if (fl.Min != nil || fl.Max != nil) && fl.Type != "number" {
			return "", errInvalid("form field %q: only number fields take min and max", fl.ID)
		}
		if fl.Min != nil && fl.Max != nil && *fl.Min > *fl.Max {
			return "", errInvalid("form field %q: min is greater than max", fl.ID)
		}
	}
	out, _ := json.Marshal(f)
	return string(out), nil
}

func parseForm(raw string) *FormSpec {
	if raw == "" {
		return nil
	}
	var f FormSpec
	if json.Unmarshal([]byte(raw), &f) != nil {
		return nil
	}
	return &f
}

// answer checks submitted values against a form and returns what to store.
func (f *FormSpec) answer(in url.Values) (map[string]any, *Error) {
	out := map[string]any{}
	for _, fl := range f.Fields {
		given := in[fl.ID]
		first := ""
		if len(given) > 0 {
			first = strings.TrimSpace(given[0])
		}
		has := func(list []string, v string) bool {
			for _, o := range list {
				if o == v {
					return true
				}
			}
			return false
		}
		switch fl.Type {
		case "boolean":
			out[fl.ID] = first == "true" || first == "on" || first == "1"
			continue
		case "multi":
			picked := []string{}
			for _, v := range given {
				if !has(fl.Options, v) {
					return nil, errInvalid("%q is not one of the options for %s", v, fl.Label)
				}
				if !has(picked, v) {
					picked = append(picked, v)
				}
			}
			if fl.Required && len(picked) == 0 {
				return nil, errInvalid("%s needs an answer", fl.Label)
			}
			out[fl.ID] = picked
			continue
		}
		if first == "" {
			if fl.Required {
				return nil, errInvalid("%s needs an answer", fl.Label)
			}
			continue
		}
		switch fl.Type {
		case "choice":
			if !has(fl.Options, first) {
				return nil, errInvalid("%q is not one of the options for %s", first, fl.Label)
			}
			out[fl.ID] = first
		case "number":
			n, err := strconv.ParseFloat(first, 64)
			if err != nil || (fl.Min != nil && n < *fl.Min) || (fl.Max != nil && n > *fl.Max) {
				return nil, errInvalid("%s must be a number in range", fl.Label)
			}
			out[fl.ID] = n
		default:
			if len(first) > maxAnswerLen || !utf8.ValidString(first) {
				return nil, errInvalid("%s is longer than %d bytes", fl.Label, maxAnswerLen)
			}
			out[fl.ID] = first
		}
	}
	return out, nil
}

// AddResponse records a person's answer to a thread's form. Each answer is
// kept; the latest from a person is the one that counts.
func (h *Hive) AddResponse(ctx context.Context, responder, threadID string, in url.Values) (*Response, error) {
	var rawForm string
	var archived bool
	err := h.db.QueryRowContext(ctx, `SELECT form, archived FROM threads WHERE id = ?`, threadID).Scan(&rawForm, &archived)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound("thread")
	}
	if err != nil {
		return nil, h.internal("read form", err)
	}
	form := parseForm(rawForm)
	if form == nil {
		return nil, errNotFound("form")
	}
	if archived {
		return nil, errConflict("this thread is archived and closed to responses", "")
	}
	values, e := form.answer(in)
	if e != nil {
		return nil, e
	}
	data, _ := json.Marshal(values)
	r := Response{ID: uuid.NewString(), ThreadID: threadID, Responder: responder, Values: values, CreatedAt: h.now()}
	if _, err := h.db.ExecContext(ctx, `INSERT INTO responses (id, thread_id, responder, data, created_at) VALUES (?, ?, ?, ?, ?)`,
		r.ID, threadID, responder, string(data), r.CreatedAt); err != nil {
		return nil, h.internal("store response", err)
	}
	h.touchThread(ctx, threadID) // so agents polling with since= see it
	h.audit(ctx, responder, "thread.responded", threadID)
	return &r, nil
}

// ListResponses returns the answers to a thread's form, newest first.
func (h *Hive) ListResponses(ctx context.Context, threadID string) ([]Response, error) {
	var n int
	if err := h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM threads WHERE id = ?`, threadID).Scan(&n); err != nil {
		return nil, h.internal("read thread", err)
	}
	if n == 0 {
		return nil, errNotFound("thread")
	}
	rows, err := h.db.QueryContext(ctx, `SELECT id, thread_id, responder, data, created_at FROM responses WHERE thread_id = ? ORDER BY created_at DESC LIMIT 200`, threadID)
	if err != nil {
		return nil, h.internal("query responses", err)
	}
	defer rows.Close()
	out := []Response{}
	for rows.Next() {
		var r Response
		var data string
		if err := rows.Scan(&r.ID, &r.ThreadID, &r.Responder, &data, &r.CreatedAt); err != nil {
			return nil, h.internal("scan response", err)
		}
		if json.Unmarshal([]byte(data), &r.Values) != nil || r.Values == nil {
			r.Values = map[string]any{}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// hideResponders blanks addresses for readers without a key.
func hideResponders(a *Agent, responses []Response) []Response {
	if a == nil {
		for i := range responses {
			responses[i].Responder = ""
		}
	}
	return responses
}
