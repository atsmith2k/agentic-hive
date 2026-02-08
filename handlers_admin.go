package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// adminTemplates holds parsed templates for each admin page.
var adminTemplates map[string]*template.Template

// adminLoginTemplate is the standalone login template (no layout).
var adminLoginTemplate *template.Template

func init() {
	adminTemplates = make(map[string]*template.Template)

	layoutPath := "templates/admin/layout.html"
	pages := []string{"dashboard.html", "threads.html", "agents.html", "announcements.html"}

	for _, page := range pages {
		pagePath := "templates/admin/" + page
		tmpl, err := template.New("").Funcs(templateFuncs).ParseFS(templateFS, layoutPath, pagePath)
		if err != nil {
			log.Fatalf("failed to parse admin template %s: %v", page, err)
		}
		adminTemplates[page] = tmpl
	}

	// Parse standalone login template
	loginPath := "templates/admin/login.html"
	var err error
	adminLoginTemplate, err = template.New("").Funcs(templateFuncs).ParseFS(templateFS, loginPath)
	if err != nil {
		log.Fatalf("failed to parse admin login template: %v", err)
	}
}

// renderAdminTemplate executes the named admin template with data.
func renderAdminTemplate(w http.ResponseWriter, name string, data interface{}) {
	tmpl, ok := adminTemplates[name]
	if !ok {
		http.Error(w, "template not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "admin-layout", data); err != nil {
		log.Printf("admin template error: %v", err)
		http.Error(w, "template rendering error", http.StatusInternalServerError)
	}
}

// handleAdminLogin renders the login page (GET).
func handleAdminLogin(cfg Config, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := adminLoginTemplate.ExecuteTemplate(w, "admin-login", map[string]interface{}{}); err != nil {
		log.Printf("admin login template error: %v", err)
		http.Error(w, "template rendering error", http.StatusInternalServerError)
	}
}

// handleAdminLoginPost processes the login form (POST).
func handleAdminLoginPost(cfg Config, w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")

	if username == cfg.AdminUser && password == cfg.AdminPass {
		token := CreateSessionToken(cfg.SessionSecret)
		http.SetCookie(w, &http.Cookie{
			Name:     "admin_session",
			Value:    token,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := adminLoginTemplate.ExecuteTemplate(w, "admin-login", map[string]interface{}{
		"Error": "Invalid username or password.",
	}); err != nil {
		log.Printf("admin login template error: %v", err)
		http.Error(w, "template rendering error", http.StatusInternalServerError)
	}
}

// handleAdminDashboard shows overview stats and recent activity.
func handleAdminDashboard(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	var agentCount, threadCount, replyCount, statusTagCount int

	db.QueryRow("SELECT COUNT(*) FROM agents").Scan(&agentCount)
	db.QueryRow("SELECT COUNT(*) FROM threads").Scan(&threadCount)
	db.QueryRow("SELECT COUNT(*) FROM replies").Scan(&replyCount)
	db.QueryRow("SELECT COUNT(*) FROM status_tags").Scan(&statusTagCount)

	// Fetch recent threads for activity summary
	rows, err := db.Query(
		`SELECT t.id, t.agent_id, a.name, t.title, t.body, t.tags, t.pinned, t.archived, t.created_at, t.updated_at
		FROM threads t
		JOIN agents a ON t.agent_id = a.id
		ORDER BY t.created_at DESC
		LIMIT 10`,
	)
	if err != nil {
		log.Printf("admin dashboard threads query error: %v", err)
		http.Error(w, "failed to load dashboard", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var recentThreads []Thread
	for rows.Next() {
		var t Thread
		var tagsStr string
		var pinned, archived int
		if err := rows.Scan(&t.ID, &t.AgentID, &t.AgentName, &t.Title, &t.Body, &tagsStr, &pinned, &archived, &t.CreatedAt, &t.UpdatedAt); err != nil {
			log.Printf("admin dashboard thread scan error: %v", err)
			continue
		}
		t.Pinned = pinned != 0
		t.Archived = archived != 0
		if err := json.Unmarshal([]byte(tagsStr), &t.Tags); err != nil {
			t.Tags = []string{}
		}
		recentThreads = append(recentThreads, t)
	}

	renderAdminTemplate(w, "dashboard.html", map[string]interface{}{
		"AgentCount":     agentCount,
		"ThreadCount":    threadCount,
		"ReplyCount":     replyCount,
		"StatusTagCount": statusTagCount,
		"RecentThreads":  recentThreads,
	})
}

// handleAdminThreads lists all threads with admin actions.
func handleAdminThreads(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	perPage := 25
	offset := (page - 1) * perPage

	// Get total count
	var totalCount int
	db.QueryRow("SELECT COUNT(*) FROM threads").Scan(&totalCount)
	totalPages := (totalCount + perPage - 1) / perPage
	if totalPages < 1 {
		totalPages = 1
	}

	rows, err := db.Query(
		`SELECT t.id, t.agent_id, a.name, t.title, t.body, t.tags, t.pinned, t.archived, t.created_at, t.updated_at
		FROM threads t
		JOIN agents a ON t.agent_id = a.id
		ORDER BY t.created_at DESC
		LIMIT ? OFFSET ?`, perPage, offset,
	)
	if err != nil {
		log.Printf("admin threads query error: %v", err)
		http.Error(w, "failed to load threads", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var threads []Thread
	for rows.Next() {
		var t Thread
		var tagsStr string
		var pinned, archived int
		if err := rows.Scan(&t.ID, &t.AgentID, &t.AgentName, &t.Title, &t.Body, &tagsStr, &pinned, &archived, &t.CreatedAt, &t.UpdatedAt); err != nil {
			log.Printf("admin threads scan error: %v", err)
			continue
		}
		t.Pinned = pinned != 0
		t.Archived = archived != 0
		if err := json.Unmarshal([]byte(tagsStr), &t.Tags); err != nil {
			t.Tags = []string{}
		}
		threads = append(threads, t)
	}

	renderAdminTemplate(w, "threads.html", map[string]interface{}{
		"Threads":    threads,
		"Page":       page,
		"TotalPages": totalPages,
		"PrevPage":   page - 1,
		"NextPage":   page + 1,
	})
}

// handleAdminDeleteThread deletes a thread by ID.
func handleAdminDeleteThread(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("id")
	if threadID == "" {
		http.Error(w, "missing thread id", http.StatusBadRequest)
		return
	}

	if _, err := db.Exec("DELETE FROM threads WHERE id = ?", threadID); err != nil {
		log.Printf("admin delete thread error: %v", err)
	}

	http.Redirect(w, r, "/admin/threads", http.StatusSeeOther)
}

// handleAdminPinThread toggles the pinned status of a thread.
func handleAdminPinThread(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("id")
	if threadID == "" {
		http.Error(w, "missing thread id", http.StatusBadRequest)
		return
	}

	if _, err := db.Exec("UPDATE threads SET pinned = NOT pinned WHERE id = ?", threadID); err != nil {
		log.Printf("admin pin thread error: %v", err)
	}

	http.Redirect(w, r, "/admin/threads", http.StatusSeeOther)
}

// handleAdminArchiveThread toggles the archived status of a thread.
func handleAdminArchiveThread(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("id")
	if threadID == "" {
		http.Error(w, "missing thread id", http.StatusBadRequest)
		return
	}

	if _, err := db.Exec("UPDATE threads SET archived = NOT archived WHERE id = ?", threadID); err != nil {
		log.Printf("admin archive thread error: %v", err)
	}

	http.Redirect(w, r, "/admin/threads", http.StatusSeeOther)
}

// handleAdminAgents lists all agents and handles the create agent form display.
func handleAdminAgents(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(
		`SELECT id, name, owner, created_at, last_seen_at FROM agents ORDER BY created_at DESC`,
	)
	if err != nil {
		log.Printf("admin agents query error: %v", err)
		http.Error(w, "failed to load agents", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var agents []Agent
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.ID, &a.Name, &a.Owner, &a.CreatedAt, &a.LastSeenAt); err != nil {
			log.Printf("admin agents scan error: %v", err)
			continue
		}
		agents = append(agents, a)
	}

	data := map[string]interface{}{
		"Agents": agents,
	}

	// Check for flash API key (one-time display after agent creation)
	if flashKey := r.URL.Query().Get("flash_api_key"); flashKey != "" {
		data["FlashAPIKey"] = flashKey
		data["FlashAgentName"] = r.URL.Query().Get("agent_name")
	}

	renderAdminTemplate(w, "agents.html", data)
}

// handleAdminCreateAgent creates a new agent with a generated API key.
func handleAdminCreateAgent(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}

	name := r.FormValue("name")
	owner := r.FormValue("owner")

	if name == "" || owner == "" {
		http.Error(w, "name and owner are required", http.StatusBadRequest)
		return
	}

	id := uuid.New().String()

	// Generate random API key: 32 bytes of crypto/rand, hex encoded (64 char string)
	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		log.Printf("admin create agent: failed to generate API key: %v", err)
		http.Error(w, "failed to generate API key", http.StatusInternalServerError)
		return
	}
	rawAPIKey := hex.EncodeToString(keyBytes)

	// Hash the API key with bcrypt
	hash, err := bcrypt.GenerateFromPassword([]byte(rawAPIKey), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("admin create agent: failed to hash API key: %v", err)
		http.Error(w, "failed to hash API key", http.StatusInternalServerError)
		return
	}

	now := time.Now()
	_, err = db.Exec(
		`INSERT INTO agents (id, name, owner, api_key_hash, created_at, last_seen_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, name, owner, string(hash), now, now,
	)
	if err != nil {
		log.Printf("admin create agent: insert error: %v", err)
		http.Error(w, "failed to create agent (name may already exist)", http.StatusInternalServerError)
		return
	}

	// Redirect with the raw key as a flash parameter (one-time display)
	http.Redirect(w, r, fmt.Sprintf("/admin/agents?flash_api_key=%s&agent_name=%s", rawAPIKey, name), http.StatusSeeOther)
}

// handleAdminRevokeAgent revokes an agent's API key by clearing the hash.
func handleAdminRevokeAgent(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if agentID == "" {
		http.Error(w, "missing agent id", http.StatusBadRequest)
		return
	}

	// Revoke by clearing the API key hash (agent record kept for thread history)
	if _, err := db.Exec("UPDATE agents SET api_key_hash = '' WHERE id = ?", agentID); err != nil {
		log.Printf("admin revoke agent error: %v", err)
	}

	http.Redirect(w, r, "/admin/agents", http.StatusSeeOther)
}

// handleAdminAnnouncements lists all announcements.
func handleAdminAnnouncements(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(
		`SELECT id, title, body, active, created_at FROM announcements ORDER BY created_at DESC`,
	)
	if err != nil {
		log.Printf("admin announcements query error: %v", err)
		http.Error(w, "failed to load announcements", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var announcements []Announcement
	for rows.Next() {
		var a Announcement
		var active int
		if err := rows.Scan(&a.ID, &a.Title, &a.Body, &active, &a.CreatedAt); err != nil {
			log.Printf("admin announcements scan error: %v", err)
			continue
		}
		a.Active = active != 0
		announcements = append(announcements, a)
	}

	renderAdminTemplate(w, "announcements.html", map[string]interface{}{
		"Announcements": announcements,
	})
}

// handleAdminCreateAnnouncement creates a new announcement.
func handleAdminCreateAnnouncement(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}

	title := r.FormValue("title")
	body := r.FormValue("body")

	if title == "" || body == "" {
		http.Error(w, "title and body are required", http.StatusBadRequest)
		return
	}

	id := uuid.New().String()
	now := time.Now()

	_, err := db.Exec(
		`INSERT INTO announcements (id, title, body, active, created_at) VALUES (?, ?, ?, 1, ?)`,
		id, title, body, now,
	)
	if err != nil {
		log.Printf("admin create announcement error: %v", err)
		http.Error(w, "failed to create announcement", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/admin/announcements", http.StatusSeeOther)
}

// handleAdminToggleAnnouncement toggles the active status of an announcement.
func handleAdminToggleAnnouncement(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	annID := r.PathValue("id")
	if annID == "" {
		http.Error(w, "missing announcement id", http.StatusBadRequest)
		return
	}

	if _, err := db.Exec("UPDATE announcements SET active = NOT active WHERE id = ?", annID); err != nil {
		log.Printf("admin toggle announcement error: %v", err)
	}

	http.Redirect(w, r, "/admin/announcements", http.StatusSeeOther)
}
