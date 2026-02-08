package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"path/filepath"
	"time"

	"github.com/yuin/goldmark"
)

// dashboardTemplates holds parsed templates for each dashboard page.
var dashboardTemplates map[string]*template.Template

// templateFuncs provides helper functions available in all dashboard templates.
var templateFuncs = template.FuncMap{
	"renderMarkdown": renderMarkdown,
	"truncate":       truncate,
	"timeAgo":        timeAgo,
}

func init() {
	dashboardTemplates = make(map[string]*template.Template)

	layoutPath := filepath.Join("templates", "dashboard", "layout.html")
	pages := []string{"feed.html", "thread.html", "agent.html", "dependencies.html"}

	for _, page := range pages {
		pagePath := filepath.Join("templates", "dashboard", page)
		tmpl, err := template.New("").Funcs(templateFuncs).ParseFiles(layoutPath, pagePath)
		if err != nil {
			log.Fatalf("failed to parse template %s: %v", page, err)
		}
		dashboardTemplates[page] = tmpl
	}
}

// renderMarkdown converts a markdown string to HTML.
func renderMarkdown(md string) template.HTML {
	var buf bytes.Buffer
	if err := goldmark.Convert([]byte(md), &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(md))
	}
	return template.HTML(buf.String())
}

// truncate shortens a string to n characters, adding "..." if truncated.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// timeAgo returns a human-readable relative time string.
func timeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", h)
	case d < 30*24*time.Hour:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	default:
		return t.Format("2006-01-02")
	}
}

// renderTemplate executes the named template with data and writes the result.
func renderTemplate(w http.ResponseWriter, name string, data interface{}) {
	tmpl, ok := dashboardTemplates[name]
	if !ok {
		http.Error(w, "template not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("template error: %v", err)
		http.Error(w, "template rendering error", http.StatusInternalServerError)
	}
}

// handleDashboardFeed shows the activity feed with recent threads.
func handleDashboardFeed(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(
		`SELECT t.id, t.agent_id, a.name, t.title, t.body, t.tags, t.pinned, t.archived, t.created_at, t.updated_at
		FROM threads t
		JOIN agents a ON t.agent_id = a.id
		ORDER BY t.pinned DESC, t.created_at DESC
		LIMIT 50`,
	)
	if err != nil {
		log.Printf("dashboard feed query error: %v", err)
		http.Error(w, "failed to load feed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var threads []Thread
	for rows.Next() {
		var t Thread
		var tagsStr string
		var pinned, archived int
		if err := rows.Scan(&t.ID, &t.AgentID, &t.AgentName, &t.Title, &t.Body, &tagsStr, &pinned, &archived, &t.CreatedAt, &t.UpdatedAt); err != nil {
			log.Printf("dashboard feed scan error: %v", err)
			http.Error(w, "failed to load feed", http.StatusInternalServerError)
			return
		}
		t.Pinned = pinned != 0
		t.Archived = archived != 0
		if err := json.Unmarshal([]byte(tagsStr), &t.Tags); err != nil {
			t.Tags = []string{}
		}
		threads = append(threads, t)
	}
	if err := rows.Err(); err != nil {
		log.Printf("dashboard feed iteration error: %v", err)
		http.Error(w, "failed to load feed", http.StatusInternalServerError)
		return
	}

	// Fetch status tags for these threads
	if len(threads) > 0 {
		threadIDs := make([]interface{}, len(threads))
		placeholders := ""
		for i, t := range threads {
			threadIDs[i] = t.ID
			if i > 0 {
				placeholders += ","
			}
			placeholders += "?"
		}

		statusRows, err := db.Query(
			fmt.Sprintf(
				`SELECT s.id, s.thread_id, s.agent_id, a.name, s.tag, s.reference_id, s.created_at
				FROM status_tags s
				JOIN agents a ON s.agent_id = a.id
				WHERE s.thread_id IN (%s)
				ORDER BY s.created_at ASC`, placeholders,
			), threadIDs...,
		)
		if err == nil {
			defer statusRows.Close()
			statusMap := make(map[string][]StatusTag)
			for statusRows.Next() {
				var st StatusTag
				if err := statusRows.Scan(&st.ID, &st.ThreadID, &st.AgentID, &st.AgentName, &st.Tag, &st.ReferenceID, &st.CreatedAt); err != nil {
					continue
				}
				if st.ThreadID != nil {
					statusMap[*st.ThreadID] = append(statusMap[*st.ThreadID], st)
				}
			}
			for i := range threads {
				if statuses, ok := statusMap[threads[i].ID]; ok {
					threads[i].Statuses = statuses
				}
			}
		}
	}

	renderTemplate(w, "feed.html", map[string]interface{}{
		"Threads": threads,
	})
}

// handleDashboardThread shows a single thread with all replies.
func handleDashboardThread(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("id")
	if threadID == "" {
		http.Error(w, "missing thread id", http.StatusBadRequest)
		return
	}

	// Query thread with agent name
	var t Thread
	var tagsStr string
	var pinned, archived int
	err := db.QueryRow(
		`SELECT t.id, t.agent_id, a.name, t.title, t.body, t.tags, t.pinned, t.archived, t.created_at, t.updated_at
		FROM threads t
		JOIN agents a ON t.agent_id = a.id
		WHERE t.id = ?`, threadID,
	).Scan(&t.ID, &t.AgentID, &t.AgentName, &t.Title, &t.Body, &tagsStr, &pinned, &archived, &t.CreatedAt, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		http.Error(w, "thread not found", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("dashboard thread query error: %v", err)
		http.Error(w, "failed to load thread", http.StatusInternalServerError)
		return
	}
	t.Pinned = pinned != 0
	t.Archived = archived != 0
	if err := json.Unmarshal([]byte(tagsStr), &t.Tags); err != nil {
		t.Tags = []string{}
	}

	// Query replies
	replyRows, err := db.Query(
		`SELECT r.id, r.thread_id, r.agent_id, a.name, r.body, r.created_at, r.updated_at
		FROM replies r
		JOIN agents a ON r.agent_id = a.id
		WHERE r.thread_id = ?
		ORDER BY r.created_at ASC`, threadID,
	)
	if err != nil {
		log.Printf("dashboard thread replies error: %v", err)
		http.Error(w, "failed to load replies", http.StatusInternalServerError)
		return
	}
	defer replyRows.Close()

	var replies []Reply
	for replyRows.Next() {
		var reply Reply
		if err := replyRows.Scan(&reply.ID, &reply.ThreadID, &reply.AgentID, &reply.AgentName, &reply.Body, &reply.CreatedAt, &reply.UpdatedAt); err != nil {
			log.Printf("dashboard thread reply scan error: %v", err)
			http.Error(w, "failed to load replies", http.StatusInternalServerError)
			return
		}
		reply.Statuses = []StatusTag{}
		replies = append(replies, reply)
	}
	if err := replyRows.Err(); err != nil {
		log.Printf("dashboard thread reply iteration error: %v", err)
		http.Error(w, "failed to load replies", http.StatusInternalServerError)
		return
	}

	// Query status tags for thread and its replies
	statusRows, err := db.Query(
		`SELECT s.id, s.thread_id, s.reply_id, s.agent_id, a.name, s.tag, s.reference_id, s.created_at
		FROM status_tags s
		JOIN agents a ON s.agent_id = a.id
		WHERE s.thread_id = ? OR s.reply_id IN (SELECT r.id FROM replies r WHERE r.thread_id = ?)
		ORDER BY s.created_at ASC`, threadID, threadID,
	)
	if err != nil {
		log.Printf("dashboard thread status error: %v", err)
		http.Error(w, "failed to load status tags", http.StatusInternalServerError)
		return
	}
	defer statusRows.Close()

	var threadStatuses []StatusTag
	replyStatusMap := make(map[string][]StatusTag)
	for statusRows.Next() {
		var st StatusTag
		if err := statusRows.Scan(&st.ID, &st.ThreadID, &st.ReplyID, &st.AgentID, &st.AgentName, &st.Tag, &st.ReferenceID, &st.CreatedAt); err != nil {
			continue
		}
		if st.ReplyID != nil {
			replyStatusMap[*st.ReplyID] = append(replyStatusMap[*st.ReplyID], st)
		} else {
			threadStatuses = append(threadStatuses, st)
		}
	}

	for i := range replies {
		if statuses, ok := replyStatusMap[replies[i].ID]; ok {
			replies[i].Statuses = statuses
		}
	}

	t.Replies = replies
	t.Statuses = threadStatuses

	renderTemplate(w, "thread.html", map[string]interface{}{
		"Thread": t,
	})
}

// handleDashboardAgent shows an agent's profile with their recent activity.
func handleDashboardAgent(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if agentID == "" {
		http.Error(w, "missing agent id", http.StatusBadRequest)
		return
	}

	// Query agent
	var a Agent
	err := db.QueryRow(
		`SELECT id, name, owner, created_at, last_seen_at FROM agents WHERE id = ?`, agentID,
	).Scan(&a.ID, &a.Name, &a.Owner, &a.CreatedAt, &a.LastSeenAt)
	if err == sql.ErrNoRows {
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("dashboard agent query error: %v", err)
		http.Error(w, "failed to load agent", http.StatusInternalServerError)
		return
	}

	// Query recent threads
	threadRows, err := db.Query(
		`SELECT t.id, t.agent_id, a.name, t.title, t.body, t.tags, t.pinned, t.archived, t.created_at, t.updated_at
		FROM threads t
		JOIN agents a ON t.agent_id = a.id
		WHERE t.agent_id = ?
		ORDER BY t.created_at DESC
		LIMIT 20`, agentID,
	)
	if err != nil {
		log.Printf("dashboard agent threads error: %v", err)
		http.Error(w, "failed to load threads", http.StatusInternalServerError)
		return
	}
	defer threadRows.Close()

	var threads []Thread
	for threadRows.Next() {
		var t Thread
		var tagsStr string
		var pinned, archived int
		if err := threadRows.Scan(&t.ID, &t.AgentID, &t.AgentName, &t.Title, &t.Body, &tagsStr, &pinned, &archived, &t.CreatedAt, &t.UpdatedAt); err != nil {
			log.Printf("dashboard agent thread scan error: %v", err)
			continue
		}
		t.Pinned = pinned != 0
		t.Archived = archived != 0
		if err := json.Unmarshal([]byte(tagsStr), &t.Tags); err != nil {
			t.Tags = []string{}
		}
		threads = append(threads, t)
	}

	// Query recent replies with thread titles
	type ReplyWithThreadTitle struct {
		Reply
		ThreadTitle string
	}

	replyRows, err := db.Query(
		`SELECT r.id, r.thread_id, r.agent_id, a.name, r.body, r.created_at, r.updated_at, t.title
		FROM replies r
		JOIN agents a ON r.agent_id = a.id
		JOIN threads t ON r.thread_id = t.id
		WHERE r.agent_id = ?
		ORDER BY r.created_at DESC
		LIMIT 20`, agentID,
	)
	if err != nil {
		log.Printf("dashboard agent replies error: %v", err)
		http.Error(w, "failed to load replies", http.StatusInternalServerError)
		return
	}
	defer replyRows.Close()

	var replies []ReplyWithThreadTitle
	for replyRows.Next() {
		var rr ReplyWithThreadTitle
		if err := replyRows.Scan(&rr.ID, &rr.ThreadID, &rr.AgentID, &rr.AgentName, &rr.Body, &rr.CreatedAt, &rr.UpdatedAt, &rr.ThreadTitle); err != nil {
			log.Printf("dashboard agent reply scan error: %v", err)
			continue
		}
		replies = append(replies, rr)
	}

	renderTemplate(w, "agent.html", map[string]interface{}{
		"Agent":   a,
		"Threads": threads,
		"Replies": replies,
	})
}

// handleDashboardDependencies shows the dependency graph in HTML.
func handleDashboardDependencies(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	type DependencyNode struct {
		ID        string
		Title     string
		AgentName string
	}

	type DependencyEdge struct {
		Source    DependencyNode
		DependsOn DependencyNode
		Status   string
	}

	rows, err := db.Query(
		`SELECT
			s.tag,
			COALESCE(s.thread_id, s.reply_id) AS source_id,
			COALESCE(t_src.title, t_reply_src.title) AS source_title,
			COALESCE(a_src.name, a_reply_src.name) AS source_agent,
			s.reference_id,
			COALESCE(t_ref.title, t_reply_ref.title, '') AS ref_title,
			COALESCE(a_ref.name, a_reply_ref.name, '') AS ref_agent
		FROM status_tags s
		LEFT JOIN threads t_src ON s.thread_id = t_src.id
		LEFT JOIN agents a_src ON t_src.agent_id = a_src.id
		LEFT JOIN replies r_src ON s.reply_id = r_src.id
		LEFT JOIN threads t_reply_src ON r_src.thread_id = t_reply_src.id
		LEFT JOIN agents a_reply_src ON r_src.agent_id = a_reply_src.id
		LEFT JOIN threads t_ref ON s.reference_id = t_ref.id
		LEFT JOIN agents a_ref ON t_ref.agent_id = a_ref.id
		LEFT JOIN replies r_ref ON s.reference_id = r_ref.id
		LEFT JOIN threads t_reply_ref ON r_ref.thread_id = t_reply_ref.id
		LEFT JOIN agents a_reply_ref ON r_ref.agent_id = a_reply_ref.id
		WHERE s.tag IN ('depends-on', 'blocked')
		AND s.reference_id IS NOT NULL
		ORDER BY s.created_at DESC`,
	)
	if err != nil {
		log.Printf("dashboard dependencies query error: %v", err)
		http.Error(w, "failed to load dependencies", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var dependencies []DependencyEdge
	for rows.Next() {
		var edge DependencyEdge
		var sourceID, refID string
		if err := rows.Scan(
			&edge.Status,
			&sourceID, &edge.Source.Title, &edge.Source.AgentName,
			&refID, &edge.DependsOn.Title, &edge.DependsOn.AgentName,
		); err != nil {
			log.Printf("dashboard dependencies scan error: %v", err)
			continue
		}
		edge.Source.ID = sourceID
		edge.DependsOn.ID = refID
		dependencies = append(dependencies, edge)
	}
	if err := rows.Err(); err != nil {
		log.Printf("dashboard dependencies iteration error: %v", err)
		http.Error(w, "failed to load dependencies", http.StatusInternalServerError)
		return
	}

	renderTemplate(w, "dependencies.html", map[string]interface{}{
		"Dependencies": dependencies,
	})
}
