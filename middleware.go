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
