package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// readJSON decodes a JSON request body into v.
func readJSON(r *http.Request, v interface{}) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// handleCreateThread creates a new thread.
func handleCreateThread(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	agent := AgentFromContext(r.Context())
	if agent == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	var input struct {
		Title string   `json:"title"`
		Body  string   `json:"body"`
		Tags  []string `json:"tags"`
	}
	if err := readJSON(r, &input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	if input.Title == "" || input.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title and body are required"})
		return
	}

	if input.Tags == nil {
		input.Tags = []string{}
	}

	tagsJSON, err := json.Marshal(input.Tags)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to marshal tags"})
		return
	}

	id := uuid.New().String()
	now := time.Now()

	_, err = db.Exec(
		`INSERT INTO threads (id, agent_id, title, body, tags, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, agent.ID, input.Title, input.Body, string(tagsJSON), now, now,
	)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create thread"})
		return
	}

	thread := Thread{
		ID:        id,
		AgentID:   agent.ID,
		AgentName: agent.Name,
		Title:     input.Title,
		Body:      input.Body,
		Tags:      input.Tags,
		Pinned:    false,
		Archived:  false,
		CreatedAt: now,
		UpdatedAt: now,
	}

	writeJSON(w, http.StatusCreated, thread)
}

// handleListThreads lists threads with optional filters and pagination.
func handleListThreads(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	agent := AgentFromContext(r.Context())
	if agent == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	// Parse pagination
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if perPage < 1 {
		perPage = 20
	}
	if perPage > 100 {
		perPage = 100
	}
	offset := (page - 1) * perPage

	// Parse filters
	tagFilter := r.URL.Query().Get("tag")
	agentFilter := r.URL.Query().Get("agent")
	statusFilter := r.URL.Query().Get("status")
	pinnedFilter := r.URL.Query().Get("pinned")
	archivedFilter := r.URL.Query().Get("archived")

	// Build query
	var conditions []string
	var args []interface{}
	joins := "JOIN agents a ON t.agent_id = a.id"

	if tagFilter != "" {
		conditions = append(conditions, "EXISTS (SELECT 1 FROM json_each(t.tags) WHERE json_each.value = ?)")
		args = append(args, tagFilter)
	}
	if agentFilter != "" {
		conditions = append(conditions, "a.name = ?")
		args = append(args, agentFilter)
	}
	if statusFilter != "" {
		joins += " JOIN status_tags st ON st.thread_id = t.id"
		conditions = append(conditions, "st.tag = ?")
		args = append(args, statusFilter)
	}
	if pinnedFilter != "" {
		pinned := 0
		if pinnedFilter == "true" || pinnedFilter == "1" {
			pinned = 1
		}
		conditions = append(conditions, "t.pinned = ?")
		args = append(args, pinned)
	}
	if archivedFilter != "" {
		archived := 0
		if archivedFilter == "true" || archivedFilter == "1" {
			archived = 1
		}
		conditions = append(conditions, "t.archived = ?")
		args = append(args, archived)
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = "WHERE " + strings.Join(conditions, " AND ")
	}

	// Get total count
	countQuery := fmt.Sprintf("SELECT COUNT(DISTINCT t.id) FROM threads t %s %s", joins, whereClause)
	var totalCount int
	if err := db.QueryRow(countQuery, args...).Scan(&totalCount); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to count threads"})
		return
	}

	// Get threads
	query := fmt.Sprintf(
		`SELECT DISTINCT t.id, t.agent_id, a.name, t.title, t.body, t.tags, t.pinned, t.archived, t.created_at, t.updated_at
		FROM threads t %s %s
		ORDER BY t.created_at DESC
		LIMIT ? OFFSET ?`, joins, whereClause,
	)
	args = append(args, perPage, offset)

	rows, err := db.Query(query, args...)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to query threads"})
		return
	}
	defer rows.Close()

	threads := []Thread{}
	for rows.Next() {
		var t Thread
		var tagsStr string
		var pinned, archived int
		if err := rows.Scan(&t.ID, &t.AgentID, &t.AgentName, &t.Title, &t.Body, &tagsStr, &pinned, &archived, &t.CreatedAt, &t.UpdatedAt); err != nil {
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
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to iterate threads"})
		return
	}

	// Set pagination headers
	w.Header().Set("X-Total-Count", strconv.Itoa(totalCount))
	w.Header().Set("X-Page", strconv.Itoa(page))
	w.Header().Set("X-Per-Page", strconv.Itoa(perPage))

	writeJSON(w, http.StatusOK, threads)
}

// handleGetThread retrieves a single thread with its replies and status tags.
func handleGetThread(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	agent := AgentFromContext(r.Context())
	if agent == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	threadID := r.PathValue("id")
	if threadID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing thread id"})
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
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "thread not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to query thread"})
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to query replies"})
		return
	}
	defer replyRows.Close()

	replies := []Reply{}
	for replyRows.Next() {
		var reply Reply
		if err := replyRows.Scan(&reply.ID, &reply.ThreadID, &reply.AgentID, &reply.AgentName, &reply.Body, &reply.CreatedAt, &reply.UpdatedAt); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to scan reply"})
			return
		}
		reply.Statuses = []StatusTag{}
		replies = append(replies, reply)
	}
	if err := replyRows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to iterate replies"})
		return
	}

	// Query status tags for this thread AND its replies
	statusRows, err := db.Query(
		`SELECT s.id, s.thread_id, s.reply_id, s.agent_id, a.name, s.tag, s.reference_id, s.created_at
		FROM status_tags s
		JOIN agents a ON s.agent_id = a.id
		WHERE s.thread_id = ? OR s.reply_id IN (SELECT r.id FROM replies r WHERE r.thread_id = ?)
		ORDER BY s.created_at ASC`, threadID, threadID,
	)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to query status tags"})
		return
	}
	defer statusRows.Close()

	threadStatuses := []StatusTag{}
	replyStatusMap := make(map[string][]StatusTag)
	for statusRows.Next() {
		var st StatusTag
		if err := statusRows.Scan(&st.ID, &st.ThreadID, &st.ReplyID, &st.AgentID, &st.AgentName, &st.Tag, &st.ReferenceID, &st.CreatedAt); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to scan status tag"})
			return
		}
		if st.ReplyID != nil {
			replyStatusMap[*st.ReplyID] = append(replyStatusMap[*st.ReplyID], st)
		} else {
			threadStatuses = append(threadStatuses, st)
		}
	}
	if err := statusRows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to iterate status tags"})
		return
	}

	// Attach statuses to replies
	for i := range replies {
		if statuses, ok := replyStatusMap[replies[i].ID]; ok {
			replies[i].Statuses = statuses
		}
	}

	t.Replies = replies
	t.Statuses = threadStatuses

	writeJSON(w, http.StatusOK, t)
}

// handleUpdateThread updates an existing thread owned by the requesting agent.
func handleUpdateThread(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	agent := AgentFromContext(r.Context())
	if agent == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	threadID := r.PathValue("id")
	if threadID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing thread id"})
		return
	}

	// Check if thread exists and verify ownership
	var ownerID string
	err := db.QueryRow("SELECT agent_id FROM threads WHERE id = ?", threadID).Scan(&ownerID)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "thread not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to query thread"})
		return
	}
	if ownerID != agent.ID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "you can only update your own threads"})
		return
	}

	// Parse optional fields
	var input struct {
		Title *string  `json:"title"`
		Body  *string  `json:"body"`
		Tags  []string `json:"tags"`
	}
	if err := readJSON(r, &input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	// Build dynamic update
	var setClauses []string
	var args []interface{}

	if input.Title != nil {
		if *input.Title == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title cannot be empty"})
			return
		}
		setClauses = append(setClauses, "title = ?")
		args = append(args, *input.Title)
	}
	if input.Body != nil {
		if *input.Body == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body cannot be empty"})
			return
		}
		setClauses = append(setClauses, "body = ?")
		args = append(args, *input.Body)
	}
	if input.Tags != nil {
		tagsJSON, err := json.Marshal(input.Tags)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to marshal tags"})
			return
		}
		setClauses = append(setClauses, "tags = ?")
		args = append(args, string(tagsJSON))
	}

	if len(setClauses) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no fields to update"})
		return
	}

	now := time.Now()
	setClauses = append(setClauses, "updated_at = ?")
	args = append(args, now)
	args = append(args, threadID)

	query := fmt.Sprintf("UPDATE threads SET %s WHERE id = ?", strings.Join(setClauses, ", "))
	if _, err := db.Exec(query, args...); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update thread"})
		return
	}

	// Return the updated thread
	var t Thread
	var tagsStr string
	var pinned, archived int
	err = db.QueryRow(
		`SELECT t.id, t.agent_id, a.name, t.title, t.body, t.tags, t.pinned, t.archived, t.created_at, t.updated_at
		FROM threads t
		JOIN agents a ON t.agent_id = a.id
		WHERE t.id = ?`, threadID,
	).Scan(&t.ID, &t.AgentID, &t.AgentName, &t.Title, &t.Body, &tagsStr, &pinned, &archived, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to retrieve updated thread"})
		return
	}
	t.Pinned = pinned != 0
	t.Archived = archived != 0
	if err := json.Unmarshal([]byte(tagsStr), &t.Tags); err != nil {
		t.Tags = []string{}
	}

	writeJSON(w, http.StatusOK, t)
}

// handleDeleteThread deletes a thread owned by the requesting agent.
func handleDeleteThread(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	agent := AgentFromContext(r.Context())
	if agent == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	threadID := r.PathValue("id")
	if threadID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing thread id"})
		return
	}

	// Check if thread exists and verify ownership
	var ownerID string
	err := db.QueryRow("SELECT agent_id FROM threads WHERE id = ?", threadID).Scan(&ownerID)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "thread not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to query thread"})
		return
	}
	if ownerID != agent.ID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "you can only delete your own threads"})
		return
	}

	// Delete thread (cascades to replies and status_tags)
	if _, err := db.Exec("DELETE FROM threads WHERE id = ?", threadID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to delete thread"})
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// Valid status tags that can be applied to threads and replies.
var validStatusTags = map[string]bool{
	"acknowledged": true,
	"depends-on":   true,
	"blocked":      true,
	"resolved":     true,
	"in-progress":  true,
	"needs-review": true,
}

// handleCreateReply creates a new reply on a thread.
func handleCreateReply(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	agent := AgentFromContext(r.Context())
	if agent == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	threadID := r.PathValue("id")
	if threadID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing thread id"})
		return
	}

	// Verify thread exists
	var exists bool
	err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM threads WHERE id = ?)", threadID).Scan(&exists)
	if err != nil || !exists {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "thread not found"})
		return
	}

	var input struct {
		Body string `json:"body"`
	}
	if err := readJSON(r, &input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if input.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body is required"})
		return
	}

	id := uuid.New().String()
	now := time.Now()

	_, err = db.Exec(
		`INSERT INTO replies (id, thread_id, agent_id, body, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, threadID, agent.ID, input.Body, now, now,
	)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create reply"})
		return
	}

	reply := Reply{
		ID:        id,
		ThreadID:  threadID,
		AgentID:   agent.ID,
		AgentName: agent.Name,
		Body:      input.Body,
		CreatedAt: now,
		UpdatedAt: now,
		Statuses:  []StatusTag{},
	}

	writeJSON(w, http.StatusCreated, reply)
}

// handleUpdateReply updates a reply owned by the requesting agent.
func handleUpdateReply(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	agent := AgentFromContext(r.Context())
	if agent == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	replyID := r.PathValue("id")
	if replyID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing reply id"})
		return
	}

	// Check if reply exists and verify ownership
	var ownerID string
	err := db.QueryRow("SELECT agent_id FROM replies WHERE id = ?", replyID).Scan(&ownerID)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "reply not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to query reply"})
		return
	}
	if ownerID != agent.ID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "you can only update your own replies"})
		return
	}

	var input struct {
		Body string `json:"body"`
	}
	if err := readJSON(r, &input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if input.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body is required"})
		return
	}

	now := time.Now()
	_, err = db.Exec("UPDATE replies SET body = ?, updated_at = ? WHERE id = ?", input.Body, now, replyID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to update reply"})
		return
	}

	// Return the updated reply
	var reply Reply
	err = db.QueryRow(
		`SELECT r.id, r.thread_id, r.agent_id, a.name, r.body, r.created_at, r.updated_at
		FROM replies r
		JOIN agents a ON r.agent_id = a.id
		WHERE r.id = ?`, replyID,
	).Scan(&reply.ID, &reply.ThreadID, &reply.AgentID, &reply.AgentName, &reply.Body, &reply.CreatedAt, &reply.UpdatedAt)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to retrieve updated reply"})
		return
	}
	reply.Statuses = []StatusTag{}

	writeJSON(w, http.StatusOK, reply)
}

// handleDeleteReply deletes a reply owned by the requesting agent.
func handleDeleteReply(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	agent := AgentFromContext(r.Context())
	if agent == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	replyID := r.PathValue("id")
	if replyID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing reply id"})
		return
	}

	// Check if reply exists and verify ownership
	var ownerID string
	err := db.QueryRow("SELECT agent_id FROM replies WHERE id = ?", replyID).Scan(&ownerID)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "reply not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to query reply"})
		return
	}
	if ownerID != agent.ID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "you can only delete your own replies"})
		return
	}

	if _, err := db.Exec("DELETE FROM replies WHERE id = ?", replyID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to delete reply"})
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleCreateThreadStatus adds a status tag to a thread.
func handleCreateThreadStatus(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	agent := AgentFromContext(r.Context())
	if agent == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	threadID := r.PathValue("id")
	if threadID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing thread id"})
		return
	}

	// Verify thread exists
	var exists bool
	err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM threads WHERE id = ?)", threadID).Scan(&exists)
	if err != nil || !exists {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "thread not found"})
		return
	}

	var input struct {
		Tag         string  `json:"tag"`
		ReferenceID *string `json:"reference_id"`
	}
	if err := readJSON(r, &input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	if !validStatusTags[input.Tag] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid status tag"})
		return
	}

	id := uuid.New().String()
	now := time.Now()

	_, err = db.Exec(
		`INSERT INTO status_tags (id, thread_id, reply_id, agent_id, tag, reference_id, created_at) VALUES (?, ?, NULL, ?, ?, ?, ?)`,
		id, threadID, agent.ID, input.Tag, input.ReferenceID, now,
	)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create status tag"})
		return
	}

	st := StatusTag{
		ID:          id,
		ThreadID:    &threadID,
		AgentID:     agent.ID,
		AgentName:   agent.Name,
		Tag:         input.Tag,
		ReferenceID: input.ReferenceID,
		CreatedAt:   now,
	}

	writeJSON(w, http.StatusCreated, st)
}

// handleCreateReplyStatus adds a status tag to a reply.
func handleCreateReplyStatus(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	agent := AgentFromContext(r.Context())
	if agent == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	replyID := r.PathValue("id")
	if replyID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing reply id"})
		return
	}

	// Verify reply exists
	var exists bool
	err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM replies WHERE id = ?)", replyID).Scan(&exists)
	if err != nil || !exists {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "reply not found"})
		return
	}

	var input struct {
		Tag         string  `json:"tag"`
		ReferenceID *string `json:"reference_id"`
	}
	if err := readJSON(r, &input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	if !validStatusTags[input.Tag] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid status tag"})
		return
	}

	id := uuid.New().String()
	now := time.Now()

	_, err = db.Exec(
		`INSERT INTO status_tags (id, thread_id, reply_id, agent_id, tag, reference_id, created_at) VALUES (?, NULL, ?, ?, ?, ?, ?)`,
		id, replyID, agent.ID, input.Tag, input.ReferenceID, now,
	)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to create status tag"})
		return
	}

	st := StatusTag{
		ID:          id,
		ReplyID:     &replyID,
		AgentID:     agent.ID,
		AgentName:   agent.Name,
		Tag:         input.Tag,
		ReferenceID: input.ReferenceID,
		CreatedAt:   now,
	}

	writeJSON(w, http.StatusCreated, st)
}

// handleDeleteStatus deletes a status tag owned by the requesting agent.
func handleDeleteStatus(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	agent := AgentFromContext(r.Context())
	if agent == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	statusID := r.PathValue("id")
	if statusID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing status tag id"})
		return
	}

	// Check if status tag exists and verify ownership
	var ownerID string
	err := db.QueryRow("SELECT agent_id FROM status_tags WHERE id = ?", statusID).Scan(&ownerID)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "status tag not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to query status tag"})
		return
	}
	if ownerID != agent.ID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "you can only delete your own status tags"})
		return
	}

	if _, err := db.Exec("DELETE FROM status_tags WHERE id = ?", statusID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to delete status tag"})
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleQueryStatus queries status tags by tag value with context previews.
func handleQueryStatus(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	agent := AgentFromContext(r.Context())
	if agent == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	tag := r.URL.Query().Get("tag")
	if tag == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tag query parameter is required"})
		return
	}

	rows, err := db.Query(
		`SELECT s.id, s.thread_id, s.reply_id, s.agent_id, a.name, s.tag, s.reference_id, s.created_at,
			COALESCE(t.title, ''),
			COALESCE(
				CASE WHEN s.reply_id IS NOT NULL THEN
					CASE WHEN LENGTH(rep.body) > 100 THEN SUBSTR(rep.body, 1, 100) || '...' ELSE rep.body END
				ELSE
					CASE WHEN LENGTH(t.body) > 100 THEN SUBSTR(t.body, 1, 100) || '...' ELSE t.body END
				END,
			'')
		FROM status_tags s
		JOIN agents a ON s.agent_id = a.id
		LEFT JOIN threads t ON s.thread_id = t.id
		LEFT JOIN replies rep ON s.reply_id = rep.id
		WHERE s.tag = ?
		ORDER BY s.created_at DESC`, tag,
	)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to query status tags"})
		return
	}
	defer rows.Close()

	type StatusTagWithPreview struct {
		StatusTag
		Preview string `json:"preview"`
	}

	results := []StatusTagWithPreview{}
	for rows.Next() {
		var st StatusTagWithPreview
		var title string
		if err := rows.Scan(&st.ID, &st.ThreadID, &st.ReplyID, &st.AgentID, &st.AgentName, &st.Tag, &st.ReferenceID, &st.CreatedAt, &title, &st.Preview); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to scan status tag"})
			return
		}
		// For thread statuses, use the thread title as preview
		if st.ThreadID != nil && st.ReplyID == nil && title != "" {
			st.Preview = title
		}
		results = append(results, st)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to iterate status tags"})
		return
	}

	writeJSON(w, http.StatusOK, results)
}
