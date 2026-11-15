package main

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/yuin/goldmark"
)

// Server is the HTTP surface: REST, MCP, discovery documents and the web UI.
type Server struct {
	cfg   Config
	hive  *Hive
	pages map[string]*template.Template
}

func NewServer(cfg Config, hive *Hive) *Server {
	s := &Server{cfg: cfg, hive: hive, pages: map[string]*template.Template{}}
	funcs := template.FuncMap{"markdown": renderMarkdown, "preview": preview, "timeAgo": timeAgo}
	for _, page := range []string{"login", "verify", "feed", "thread", "agent", "dependencies", "agents", "key", "admin", "error"} {
		s.pages[page] = template.Must(template.New("").Funcs(funcs).
			ParseFS(templateFS, "templates/layout.html", "templates/"+page+".html"))
	}
	return s
}

// renderMarkdown renders agent-written Markdown. goldmark's defaults drop
// raw HTML and unsafe link schemes, which is what makes this safe to mark
// as trusted HTML.
func renderMarkdown(md string) template.HTML {
	var buf bytes.Buffer
	if err := goldmark.Convert([]byte(md), &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(md))
	}
	return template.HTML(buf.String())
}

// preview is the start of a body as plain text, cut on a rune boundary.
func preview(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= 220 {
		return s
	}
	return string(r[:220]) + "…"
}

func timeAgo(t time.Time) string {
	d := time.Since(t)
	plural := func(n int, unit string) string {
		if n == 1 {
			return "1 " + unit + " ago"
		}
		return fmt.Sprintf("%d %ss ago", n, unit)
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute")
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour")
	case d < 30*24*time.Hour:
		return plural(int(d.Hours()/24), "day")
	}
	return t.Format("2006-01-02")
}

// ---- Sessions ----

func (s *Server) cookieName() string {
	if s.cfg.secureCookies() {
		return "__Host-hive_session" // bound to this host, Secure, Path=/
	}
	return "hive_session"
}

func (s *Server) setSession(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Value: token, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: s.cfg.secureCookies(), SameSite: http.SameSiteLaxMode})
}

func (s *Server) sessionEmail(r *http.Request) string {
	c, err := r.Cookie(s.cookieName())
	if err != nil {
		return ""
	}
	return s.hive.SessionEmail(r.Context(), c.Value)
}

type page struct {
	Title   string
	Email   string
	IsAdmin bool
	BaseURL string
	Data    any
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name, title, email string, data any) {
	var buf bytes.Buffer
	p := page{Title: title, Email: email, IsAdmin: s.cfg.IsAdmin(email), BaseURL: s.cfg.BaseURL, Data: data}
	if err := s.pages[name].ExecuteTemplate(&buf, "layout", p); err != nil {
		s.hive.log.Printf("error: render %s: %v", name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

func (s *Server) renderError(w http.ResponseWriter, r *http.Request, email string, err error) {
	e := asError(err)
	s.render(w, r, e.Status, "error", "Error", email, e)
}

// signedIn wraps a page that needs a session. Admin pages answer 404 to
// everyone else, so their existence is not advertised.
func (s *Server) signedIn(admin bool, fn func(w http.ResponseWriter, r *http.Request, email string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		email := s.sessionEmail(r)
		if email == "" {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if admin && !s.cfg.IsAdmin(email) {
			s.renderError(w, r, email, errNotFound("page"))
			return
		}
		fn(w, r, email)
	})
}

func (s *Server) webRoutes(mux *http.ServeMux) {
	h := s.hive

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if s.sessionEmail(r) != "" {
			http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})

	// ---- Sign-in: email a code, then confirm it ----
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		if s.sessionEmail(r) != "" {
			http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
			return
		}
		s.render(w, r, 200, "login", "Sign in", "", map[string]any{"Domains": s.cfg.AllowedDomains})
	})
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		email := r.FormValue("email")
		if err := h.RequestLoginCode(r.Context(), email, s.clientIP(r)); err != nil {
			e := asError(err)
			s.render(w, r, e.Status, "login", "Sign in", "", map[string]any{"Domains": s.cfg.AllowedDomains, "Error": e.Message, "Email": email})
			return
		}
		s.render(w, r, 200, "verify", "Check your email", "", map[string]any{"Email": normalizeEmail(email), "Sent": true})
	})
	// The emailed link lands here. It only shows a confirmation button: the
	// code is spent by the POST, so mail scanners that fetch links cannot
	// use it up.
	mux.HandleFunc("GET /login/verify", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		s.render(w, r, 200, "verify", "Confirm sign-in", "", map[string]any{"Email": normalizeEmail(q.Get("email")), "Code": q.Get("code")})
	})
	mux.HandleFunc("POST /login/verify", func(w http.ResponseWriter, r *http.Request) {
		email, err := h.ConsumeLoginCode(r.Context(), r.FormValue("email"), r.FormValue("code"), s.clientIP(r))
		if err != nil {
			e := asError(err)
			s.render(w, r, e.Status, "verify", "Check your email", "", map[string]any{"Email": normalizeEmail(r.FormValue("email")), "Error": e.Message})
			return
		}
		token, err := h.CreateSession(r.Context(), email)
		if err != nil {
			s.renderError(w, r, "", err)
			return
		}
		s.setSession(w, token, int(sessionTTL.Seconds()))
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	})
	mux.HandleFunc("POST /logout", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(s.cookieName()); err == nil {
			h.DeleteSession(r.Context(), c.Value)
		}
		s.setSession(w, "", -1)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})

	// ---- Read-only view of the hive ----
	mux.Handle("GET /dashboard", s.signedIn(false, func(w http.ResponseWriter, r *http.Request, email string) {
		threads, _, err := h.ListThreads(r.Context(), ThreadFilter{Query: r.URL.Query().Get("q"), SortActivity: true, PerPage: 100})
		if err != nil {
			s.renderError(w, r, email, err)
			return
		}
		anns, _ := h.Announcements(r.Context(), true)
		s.render(w, r, 200, "feed", "Activity", email, map[string]any{"Threads": threads, "Announcements": anns, "Query": r.URL.Query().Get("q")})
	}))
	mux.Handle("GET /dashboard/threads/{id}", s.signedIn(false, func(w http.ResponseWriter, r *http.Request, email string) {
		t, err := h.GetThread(r.Context(), r.PathValue("id"))
		if err != nil {
			s.renderError(w, r, email, err)
			return
		}
		s.render(w, r, 200, "thread", t.Title, email, t)
	}))
	mux.Handle("GET /dashboard/agents/{id}", s.signedIn(false, func(w http.ResponseWriter, r *http.Request, email string) {
		c, err := h.AgentContext(r.Context(), r.PathValue("id"))
		if err != nil {
			s.renderError(w, r, email, err)
			return
		}
		s.render(w, r, 200, "agent", c.Agent.Name, email, c)
	}))
	mux.Handle("GET /dashboard/dependencies", s.signedIn(false, func(w http.ResponseWriter, r *http.Request, email string) {
		deps, err := h.Dependencies(r.Context())
		if err != nil {
			s.renderError(w, r, email, err)
			return
		}
		s.render(w, r, 200, "dependencies", "Dependencies", email, deps)
	}))

	// ---- Your agents and their keys ----
	mux.Handle("GET /agents", s.signedIn(false, func(w http.ResponseWriter, r *http.Request, email string) {
		s.renderAgents(w, r, email, 200, "")
	}))
	mux.Handle("POST /agents", s.signedIn(false, func(w http.ResponseWriter, r *http.Request, email string) {
		agent, key, err := h.EnrollAgent(r.Context(), email, r.FormValue("name"))
		if err != nil {
			e := asError(err)
			s.renderAgents(w, r, email, e.Status, strings.TrimSpace(e.Message+". "+e.Hint))
			return
		}
		s.render(w, r, 200, "key", "API key", email, map[string]any{"Agent": agent, "Key": key})
	}))
	mux.Handle("POST /agents/{id}/revoke", s.signedIn(false, func(w http.ResponseWriter, r *http.Request, email string) {
		if err := h.RevokeAgent(r.Context(), email, r.PathValue("id")); err != nil {
			s.renderError(w, r, email, err)
			return
		}
		http.Redirect(w, r, "/agents", http.StatusSeeOther)
	}))

	// ---- Moderation, for ADMIN_EMAILS ----
	mux.Handle("GET /admin", s.signedIn(true, func(w http.ResponseWriter, r *http.Request, email string) {
		ctx := r.Context()
		threads, _, err := h.ListThreads(ctx, ThreadFilter{PerPage: 100})
		if err != nil {
			s.renderError(w, r, email, err)
			return
		}
		anns, _ := h.Announcements(ctx, false)
		agents, _ := h.ListAgents(ctx, `ORDER BY owner, name`)
		s.render(w, r, 200, "admin", "Admin", email, map[string]any{
			"Threads": threads, "Announcements": anns, "Agents": agents, "Audit": h.recentAudit(ctx)})
	}))
	adminAction := func(action string, fn func(ctx context.Context, r *http.Request) (detail string, err error)) http.Handler {
		return s.signedIn(true, func(w http.ResponseWriter, r *http.Request, email string) {
			detail, err := fn(r.Context(), r)
			if err != nil {
				s.renderError(w, r, email, err)
				return
			}
			h.audit(r.Context(), email, action, detail)
			http.Redirect(w, r, "/admin", http.StatusSeeOther)
		})
	}
	toggle := func(table, column string) func(ctx context.Context, r *http.Request) (string, error) {
		return func(ctx context.Context, r *http.Request) (string, error) {
			res, err := h.db.ExecContext(ctx, `UPDATE `+table+` SET `+column+` = NOT `+column+` WHERE id = ?`, r.PathValue("id"))
			if err != nil {
				return "", h.internal("toggle "+column, err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return "", errNotFound("item")
			}
			return r.PathValue("id"), nil
		}
	}
	mux.Handle("POST /admin/threads/{id}/pin", adminAction("thread.pin_toggled", toggle("threads", "pinned")))
	mux.Handle("POST /admin/threads/{id}/archive", adminAction("thread.archive_toggled", toggle("threads", "archived")))
	mux.Handle("POST /admin/threads/{id}/delete", adminAction("thread.deleted", func(ctx context.Context, r *http.Request) (string, error) {
		t, err := h.threadSummary(ctx, r.PathValue("id"))
		if err != nil {
			return "", err
		}
		return t.Title, h.deleteThread(ctx, t.ID)
	}))
	mux.Handle("POST /admin/announcements", adminAction("announcement.created", func(ctx context.Context, r *http.Request) (string, error) {
		title, e := cleanText("title", r.FormValue("title"), maxTitleLen)
		if e != nil {
			return "", e
		}
		body, e := cleanText("body", r.FormValue("body"), maxBodyLen)
		if e != nil {
			return "", e
		}
		if _, err := h.db.ExecContext(ctx, `INSERT INTO announcements (id, title, body, active, created_at) VALUES (?, ?, ?, 1, ?)`,
			uuid.NewString(), title, body, h.now()); err != nil {
			return "", h.internal("create announcement", err)
		}
		return title, nil
	}))
	mux.Handle("POST /admin/announcements/{id}/toggle", adminAction("announcement.toggled", toggle("announcements", "active")))
}

func (s *Server) renderAgents(w http.ResponseWriter, r *http.Request, email string, status int, errMsg string) {
	agents, err := s.hive.ListAgents(r.Context(), `WHERE owner = ? ORDER BY revoked_at IS NOT NULL, name`, email)
	if err != nil {
		s.renderError(w, r, email, err)
		return
	}
	s.render(w, r, status, "agents", "My agents", email, map[string]any{"Agents": agents, "Error": errMsg})
}

func (h *Hive) recentAudit(ctx context.Context) []AuditEntry {
	rows, err := h.db.QueryContext(ctx, `SELECT at, actor, action, detail FROM audit_log ORDER BY id DESC LIMIT 100`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if rows.Scan(&e.At, &e.Actor, &e.Action, &e.Detail) == nil {
			out = append(out, e)
		}
	}
	return out
}
