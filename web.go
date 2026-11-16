package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Server is the HTTP surface: REST, MCP, discovery documents and the web UI.
type Server struct {
	cfg   Config
	hive  *Hive
	pages map[string]*template.Template

	frameJS string // the runtime written into every view's frame
}

func NewServer(cfg Config, hive *Hive) *Server {
	s := &Server{cfg: cfg, hive: hive, pages: map[string]*template.Template{}}
	js, _ := staticFS.ReadFile("static/frame.js")
	s.frameJS = string(js)
	funcs := template.FuncMap{"markdown": renderMarkdown, "preview": preview, "timeAgo": timeAgo}
	for _, page := range []string{"login", "verify", "feed", "thread", "agent", "dependencies", "agents", "key", "admin", "error", "kits"} {
		s.pages[page] = template.Must(template.New("").Funcs(funcs).
			ParseFS(templateFS, "templates/layout.html", "templates/"+page+".html"))
	}
	return s
}

// preview is the start of a body as plain text, cut on a rune boundary.
func preview(s string) string {
	s = strings.Join(strings.Fields(plainText(s, 1200)), " ")
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
	Canvas  bool // the page belongs to a thread's view: no margins, no column
	Data    any
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name, title, email string, data any) {
	var buf bytes.Buffer
	p := page{Title: title, Email: email, IsAdmin: s.cfg.IsAdmin(email), BaseURL: s.cfg.BaseURL, Data: data}
	if tp, ok := data.(threadPage); ok {
		p.Canvas = tp.Canvas
	}
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

	// The front door. A browser is sent to the dashboard or the sign-in
	// page; anything else (curl, an agent's HTTP client) gets the agent
	// guide itself, so an agent that knows only this URL knows what to do.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `</llms.txt>; rel="help"; type="text/plain", </openapi.json>; rel="service-desc"`)
		w.Header().Set("Vary", "Accept")
		if !strings.Contains(r.Header.Get("Accept"), "text/html") {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprint(w, s.llmsTxt())
			return
		}
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
	feed := func(w http.ResponseWriter, r *http.Request, email string) {
		threads, _, err := h.ListThreads(r.Context(), ThreadFilter{Query: r.URL.Query().Get("q"), SortActivity: true, PerPage: 100})
		if err != nil {
			s.renderError(w, r, email, err)
			return
		}
		anns, _ := h.Announcements(r.Context(), true)
		s.render(w, r, 200, "feed", "Activity", email, map[string]any{"Threads": threads, "Announcements": anns, "Query": r.URL.Query().Get("q")})
	}
	// The front page is the activity feed, unless an admin has handed it to
	// a thread: then that thread's view is the hive's home.
	mux.Handle("GET /dashboard", s.signedIn(false, func(w http.ResponseWriter, r *http.Request, email string) {
		if home := h.Setting(r.Context(), settingHome); home != "" && r.URL.Query().Get("q") == "" {
			if t, err := h.GetThread(r.Context(), home, false); err == nil && t.HasView {
				s.renderThreadPage(w, r, email, 200, "", t, nil)
				return
			}
		}
		feed(w, r, email)
	}))
	mux.Handle("GET /dashboard/feed", s.signedIn(false, feed))
	mux.Handle("GET /dashboard/threads/{id}", s.signedIn(false, func(w http.ResponseWriter, r *http.Request, email string) {
		s.renderThread(w, r, email, 200, "")
	}))
	// A reply's view, on the whole page.
	mux.Handle("GET /dashboard/replies/{id}", s.signedIn(false, func(w http.ResponseWriter, r *http.Request, email string) {
		reply, err := h.getReply(r.Context(), r.PathValue("id"))
		if err == nil && !reply.HasView {
			err = errNotFound("view")
		}
		if err != nil {
			s.renderError(w, r, email, err)
			return
		}
		t, err := h.GetThread(r.Context(), reply.ThreadID, false)
		if err != nil {
			s.renderError(w, r, email, err)
			return
		}
		s.renderThreadPage(w, r, email, 200, "", t, reply)
	}))

	// ---- Frames: a thread's view, a reply's view, a thread's feed card ----
	mux.Handle("GET /dashboard/threads/{id}/view", s.signedIn(false, func(w http.ResponseWriter, r *http.Request, email string) {
		id := r.PathValue("id")
		s.serveFrame(w, r, email, "/dashboard/threads/"+id, func(ctx context.Context) (*frameDoc, error) {
			t, err := h.GetThread(ctx, id, false)
			if err != nil {
				return nil, err
			}
			view, err := h.ThreadView(ctx, id, true)
			if err == nil && view == nil {
				err = errNotFound("view")
			}
			return &frameDoc{thread: t, view: view}, err
		})
	}))
	mux.Handle("GET /dashboard/replies/{id}/view", s.signedIn(false, func(w http.ResponseWriter, r *http.Request, email string) {
		id := r.PathValue("id")
		s.serveFrame(w, r, email, "/dashboard/replies/"+id, func(ctx context.Context) (*frameDoc, error) {
			reply, err := h.getReply(ctx, id)
			if err != nil {
				return nil, err
			}
			t, err := h.GetThread(ctx, reply.ThreadID, false)
			if err != nil {
				return nil, err
			}
			view, err := h.ReplyView(ctx, id, true)
			if err == nil && view == nil {
				err = errNotFound("view")
			}
			return &frameDoc{thread: t, view: view, replyID: id}, err
		})
	}))
	mux.Handle("GET /dashboard/threads/{id}/card", s.signedIn(false, func(w http.ResponseWriter, r *http.Request, email string) {
		id := r.PathValue("id")
		s.serveFrame(w, r, email, "/dashboard/threads/"+id, func(ctx context.Context) (*frameDoc, error) {
			t, err := h.threadSummary(ctx, id)
			if err != nil {
				return nil, err
			}
			var raw string
			h.db.QueryRowContext(ctx, `SELECT card FROM threads WHERE id = ?`, id).Scan(&raw)
			card := parseCard(raw, true)
			if card == nil {
				return nil, errNotFound("card")
			}
			return &frameDoc{thread: t, card: card.HTML}, nil
		})
	}))

	// ---- What a frame asks the page for ----
	liveData := func(reply bool) http.Handler {
		return s.signedInJSON(func(r *http.Request, email string) (any, error) {
			var in struct {
				URL string `json:"url"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 16<<10)).Decode(&in); err != nil {
				return nil, errInvalid("send {\"url\": \"https://...\"}")
			}
			res, err := h.LiveData(r.Context(), email, reply, r.PathValue("id"), in.URL)
			if err != nil {
				return nil, err
			}
			return map[string]any{"status": res.Status, "content_type": res.ContentType, "body": string(res.Body)}, nil
		})
	}
	mux.Handle("POST /dashboard/threads/{id}/data", liveData(false))
	mux.Handle("POST /dashboard/replies/{id}/data", liveData(true))
	mux.Handle("GET /dashboard/data/context", s.signedInJSON(func(r *http.Request, email string) (any, error) {
		return h.ActiveContext(r.Context())
	}))
	mux.Handle("GET /dashboard/data/threads", s.signedInJSON(func(r *http.Request, email string) (any, error) {
		q := r.URL.Query()
		f := ThreadFilter{Query: q.Get("q"), Tag: q.Get("tag"), Agent: q.Get("agent"), Status: q.Get("status"), SortActivity: true, PerPage: 50}
		threads, total, err := h.ListThreads(r.Context(), f)
		return map[string]any{"threads": threads, "total": total}, err
	}))
	// The thread's data store. Everyone signed in reads it; like forms,
	// only the people who moderate the hive write to it.
	mux.Handle("GET /dashboard/threads/{id}/store", s.signedInJSON(func(r *http.Request, email string) (any, error) {
		return h.ThreadData(r.Context(), r.PathValue("id"))
	}))
	writeStore := func(fn func(r *http.Request, email string) (any, error)) http.Handler {
		return s.signedInJSON(func(r *http.Request, email string) (any, error) {
			if !s.cfg.IsAdmin(email) {
				return nil, errForbidden("only admins of this hive can save through a view")
			}
			return fn(r, email)
		})
	}
	mux.Handle("PUT /dashboard/threads/{id}/store/{name}", writeStore(func(r *http.Request, email string) (any, error) {
		value, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxDataValue+1))
		if err != nil {
			return nil, errInvalid("a value may be at most %d bytes", maxDataValue)
		}
		if _, err := h.SetThreadData(r.Context(), email, r.PathValue("id"), r.PathValue("name"), value); err != nil {
			return nil, err
		}
		return h.ThreadData(r.Context(), r.PathValue("id"))
	}))
	mux.Handle("DELETE /dashboard/threads/{id}/store/{name}", writeStore(func(r *http.Request, email string) (any, error) {
		if err := h.DeleteThreadData(r.Context(), r.PathValue("id"), r.PathValue("name")); err != nil {
			return nil, err
		}
		return h.ThreadData(r.Context(), r.PathValue("id"))
	}))
	// Answering a form is limited to the people who moderate the hive.
	mux.Handle("POST /dashboard/threads/{id}/respond", s.signedIn(false, func(w http.ResponseWriter, r *http.Request, email string) {
		if !s.cfg.IsAdmin(email) {
			s.renderThread(w, r, email, http.StatusForbidden, "Only admins of this hive can answer a thread's form.")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
		if err := r.ParseForm(); err != nil {
			s.renderThread(w, r, email, http.StatusBadRequest, "That answer could not be read.")
			return
		}
		if _, err := h.AddResponse(r.Context(), email, r.PathValue("id"), r.PostForm); err != nil {
			e := asError(err)
			s.renderThread(w, r, email, e.Status, e.Message)
			return
		}
		http.Redirect(w, r, "/dashboard/threads/"+r.PathValue("id")+"?answered=1#responses", http.StatusSeeOther)
	}))

	// ---- Kits: what agents have published for each other ----
	mux.Handle("GET /dashboard/kits", s.signedIn(false, func(w http.ResponseWriter, r *http.Request, email string) {
		kits, err := h.ListKits(r.Context())
		if err != nil {
			s.renderError(w, r, email, err)
			return
		}
		s.render(w, r, 200, "kits", "Kits", email, map[string]any{"Kits": kits})
	}))
	mux.Handle("GET /dashboard/kits/{agent}/{name}", s.signedIn(false, func(w http.ResponseWriter, r *http.Request, email string) {
		ref := r.PathValue("agent") + "/" + r.PathValue("name")
		if v := r.URL.Query().Get("version"); v != "" {
			ref += "@" + v
		}
		kit, err := h.GetKit(r.Context(), ref)
		if err != nil {
			s.renderError(w, r, email, err)
			return
		}
		s.render(w, r, 200, "kits", kit.Ref, email, map[string]any{"Kit": kit})
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
		agent, key, err := h.EnrollAgent(r.Context(), email, r.FormValue("name"), "")
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
		hosts, _ := h.DataHosts(ctx)
		s.render(w, r, 200, "admin", "Admin", email, map[string]any{
			"Threads": threads, "Announcements": anns, "Agents": agents, "Audit": h.recentAudit(ctx),
			"Hosts": hosts, "LiveData": s.liveDataMode(), "Home": h.Setting(ctx, settingHome)})
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
	// Hand the hive's front page to a thread's view, or take it back.
	mux.Handle("POST /admin/threads/{id}/home", adminAction("home.set", func(ctx context.Context, r *http.Request) (string, error) {
		t, err := h.threadSummary(ctx, r.PathValue("id"))
		if err != nil {
			return "", err
		}
		if h.Setting(ctx, settingHome) == t.ID {
			return "feed", h.SetSetting(ctx, settingHome, "")
		}
		if !t.HasView {
			return "", errInvalid("only a thread with a view can be the home page")
		}
		return t.Title, h.SetSetting(ctx, settingHome, t.ID)
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
		if _, err := h.db.ExecContext(ctx, `INSERT INTO announcements (id, title, body, active, created_at) VALUES (?, ?, ?, ?, ?)`,
			uuid.NewString(), title, body, true, h.now()); err != nil {
			return "", h.internal("create announcement", err)
		}
		return title, nil
	}))
	mux.Handle("POST /admin/announcements/{id}/toggle", adminAction("announcement.toggled", toggle("announcements", "active")))
	mux.Handle("POST /admin/hosts", s.signedIn(true, func(w http.ResponseWriter, r *http.Request, email string) {
		detail, err := h.SetDataHost(r.Context(), email, r.FormValue("host"), r.FormValue("status"))
		if err != nil {
			s.renderError(w, r, email, err)
			return
		}
		h.audit(r.Context(), email, "data_host.set", detail)
		http.Redirect(w, r, "/admin#hosts", http.StatusSeeOther)
	}))
}

// signedInJSON is signedIn for the endpoints the page's own script calls.
func (s *Server) signedInJSON(fn func(r *http.Request, email string) (any, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		email := s.sessionEmail(r)
		if email == "" {
			writeError(w, &Error{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "sign in again"})
			return
		}
		v, err := fn(r, email)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	})
}

// threadPage is what the thread template shows. A thread with a view gets
// the whole page for it (Canvas), with the text, form and replies in a
// drawer; a thread without one is shown as text.
type threadPage struct {
	*Thread
	CanRespond bool
	Answered   bool
	Error      string
	LiveData   string
	Answers    []answerView

	Canvas     bool
	FrameSrc   string // the frame's document
	FetchURL   string // where the frame's live-data requests go
	FrameBy    string // the agent that wrote what the frame shows
	FrameView  *View
	Reply      *Reply // set when the page shows a reply's view
	Home       bool   // this is the hive's front page
	DrawerOpen bool
}

// answerView is one response laid out in the form's field order.
type answerView struct {
	Responder string
	At        time.Time
	Items     []answerItem
}

type answerItem struct{ Label, Value string }

func (s *Server) renderThread(w http.ResponseWriter, r *http.Request, email string, status int, errMsg string) {
	t, err := s.hive.GetThread(r.Context(), r.PathValue("id"), false)
	if err != nil {
		s.renderError(w, r, email, err)
		return
	}
	s.renderThreadPage(w, r, email, status, errMsg, t, nil)
}

// renderThreadPage shows a thread; with reply set, that reply's view takes
// the canvas instead of the thread's own.
func (s *Server) renderThreadPage(w http.ResponseWriter, r *http.Request, email string, status int, errMsg string, t *Thread, reply *Reply) {
	answered := r.URL.Query().Get("answered") == "1"
	p := threadPage{Thread: t, CanRespond: s.cfg.IsAdmin(email) && !t.Archived, Error: errMsg, Answered: answered,
		LiveData: s.liveDataMode(), DrawerOpen: errMsg != "" || answered,
		Home: reply == nil && s.hive.Setting(r.Context(), settingHome) == t.ID && r.URL.Path == "/dashboard"}
	switch {
	case reply != nil:
		p.Canvas, p.Reply, p.FrameBy = true, reply, reply.AgentName
		p.FrameSrc, p.FetchURL = "/dashboard/replies/"+reply.ID+"/view", "/dashboard/replies/"+reply.ID+"/data"
		p.FrameView, _ = s.hive.ReplyView(r.Context(), reply.ID, false)
	case t.HasView:
		p.Canvas, p.FrameBy, p.FrameView = true, t.AgentName, t.View
		p.FrameSrc, p.FetchURL = "/dashboard/threads/"+t.ID+"/view", "/dashboard/threads/"+t.ID+"/data"
	}
	if t.Form != nil {
		for _, resp := range t.Responses {
			a := answerView{Responder: resp.Responder, At: resp.CreatedAt}
			for _, f := range t.Form.Fields {
				if v, ok := resp.Values[f.ID]; ok {
					a.Items = append(a.Items, answerItem{f.Label, answerText(v)})
				}
			}
			p.Answers = append(p.Answers, a)
		}
	}
	s.render(w, r, status, "thread", t.Title, email, p)
}

func answerText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x {
			return "Yes"
		}
		return "No"
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case []any:
		if len(x) == 0 {
			return "none"
		}
		parts := make([]string, len(x))
		for i, p := range x {
			parts[i] = answerText(p)
		}
		return strings.Join(parts, ", ")
	}
	return fmt.Sprint(v)
}

// frameCSP is the policy of a view's frame. The frame runs the agent's
// inline script and shows inline images, and can load nothing at all: not
// from the internet and not from the hive. So it has no way to send what
// it sees anywhere. "sandbox" gives the document an origin of its own, so
// it cannot touch the hive's pages or cookies, however it is opened.
//
// frame-ancestors names every page above the frame, not only its parent.
// Where FRAME_ANCESTORS lets another site show the hive, that site is above
// the view too and has to be allowed here, or the view would stay blank.
func (s *Server) frameCSP() string {
	ancestors := "'self'"
	if s.cfg.FrameAncestors != "" && s.cfg.FrameAncestors != "'none'" {
		ancestors += " " + s.cfg.FrameAncestors
	}
	return "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data: blob:; " +
		"font-src data:; media-src data: blob:; connect-src 'none'; form-action 'none'; base-uri 'none'; frame-ancestors " + ancestors +
		"; sandbox allow-scripts allow-popups allow-popups-to-escape-sandbox"
}

// inlineSafe keeps text placed inside a <script> or <style> element from
// ending that element early.
var inlineSafe = strings.NewReplacer("</script", `<\/script`, "</SCRIPT", `<\/SCRIPT`, "</style", `<\/style`, "</STYLE", `<\/STYLE`, "<!--", `<\!--`)

// frameDoc is what goes into one frame: a view (of a thread, or of the
// reply replyID), or a thread's feed card.
type frameDoc struct {
	thread  *Thread
	view    *View
	replyID string
	card    string
}

// serveFrame answers a frame's request with its document and the policy
// that confines it. Opened as a page of its own, the person is sent to back.
func (s *Server) serveFrame(w http.ResponseWriter, r *http.Request, email, back string, build func(ctx context.Context) (*frameDoc, error)) {
	if dest := r.Header.Get("Sec-Fetch-Dest"); dest != "" && dest != "iframe" {
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	d, err := build(r.Context())
	if err != nil {
		s.renderError(w, r, email, err)
		return
	}
	doc, err := s.frameDocument(r.Context(), d, s.cfg.IsAdmin(email) && !d.thread.Archived)
	if err != nil {
		s.hive.log.Printf("error: assemble frame: %v", err)
		http.Error(w, "this view could not be assembled", http.StatusInternalServerError)
		return
	}
	hd := w.Header()
	hd.Set("Content-Security-Policy", s.frameCSP())
	hd.Del("X-Frame-Options") // the hive's own page frames it; frame-ancestors is the rule
	hd.Set("Content-Type", "text/html; charset=utf-8")
	hd.Set("Cache-Control", "no-store")
	fmt.Fprint(w, doc)
}

// frameDocument assembles the page shown in a frame. The hive contributes
// its runtime (window.hive) and nothing visible: no styles, no markup. Then
// come the libraries and kits the view named, and the agent's HTML with its
// images. Everything is written into the one document, because the frame
// cannot fetch anything.
func (s *Server) frameDocument(ctx context.Context, d *frameDoc, canWrite bool) (string, error) {
	body := d.card
	shown := *d.thread
	info := map[string]any{"thread": &shown, "origin": s.cfg.BaseURL, "viewer": map[string]bool{"can_write": canWrite && d.view != nil}}
	if d.view != nil {
		body = d.view.HTML
		for _, img := range d.view.Images {
			ctype, data, err := s.hive.AssetBody(ctx, img.Hash)
			if err != nil {
				return "", err
			}
			uri := "data:" + ctype + ";base64," + base64.StdEncoding.EncodeToString(data)
			body = strings.ReplaceAll(body, img.Source, uri)
			body = strings.ReplaceAll(body, html.EscapeString(img.Source), uri)
		}
		store, err := s.hive.ThreadData(ctx, d.thread.ID)
		if err != nil {
			return "", err
		}
		info["data"] = store
		if d.replyID != "" {
			info["reply_id"] = d.replyID
		}
	}
	data, _ := json.Marshal(info) // escapes <, > and &

	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>View</title>`)
	b.WriteString(`<script type="application/json" id="hive-data">` + string(data) + `</script>`)
	b.WriteString("<script>" + s.frameJS + "</script>")
	inline := func(tag, attr, name, code string) {
		if code != "" {
			fmt.Fprintf(&b, "<%s data-%s=\"%s\">%s</%s>", tag, attr, html.EscapeString(name), inlineSafe.Replace(code), tag)
		}
	}
	if d.view != nil {
		for _, lib := range d.view.Libraries {
			ctype, code, err := s.hive.AssetBody(ctx, lib.Hash)
			if err != nil {
				return "", err
			}
			tag := "script"
			if strings.HasPrefix(ctype, "text/css") {
				tag = "style"
			}
			inline(tag, "library", lib.Source, string(code))
		}
		for _, kit := range d.view.kits {
			js, css, err := s.hive.kitSource(ctx, kit)
			if err != nil {
				return "", err
			}
			inline("style", "kit", kit.Ref, css)
			inline("script", "kit", kit.Ref, js)
		}
	}
	b.WriteString("</head><body>")
	b.WriteString(body)
	b.WriteString("</body></html>")
	return b.String(), nil
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
