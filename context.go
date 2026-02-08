package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
)

// handleAgentContext returns what a specific agent has been doing:
// their profile, recent threads, recent replies, and active status tags.
func handleAgentContext(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	agent := AgentFromContext(r.Context())
	if agent == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	agentID := r.PathValue("id")
	if agentID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing agent id"})
		return
	}

	// Query agent record
	var a Agent
	err := db.QueryRow(
		`SELECT id, name, owner, created_at, last_seen_at FROM agents WHERE id = ?`, agentID,
	).Scan(&a.ID, &a.Name, &a.Owner, &a.CreatedAt, &a.LastSeenAt)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "agent not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to query agent"})
		return
	}

	// Query last 10 threads by this agent
	threadRows, err := db.Query(
		`SELECT t.id, t.agent_id, a.name, t.title, t.body, t.tags, t.pinned, t.archived, t.created_at, t.updated_at
		FROM threads t
		JOIN agents a ON t.agent_id = a.id
		WHERE t.agent_id = ?
		ORDER BY t.created_at DESC
		LIMIT 10`, agentID,
	)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to query threads"})
		return
	}
	defer threadRows.Close()

	threads := []Thread{}
	for threadRows.Next() {
		var t Thread
		var tagsStr string
		var pinned, archived int
		if err := threadRows.Scan(&t.ID, &t.AgentID, &t.AgentName, &t.Title, &t.Body, &tagsStr, &pinned, &archived, &t.CreatedAt, &t.UpdatedAt); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to scan thread"})
			return
		}
		t.Pinned = pinned != 0
		t.Archived = archived != 0
		if err := json.Unmarshal([]byte(tagsStr), &t.Tags); err != nil {
			t.Tags = []string{}
		}
		threads = append(threads, t)
	}
	if err := threadRows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to iterate threads"})
		return
	}

	// Query last 10 replies by this agent (with thread title for context)
	type ReplyWithThreadTitle struct {
		Reply
		ThreadTitle string `json:"thread_title"`
	}

	replyRows, err := db.Query(
		`SELECT r.id, r.thread_id, r.agent_id, a.name, r.body, r.created_at, r.updated_at, t.title
		FROM replies r
		JOIN agents a ON r.agent_id = a.id
		JOIN threads t ON r.thread_id = t.id
		WHERE r.agent_id = ?
		ORDER BY r.created_at DESC
		LIMIT 10`, agentID,
	)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to query replies"})
		return
	}
	defer replyRows.Close()

	replies := []ReplyWithThreadTitle{}
	for replyRows.Next() {
		var rr ReplyWithThreadTitle
		if err := replyRows.Scan(&rr.ID, &rr.ThreadID, &rr.AgentID, &rr.AgentName, &rr.Body, &rr.CreatedAt, &rr.UpdatedAt, &rr.ThreadTitle); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to scan reply"})
			return
		}
		replies = append(replies, rr)
	}
	if err := replyRows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to iterate replies"})
		return
	}

	// Query active status tags applied by this agent
	statusRows, err := db.Query(
		`SELECT s.id, s.thread_id, s.reply_id, s.agent_id, a.name, s.tag, s.reference_id, s.created_at
		FROM status_tags s
		JOIN agents a ON s.agent_id = a.id
		WHERE s.agent_id = ?
		ORDER BY s.created_at DESC`, agentID,
	)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to query status tags"})
		return
	}
	defer statusRows.Close()

	statuses := []StatusTag{}
	for statusRows.Next() {
		var st StatusTag
		if err := statusRows.Scan(&st.ID, &st.ThreadID, &st.ReplyID, &st.AgentID, &st.AgentName, &st.Tag, &st.ReferenceID, &st.CreatedAt); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to scan status tag"})
			return
		}
		statuses = append(statuses, st)
	}
	if err := statusRows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to iterate status tags"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"agent":           a,
		"recent_threads":  threads,
		"recent_replies":  replies,
		"active_statuses": statuses,
	})
}

// handleActiveContext returns an overview of all currently active work:
// announcements, in-progress items, needs-review items, blocked items, and recent threads.
func handleActiveContext(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	agent := AgentFromContext(r.Context())
	if agent == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	// Query active announcements
	annRows, err := db.Query(
		`SELECT id, title, body, active, created_at FROM announcements WHERE active = 1 ORDER BY created_at DESC`,
	)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to query announcements"})
		return
	}
	defer annRows.Close()

	announcements := []Announcement{}
	for annRows.Next() {
		var ann Announcement
		var active int
		if err := annRows.Scan(&ann.ID, &ann.Title, &ann.Body, &active, &ann.CreatedAt); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to scan announcement"})
			return
		}
		ann.Active = active != 0
		announcements = append(announcements, ann)
	}
	if err := annRows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to iterate announcements"})
		return
	}

	// Helper to query threads by status tag
	queryThreadsByStatus := func(tag string) ([]Thread, error) {
		rows, err := db.Query(
			`SELECT DISTINCT t.id, t.agent_id, a.name, t.title, t.body, t.tags, t.pinned, t.archived, t.created_at, t.updated_at
			FROM threads t
			JOIN agents a ON t.agent_id = a.id
			JOIN status_tags s ON s.thread_id = t.id
			WHERE s.tag = ?
			ORDER BY t.created_at DESC`, tag,
		)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		threads := []Thread{}
		for rows.Next() {
			var t Thread
			var tagsStr string
			var pinned, archived int
			if err := rows.Scan(&t.ID, &t.AgentID, &t.AgentName, &t.Title, &t.Body, &tagsStr, &pinned, &archived, &t.CreatedAt, &t.UpdatedAt); err != nil {
				return nil, err
			}
			t.Pinned = pinned != 0
			t.Archived = archived != 0
			if err := json.Unmarshal([]byte(tagsStr), &t.Tags); err != nil {
				t.Tags = []string{}
			}
			threads = append(threads, t)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return threads, nil
	}

	inProgress, err := queryThreadsByStatus("in-progress")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to query in-progress threads"})
		return
	}

	needsReview, err := queryThreadsByStatus("needs-review")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to query needs-review threads"})
		return
	}

	blocked, err := queryThreadsByStatus("blocked")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to query blocked threads"})
		return
	}

	// Query last 20 threads
	recentRows, err := db.Query(
		`SELECT t.id, t.agent_id, a.name, t.title, t.body, t.tags, t.pinned, t.archived, t.created_at, t.updated_at
		FROM threads t
		JOIN agents a ON t.agent_id = a.id
		ORDER BY t.created_at DESC
		LIMIT 20`,
	)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to query recent threads"})
		return
	}
	defer recentRows.Close()

	recentThreads := []Thread{}
	for recentRows.Next() {
		var t Thread
		var tagsStr string
		var pinned, archived int
		if err := recentRows.Scan(&t.ID, &t.AgentID, &t.AgentName, &t.Title, &t.Body, &tagsStr, &pinned, &archived, &t.CreatedAt, &t.UpdatedAt); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to scan thread"})
			return
		}
		t.Pinned = pinned != 0
		t.Archived = archived != 0
		if err := json.Unmarshal([]byte(tagsStr), &t.Tags); err != nil {
			t.Tags = []string{}
		}
		recentThreads = append(recentThreads, t)
	}
	if err := recentRows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to iterate recent threads"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"announcements":  announcements,
		"in_progress":    inProgress,
		"needs_review":   needsReview,
		"blocked":        blocked,
		"recent_threads": recentThreads,
	})
}

// handleDependencies returns the dependency graph: all status_tags where
// the tag is "depends-on" or "blocked" and reference_id is not null,
// with source and target thread/reply info joined.
func handleDependencies(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	agent := AgentFromContext(r.Context())
	if agent == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	type DependencyNode struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		AgentName string `json:"agent_name"`
	}

	type DependencyEdge struct {
		Source    DependencyNode `json:"source"`
		DependsOn DependencyNode `json:"depends_on"`
		Status   string         `json:"status"`
	}

	// Query status_tags that represent dependency relationships:
	// tag is "depends-on" or "blocked" AND reference_id IS NOT NULL.
	// Join to get source thread info and referenced thread info.
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to query dependencies"})
		return
	}
	defer rows.Close()

	dependencies := []DependencyEdge{}
	for rows.Next() {
		var edge DependencyEdge
		var sourceID, refID string
		if err := rows.Scan(
			&edge.Status,
			&sourceID, &edge.Source.Title, &edge.Source.AgentName,
			&refID, &edge.DependsOn.Title, &edge.DependsOn.AgentName,
		); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to scan dependency"})
			return
		}
		edge.Source.ID = sourceID
		edge.DependsOn.ID = refID
		dependencies = append(dependencies, edge)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to iterate dependencies"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"dependencies": dependencies,
	})
}
