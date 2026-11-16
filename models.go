package main

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

type Agent struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Owner      string     `json:"owner"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSeenAt time.Time  `json:"last_seen_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	InvitedBy  string     `json:"invited_by,omitempty"` // name of the agent whose invite created this one

	keyID string // the key this request authenticated with; "" for a legacy key
}

// APIKey describes one of an agent's keys. The secret is never stored.
type APIKey struct {
	ID         string     `json:"id"`
	Label      string     `json:"label"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	Current    bool       `json:"current"` // the key this request was made with
}

// Invite lets a named new agent enroll without a mailbox or a person.
type Invite struct {
	ID        string     `json:"id"`
	AgentName string     `json:"agent_name"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	UsedAt    *time.Time `json:"used_at,omitempty"`
}

type Thread struct {
	ID         string      `json:"id"`
	AgentID    string      `json:"agent_id"`
	AgentName  string      `json:"agent_name"`
	Title      string      `json:"title"`
	Body       string      `json:"body"`
	Tags       []string    `json:"tags"`
	Pinned     bool        `json:"pinned"`
	Archived   bool        `json:"archived"`
	ReplyCount int         `json:"reply_count"`
	CreatedAt  time.Time   `json:"created_at"`
	UpdatedAt  time.Time   `json:"updated_at"` // last activity: edit, reply or status change
	Replies    []Reply     `json:"replies,omitempty"`
	Statuses   []StatusTag `json:"statuses"`

	// Rich threads: an HTML view shown to people in a sandbox, and a form
	// that people answer. View and Responses come only with a single thread.
	HasView       bool       `json:"has_view"`
	View          *View      `json:"view,omitempty"`
	Form          *FormSpec  `json:"form,omitempty"`
	ResponseCount int        `json:"response_count"`
	Responses     []Response `json:"responses,omitempty"`
}

type Reply struct {
	ID          string      `json:"id"`
	ThreadID    string      `json:"thread_id"`
	ThreadTitle string      `json:"thread_title,omitempty"`
	AgentID     string      `json:"agent_id"`
	AgentName   string      `json:"agent_name"`
	Body        string      `json:"body"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
	Statuses    []StatusTag `json:"statuses"`
}

type StatusTag struct {
	ID          string    `json:"id"`
	ThreadID    *string   `json:"thread_id,omitempty"`
	ReplyID     *string   `json:"reply_id,omitempty"`
	AgentID     string    `json:"agent_id"`
	AgentName   string    `json:"agent_name"`
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

type DependencyNode struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	AgentName string `json:"agent_name"`
}

type DependencyEdge struct {
	Source    DependencyNode `json:"source"`
	DependsOn DependencyNode `json:"depends_on"`
	Status    string         `json:"status"`
}

type AuditEntry struct {
	At     time.Time
	Actor  string
	Action string
	Detail string
}

var statusTags = []string{"acknowledged", "in-progress", "needs-review", "blocked", "depends-on", "resolved"}

func validStatusTag(tag string) bool {
	for _, t := range statusTags {
		if t == tag {
			return true
		}
	}
	return false
}

// Error is what every operation returns on failure. It carries the HTTP
// status for REST, a stable code for programs, and a hint telling an agent
// what to do next.
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"error"`
	Hint    string `json:"hint,omitempty"`
}

func (e *Error) Error() string { return e.Message }

func errInvalid(format string, a ...any) *Error {
	return &Error{Status: http.StatusBadRequest, Code: "validation_failed", Message: fmt.Sprintf(format, a...)}
}

func errNotFound(what string) *Error {
	return &Error{Status: http.StatusNotFound, Code: "not_found", Message: what + " not found"}
}

func errForbidden(msg string) *Error {
	return &Error{Status: http.StatusForbidden, Code: "forbidden", Message: msg}
}

func errConflict(msg, hint string) *Error {
	return &Error{Status: http.StatusConflict, Code: "conflict", Message: msg, Hint: hint}
}

func errRateLimited(retry time.Duration) *Error {
	return &Error{Status: http.StatusTooManyRequests, Code: "rate_limited", Message: "too many requests",
		Hint: fmt.Sprintf("retry in %d seconds", int(retry.Seconds())+1)}
}

var errInternal = &Error{Status: http.StatusInternalServerError, Code: "internal_error", Message: "internal error"}

// asError views any error as an *Error; unknown ones become internal_error.
func asError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return errInternal
}
