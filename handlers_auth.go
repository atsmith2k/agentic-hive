package main

import (
	"database/sql"
	"html/template"
	"log"
	"net/http"

	"golang.org/x/crypto/bcrypt"
)

// userLoginTemplate is the standalone login template for users.
var userLoginTemplate *template.Template

func init() {
	var err error
	loginPath := "templates/login.html"
	userLoginTemplate, err = template.New("").Funcs(templateFuncs).ParseFS(templateFS, loginPath)
	if err != nil {
		log.Fatalf("failed to parse user login template: %v", err)
	}
}

// handleLogin renders the user login page (GET).
func handleLogin(cfg Config, w http.ResponseWriter, r *http.Request) {
	// If already logged in, redirect to dashboard
	cookie, err := r.Cookie("user_session")
	if err == nil {
		if _, valid := ValidateUserSessionToken(cookie.Value, cfg.SessionSecret); valid {
			http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
			return
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := userLoginTemplate.ExecuteTemplate(w, "user-login", map[string]interface{}{}); err != nil {
		log.Printf("user login template error: %v", err)
		http.Error(w, "template rendering error", http.StatusInternalServerError)
	}
}

// handleLoginPost processes the user login form (POST).
func handleLoginPost(db *sql.DB, cfg Config, w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")

	// Look up user
	var user User
	err := db.QueryRow(
		"SELECT id, username, password_hash, created_at FROM users WHERE username = ?",
		username,
	).Scan(&user.ID, &user.Username, &user.PasswordHash, &user.CreatedAt)

	if err != nil || bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)) != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := userLoginTemplate.ExecuteTemplate(w, "user-login", map[string]interface{}{
			"Error": "Invalid username or password.",
		}); err != nil {
			log.Printf("user login template error: %v", err)
			http.Error(w, "template rendering error", http.StatusInternalServerError)
		}
		return
	}

	// Create session token
	token := CreateUserSessionToken(user.ID, cfg.SessionSecret)
	http.SetCookie(w, &http.Cookie{
		Name:     "user_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

// handleLogout clears the user session and redirects to login.
func handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "user_session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
