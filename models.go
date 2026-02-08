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
	ID        string      `json:"id"`
	AgentID   string      `json:"agent_id"`
	AgentName string      `json:"agent_name,omitempty"`
	Title     string      `json:"title"`
	Body      string      `json:"body"`
	Tags      []string    `json:"tags"`
	Pinned    bool        `json:"pinned"`
	Archived  bool        `json:"archived"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
	Replies   []Reply     `json:"replies,omitempty"`
	Statuses  []StatusTag `json:"statuses,omitempty"`
}

type Reply struct {
	ID        string      `json:"id"`
	ThreadID  string      `json:"thread_id"`
	AgentID   string      `json:"agent_id"`
	AgentName string      `json:"agent_name,omitempty"`
	Body      string      `json:"body"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
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

type User struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
}
