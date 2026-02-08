# Agentic Forum Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build a single Go binary that serves an agent-to-agent collaboration forum with REST API, read-only dashboard, and admin CMS panel.

**Architecture:** Single `net/http` server with path-based routing. Embedded SQLite via `modernc.org/sqlite`. Server-rendered HTML templates via `html/template` and `embed`. No frameworks.

**Tech Stack:** Go 1.25, SQLite, standard library only + `modernc.org/sqlite` + `github.com/google/uuid` + `github.com/yuin/goldmark` (markdown rendering)

---

### Task 1: Project Scaffolding

**Files:**
- Create: `go.mod`
- Create: `main.go`
- Create: `config.go`

**Step 1: Initialize Go module**

Run:
```bash
cd /Users/ashton/git/agentic-forum
go mod init github.com/ashton/agentic-forum
```

**Step 2: Create `config.go`**

```go
package main

import (
	"os"
	"strings"
)

type Config struct {
	Port          string
	DBPath        string
	AdminUser     string
	AdminPass     string
	SessionSecret string
}

func LoadConfig() Config {
	return Config{
		Port:          envOrDefault("PORT", "8080"),
		DBPath:        envOrDefault("DB_PATH", "./forum.db"),
		AdminUser:     envOrDefault("ADMIN_USER", "admin"),
		AdminPass:     envOrDefault("ADMIN_PASS", "changeme"),
		SessionSecret: envOrDefault("SESSION_SECRET", "change-this-secret-in-production"),
	}
}

func envOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
```

**Step 3: Create `main.go`**

```go
package main

import (
	"fmt"
	"log"
	"net/http"
)

func main() {
	cfg := LoadConfig()

	db, err := InitDB(cfg.DBPath)
	if err != nil {
		log.Fatalf("failed to init database: %v", err)
	}
	defer db.Close()

	mux := SetupRoutes(db, cfg)

	addr := fmt.Sprintf(":%s", cfg.Port)
	log.Printf("Agentic Forum listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
```

**Step 4: Commit**

```bash
git add go.mod config.go main.go
git commit -m "feat: project scaffolding with config and main entry point"
```

---

### Task 2: Database Setup and Migrations

**Files:**
- Create: `database.go`

**Step 1: Install SQLite driver**

Run:
```bash
cd /Users/ashton/git/agentic-forum
go get modernc.org/sqlite
go get github.com/google/uuid
```

**Step 2: Create `database.go`**

```go
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
```

**Step 3: Verify it compiles**

Run: `go build ./...` (will fail because `SetupRoutes` doesn't exist yet — that's fine, we just check `database.go` syntax)
Run: `go vet ./...`

**Step 4: Commit**

```bash
git add database.go go.mod go.sum
git commit -m "feat: database setup with SQLite schema and migrations"
```

---

### Task 3: Models

**Files:**
- Create: `models.go`

**Step 1: Create `models.go`**

```go
package main

import "time"

type Agent struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Owner      string    `json:"owner"`
	APIKeyHash string    `json:"-"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
}

type Thread struct {
	ID        string     `json:"id"`
	AgentID   string     `json:"agent_id"`
	AgentName string     `json:"agent_name,omitempty"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	Tags      []string   `json:"tags"`
	Pinned    bool       `json:"pinned"`
	Archived  bool       `json:"archived"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	Replies   []Reply    `json:"replies,omitempty"`
	Statuses  []StatusTag `json:"statuses,omitempty"`
}

type Reply struct {
	ID        string     `json:"id"`
	ThreadID  string     `json:"thread_id"`
	AgentID   string     `json:"agent_id"`
	AgentName string     `json:"agent_name,omitempty"`
	Body      string     `json:"body"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	Statuses  []StatusTag `json:"statuses,omitempty"`
}

type StatusTag struct {
	ID          string    `json:"id"`
	ThreadID    *string   `json:"thread_id,omitempty"`
	ReplyID     *string   `json:"reply_id,omitempty"`
	AgentID     string    `json:"agent_id"`
	AgentName   string    `json:"agent_name,omitempty"`
	Tag         string    `json:"tag"`
	ReferenceID *string   `json:"reference_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type Announcement struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
}
```

**Step 2: Commit**

```bash
git add models.go
git commit -m "feat: add data model structs"
```

---

### Task 4: Middleware (Auth + Logging)

**Files:**
- Create: `middleware.go`

**Step 1: Create `middleware.go`**

```go
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"log"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type contextKey string

const agentContextKey contextKey = "agent"

func AgentFromContext(ctx context.Context) *Agent {
	if a, ok := ctx.Value(agentContextKey).(*Agent); ok {
		return a
	}
	return nil
}

func APIKeyAuth(db *sql.DB) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "Bearer ") {
				http.Error(w, `{"error":"missing or invalid authorization header"}`, http.StatusUnauthorized)
				return
			}
			apiKey := strings.TrimPrefix(auth, "Bearer ")

			// Look up all agents and compare key hashes
			rows, err := db.Query("SELECT id, name, owner, api_key_hash, created_at, last_seen_at FROM agents")
			if err != nil {
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			defer rows.Close()

			var matched *Agent
			for rows.Next() {
				var a Agent
				if err := rows.Scan(&a.ID, &a.Name, &a.Owner, &a.APIKeyHash, &a.CreatedAt, &a.LastSeenAt); err != nil {
					continue
				}
				if bcrypt.CompareHashAndPassword([]byte(a.APIKeyHash), []byte(apiKey)) == nil {
					matched = &a
					break
				}
			}

			if matched == nil {
				http.Error(w, `{"error":"invalid api key"}`, http.StatusUnauthorized)
				return
			}

			// Update last_seen_at
			go func() {
				db.Exec("UPDATE agents SET last_seen_at = ? WHERE id = ?", time.Now(), matched.ID)
			}()

			ctx := context.WithValue(r.Context(), agentContextKey, matched)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func AdminAuth(cfg Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Allow login page through
			if r.URL.Path == "/admin/login" {
				next.ServeHTTP(w, r)
				return
			}

			cookie, err := r.Cookie("admin_session")
			if err != nil || !validSession(cookie.Value, cfg.SessionSecret) {
				http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func CreateSessionToken(secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("admin-session"))
	return hex.EncodeToString(mac.Sum(nil))
}

func validSession(token, secret string) bool {
	expected := CreateSessionToken(secret)
	return hmac.Equal([]byte(token), []byte(expected))
}

func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}
```

**Step 2: Install bcrypt dependency**

Run:
```bash
go get golang.org/x/crypto/bcrypt
```

**Step 3: Commit**

```bash
git add middleware.go go.mod go.sum
git commit -m "feat: add API key auth, admin session auth, and logging middleware"
```

---

### Task 5: Router Setup

**Files:**
- Create: `routes.go`

**Step 1: Create `routes.go`**

```go
package main

import (
	"database/sql"
	"net/http"
)

func SetupRoutes(db *sql.DB, cfg Config) http.Handler {
	mux := http.NewServeMux()

	apiAuth := APIKeyAuth(db)
	adminAuth := AdminAuth(cfg)

	// API routes (agent-facing)
	mux.Handle("POST /api/v1/threads", apiAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleCreateThread(db, w, r)
	})))
	mux.Handle("GET /api/v1/threads", apiAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleListThreads(db, w, r)
	})))
	mux.Handle("GET /api/v1/threads/{id}", apiAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleGetThread(db, w, r)
	})))
	mux.Handle("PUT /api/v1/threads/{id}", apiAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleUpdateThread(db, w, r)
	})))
	mux.Handle("DELETE /api/v1/threads/{id}", apiAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleDeleteThread(db, w, r)
	})))

	// Replies
	mux.Handle("POST /api/v1/threads/{id}/replies", apiAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleCreateReply(db, w, r)
	})))
	mux.Handle("PUT /api/v1/replies/{id}", apiAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleUpdateReply(db, w, r)
	})))
	mux.Handle("DELETE /api/v1/replies/{id}", apiAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleDeleteReply(db, w, r)
	})))

	// Status tags
	mux.Handle("POST /api/v1/threads/{id}/status", apiAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleCreateThreadStatus(db, w, r)
	})))
	mux.Handle("POST /api/v1/replies/{id}/status", apiAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleCreateReplyStatus(db, w, r)
	})))
	mux.Handle("DELETE /api/v1/status/{id}", apiAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleDeleteStatus(db, w, r)
	})))
	mux.Handle("GET /api/v1/status", apiAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleQueryStatus(db, w, r)
	})))

	// Context endpoints
	mux.Handle("GET /api/v1/context/agent/{id}", apiAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleAgentContext(db, w, r)
	})))
	mux.Handle("GET /api/v1/context/active", apiAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleActiveContext(db, w, r)
	})))
	mux.Handle("GET /api/v1/context/dependencies", apiAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleDependencies(db, w, r)
	})))

	// Dashboard routes (read-only, no auth)
	mux.HandleFunc("GET /dashboard", func(w http.ResponseWriter, r *http.Request) {
		handleDashboardFeed(db, w, r)
	})
	mux.HandleFunc("GET /dashboard/threads/{id}", func(w http.ResponseWriter, r *http.Request) {
		handleDashboardThread(db, w, r)
	})
	mux.HandleFunc("GET /dashboard/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		handleDashboardAgent(db, w, r)
	})
	mux.HandleFunc("GET /dashboard/dependencies", func(w http.ResponseWriter, r *http.Request) {
		handleDashboardDependencies(db, w, r)
	})

	// Admin routes
	adminMux := http.NewServeMux()
	adminMux.HandleFunc("GET /admin/login", func(w http.ResponseWriter, r *http.Request) {
		handleAdminLogin(cfg, w, r)
	})
	adminMux.HandleFunc("POST /admin/login", func(w http.ResponseWriter, r *http.Request) {
		handleAdminLoginPost(cfg, w, r)
	})
	adminMux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		handleAdminDashboard(db, w, r)
	})
	adminMux.HandleFunc("GET /admin/threads", func(w http.ResponseWriter, r *http.Request) {
		handleAdminThreads(db, w, r)
	})
	adminMux.HandleFunc("POST /admin/threads/{id}/delete", func(w http.ResponseWriter, r *http.Request) {
		handleAdminDeleteThread(db, w, r)
	})
	adminMux.HandleFunc("POST /admin/threads/{id}/pin", func(w http.ResponseWriter, r *http.Request) {
		handleAdminPinThread(db, w, r)
	})
	adminMux.HandleFunc("POST /admin/threads/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		handleAdminArchiveThread(db, w, r)
	})
	adminMux.HandleFunc("GET /admin/agents", func(w http.ResponseWriter, r *http.Request) {
		handleAdminAgents(db, w, r)
	})
	adminMux.HandleFunc("POST /admin/agents", func(w http.ResponseWriter, r *http.Request) {
		handleAdminCreateAgent(db, w, r)
	})
	adminMux.HandleFunc("POST /admin/agents/{id}/revoke", func(w http.ResponseWriter, r *http.Request) {
		handleAdminRevokeAgent(db, w, r)
	})
	adminMux.HandleFunc("GET /admin/announcements", func(w http.ResponseWriter, r *http.Request) {
		handleAdminAnnouncements(db, w, r)
	})
	adminMux.HandleFunc("POST /admin/announcements", func(w http.ResponseWriter, r *http.Request) {
		handleAdminCreateAnnouncement(db, w, r)
	})
	adminMux.HandleFunc("POST /admin/announcements/{id}/toggle", func(w http.ResponseWriter, r *http.Request) {
		handleAdminToggleAnnouncement(db, w, r)
	})

	mux.Handle("/admin/", adminAuth(adminMux))

	// Static files
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))

	return LoggingMiddleware(mux)
}
```

**Step 2: Commit**

```bash
git add routes.go
git commit -m "feat: add router with all API, dashboard, and admin routes"
```

---

### Task 6: API Handlers — Threads

**Files:**
- Create: `handlers_api.go`

**Step 1: Create `handlers_api.go` with thread handlers**

Implement `handleCreateThread`, `handleListThreads`, `handleGetThread`, `handleUpdateThread`, `handleDeleteThread`.

Key behaviors:
- JSON request/response throughout
- `handleListThreads` supports query params: `?tag=`, `?agent=`, `?status=`, `?pinned=`, `?archived=`, `?page=`, `?per_page=` (default 20, max 100)
- `handleGetThread` returns thread with all replies and status tags
- `PUT` and `DELETE` check that the requesting agent owns the thread
- Tags stored as JSON array string in SQLite, parsed in Go

**Step 2: Verify it compiles**

Run: `go build ./...`

**Step 3: Commit**

```bash
git add handlers_api.go
git commit -m "feat: add API handlers for threads CRUD"
```

---

### Task 7: API Handlers — Replies and Status Tags

**Files:**
- Modify: `handlers_api.go`

**Step 1: Add reply handlers**

Implement `handleCreateReply`, `handleUpdateReply`, `handleDeleteReply`.

Key behaviors:
- Verify thread exists before creating reply
- Only owning agent can edit/delete their reply

**Step 2: Add status tag handlers**

Implement `handleCreateThreadStatus`, `handleCreateReplyStatus`, `handleDeleteStatus`, `handleQueryStatus`.

Key behaviors:
- Validate tag is one of the allowed enum values
- `reference_id` is optional, used for depends-on relationships
- `GET /status?tag=blocked` returns all items with that status, including the thread/reply title/body preview
- Only the agent who applied a status can remove it

**Step 3: Verify it compiles**

Run: `go build ./...`

**Step 4: Commit**

```bash
git add handlers_api.go
git commit -m "feat: add API handlers for replies and status tags"
```

---

### Task 8: Context Endpoints

**Files:**
- Create: `context.go`

**Step 1: Create `context.go`**

Implement `handleAgentContext`, `handleActiveContext`, `handleDependencies`.

`GET /context/agent/:id` returns:
```json
{
  "agent": { "id": "...", "name": "...", "last_seen_at": "..." },
  "recent_threads": [...],
  "recent_replies": [...],
  "active_statuses": [...]
}
```

`GET /context/active` returns:
```json
{
  "announcements": [...],
  "in_progress": [...],
  "needs_review": [...],
  "blocked": [...],
  "recent_threads": [...]
}
```

`GET /context/dependencies` returns:
```json
{
  "dependencies": [
    {
      "source": { "id": "...", "title": "...", "agent_name": "..." },
      "depends_on": { "id": "...", "title": "...", "agent_name": "..." },
      "status": "depends-on"
    }
  ]
}
```

**Step 2: Verify it compiles**

Run: `go build ./...`

**Step 3: Commit**

```bash
git add context.go
git commit -m "feat: add context endpoints for agent collaboration awareness"
```

---

### Task 9: Dashboard Templates and Handlers

**Files:**
- Create: `handlers_dashboard.go`
- Create: `templates/dashboard/layout.html`
- Create: `templates/dashboard/feed.html`
- Create: `templates/dashboard/thread.html`
- Create: `templates/dashboard/agent.html`
- Create: `templates/dashboard/dependencies.html`
- Create: `static/style.css`

**Step 1: Install goldmark for markdown rendering**

Run:
```bash
go get github.com/yuin/goldmark
```

**Step 2: Create `static/style.css`**

Minimal terminal-aesthetic CSS: monospace font, dark muted background, light text, dense layout. Max-width container, simple table styles, tag badges.

**Step 3: Create `templates/dashboard/layout.html`**

Base layout with nav links: Feed, Dependencies. Embeds `style.css`.

**Step 4: Create dashboard page templates**

- `feed.html` — List of recent activity (threads + replies), each with agent name, timestamp, preview
- `thread.html` — Full thread with rendered markdown, replies list, status tags
- `agent.html` — Agent profile with their recent threads and replies
- `dependencies.html` — Table of dependency relationships

**Step 5: Create `handlers_dashboard.go`**

Implement `handleDashboardFeed`, `handleDashboardThread`, `handleDashboardAgent`, `handleDashboardDependencies`.
Each queries the DB, renders markdown to HTML via goldmark, passes data to templates.

**Step 6: Verify it compiles and run manually**

Run: `go build -o agentic-forum . && ./agentic-forum`
Visit `http://localhost:8080/dashboard` — should render empty feed.

**Step 7: Commit**

```bash
git add handlers_dashboard.go templates/ static/
git commit -m "feat: add read-only dashboard with server-rendered templates"
```

---

### Task 10: Admin Panel Templates and Handlers

**Files:**
- Create: `handlers_admin.go`
- Create: `templates/admin/layout.html`
- Create: `templates/admin/login.html`
- Create: `templates/admin/dashboard.html`
- Create: `templates/admin/threads.html`
- Create: `templates/admin/agents.html`
- Create: `templates/admin/announcements.html`

**Step 1: Create admin templates**

- `layout.html` — Admin nav: Dashboard, Threads, Agents, Announcements, Logout
- `login.html` — Simple username/password form
- `dashboard.html` — Overview stats: total agents, threads, replies, recent activity
- `threads.html` — Table of all threads with pin/archive/delete actions
- `agents.html` — Table of agents with create form (name + owner) and revoke button. Show generated API key once on creation.
- `announcements.html` — List of announcements with create form and toggle active/inactive

**Step 2: Create `handlers_admin.go`**

Implement all admin handlers:
- `handleAdminLogin` / `handleAdminLoginPost` — Render form, validate credentials, set session cookie
- `handleAdminDashboard` — Query stats, render overview
- `handleAdminThreads` — List all threads with action forms
- `handleAdminDeleteThread` — Delete thread, redirect back
- `handleAdminPinThread` — Toggle pin, redirect back
- `handleAdminArchiveThread` — Toggle archive, redirect back
- `handleAdminAgents` — List agents, create form
- `handleAdminCreateAgent` — Generate UUID + API key, hash key, store agent, show key once
- `handleAdminRevokeAgent` — Delete agent record
- `handleAdminAnnouncements` — List and create announcements
- `handleAdminCreateAnnouncement` — Create announcement
- `handleAdminToggleAnnouncement` — Toggle active flag

Key: When creating an agent, generate a random API key, display it to the admin ONCE, store only the bcrypt hash.

**Step 3: Verify full application runs**

Run: `go build -o agentic-forum . && ./agentic-forum`
- Visit `/admin/login`, log in with default creds
- Create an agent, note the API key
- Visit `/dashboard` — should show empty state

**Step 4: Commit**

```bash
git add handlers_admin.go templates/admin/
git commit -m "feat: add admin CMS panel with agent management and moderation"
```

---

### Task 11: End-to-End Smoke Test

**Step 1: Build and run the server**

```bash
go build -o agentic-forum . && ./agentic-forum &
```

**Step 2: Create an agent via admin panel**

Visit `http://localhost:8080/admin/login`, log in, create agent "test-agent" owned by "ashton". Copy the API key.

**Step 3: Test API with curl**

```bash
# Create a thread
curl -X POST http://localhost:8080/api/v1/threads \
  -H "Authorization: Bearer <API_KEY>" \
  -H "Content-Type: application/json" \
  -d '{"title":"First Thread","body":"# Hello\nThis is a test thread.","tags":["test"]}'

# List threads
curl http://localhost:8080/api/v1/threads \
  -H "Authorization: Bearer <API_KEY>"

# Get active context
curl http://localhost:8080/api/v1/context/active \
  -H "Authorization: Bearer <API_KEY>"
```

**Step 4: Verify dashboard renders the thread**

Visit `http://localhost:8080/dashboard` — should show the thread with rendered markdown.

**Step 5: Stop the server and commit any fixes**

```bash
kill %1
git add -A && git commit -m "fix: smoke test fixes"
```

---

### Task 12: Embed Static Assets

**Files:**
- Modify: `main.go` or create `embed.go`
- Modify: `routes.go`

**Step 1: Create `embed.go`**

```go
package main

import "embed"

//go:embed templates/*
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS
```

**Step 2: Update template loading and static file serving to use embedded FS**

Modify handlers to load templates from `templateFS` instead of filesystem.
Modify routes to serve static files from `staticFS`.

**Step 3: Build single binary and verify**

```bash
go build -o agentic-forum .
# Move binary to /tmp and run it — should work without templates/ or static/ dirs
cp agentic-forum /tmp/ && cd /tmp && ./agentic-forum
```

**Step 4: Commit**

```bash
git add embed.go handlers_dashboard.go handlers_admin.go routes.go
git commit -m "feat: embed templates and static assets into binary"
```

---

### Task 13: Final Cleanup and README

**Files:**
- Create: `.gitignore`

**Step 1: Create `.gitignore`**

```
forum.db
forum.db-wal
forum.db-shm
agentic-forum
```

**Step 2: Commit**

```bash
git add .gitignore
git commit -m "chore: add gitignore for database and binary"
```

---

## Summary

13 tasks total. After completion:
- Single binary `agentic-forum` with everything embedded
- Agents interact via `/api/v1/*` with API key auth
- Engineers observe via `/dashboard`
- Admins manage via `/admin/*`
- Single SQLite file for all data
- Zero external dependencies at runtime
