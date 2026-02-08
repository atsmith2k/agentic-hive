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
