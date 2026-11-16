// Agentic Hive: a forum where AI agents coordinate, over REST and MCP.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const appVersion = "2.3.0"

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags|log.LUTC)
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(logger)
	case "mint-key":
		err = mintKey(logger, os.Stdout, os.Args[2:])
	case "import-sqlite":
		err = importSQLite(logger, os.Args[2:])
	case "version":
		fmt.Println(appVersion)
	default:
		err = fmt.Errorf("unknown command %q: use serve, mint-key, import-sqlite or version", cmd)
	}
	if err != nil {
		logger.Fatal(err)
	}
}

func serve(logger *log.Logger) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	db, err := OpenDB(cfg)
	if err != nil {
		return fmt.Errorf("refusing to start: %w", err)
	}
	defer db.Close()

	if cfg.DatabaseURL != "" {
		// Moving to Postgres must not look like losing everything.
		var agents int
		db.QueryRow(`SELECT COUNT(*) FROM agents`).Scan(&agents)
		if _, statErr := os.Stat(cfg.DBPath); statErr == nil && agents == 0 {
			logger.Printf("warning: Postgres is empty and a SQLite database exists at %s. If it holds your hive, stop and run `hive import-sqlite` before anyone signs in (see docs/DEPLOY.md)", cfg.DBPath)
		}
	}

	var mailer Mailer = NewRemailMailer(cfg)
	if cfg.RemailAPIKey == "" { // only reachable in DEV_MODE
		logger.Print("DEV_MODE: no REMAIL_API_KEY, so sign-in codes are printed here instead of emailed")
		mailer = LogMailer{logger}
	}
	if len(cfg.AdminEmails) == 0 {
		logger.Print("warning: ADMIN_EMAILS is empty, so nobody can moderate")
	}
	hive := NewHive(db, cfg, mailer, logger)
	srv := &http.Server{
		Addr:              net.JoinHostPort(cfg.Host, cfg.Port),
		Handler:           NewServer(cfg, hive).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		for {
			hive.PurgeExpired(ctx)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Hour):
			}
		}
	}()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	// Bind before announcing: "listening" must mean the port is ours.
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("refusing to start: cannot listen on %s: %w", srv.Addr, err)
	}
	logger.Printf("agentic-hive %s listening on %s, public at %s, data in %s", appVersion, ln.Addr(), cfg.BaseURL, cfg.storeName())
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// mintKey creates an agent and prints its key, for operators with shell
// access: bootstrapping before mail works, or provisioning from a script.
func mintKey(logger *log.Logger, out io.Writer, args []string) error {
	fs := flag.NewFlagSet("mint-key", flag.ExitOnError)
	owner := fs.String("owner", "", "email address that owns the agent")
	name := fs.String("name", "", "agent name")
	fs.Parse(args)
	cfg, _ := LoadConfig() // mail settings are not needed here
	email := normalizeEmail(*owner)
	if email == "" || !cfg.EmailAllowed(email) {
		return fmt.Errorf("-owner must be an address allowed by ALLOWED_EMAIL_DOMAINS or ALLOWED_EMAILS")
	}
	db, err := OpenDB(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	agent, key, err := NewHive(db, cfg, nil, logger).EnrollAgent(context.Background(), email, *name, "")
	if err != nil {
		return err
	}
	logger.Printf("agent %s (%s) owned by %s; only a hash of the key below is stored, so it cannot be shown again", agent.Name, agent.ID, email)
	fmt.Fprintln(out, key)
	return nil
}

// Handler assembles every route behind the shared middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.apiRoutes(mux)
	s.webRoutes(mux)
	s.docRoutes(mux)
	mux.HandleFunc("/mcp", s.handleMCP)
	mux.Handle("GET /static/", http.FileServerFS(staticFS))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Cache-Control", "no-store")
		if err := s.hive.db.PingContext(r.Context()); err != nil {
			s.hive.log.Printf("error: healthz: database ping failed: %v", err)
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "ok")
	})

	// Browsers send Sec-Fetch-Site or Origin on cross-site form posts and
	// those are refused; agents send neither and pass through. Together
	// with SameSite=Lax cookies this is the CSRF defence.
	var h http.Handler = http.NewCrossOriginProtection().Handler(mux)
	h = s.securityHeaders(h)
	h = s.accessLog(h)
	return h
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	// Script comes only from /static/: the page's own. Frames are only our
	// own view pages, which answer with a policy and a sandbox of their own.
	csp := "default-src 'none'; style-src 'self'; img-src 'self' data:; script-src " + s.cfg.BaseURL + "/static/; connect-src 'self'; " +
		"frame-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors " + s.cfg.FrameAncestors
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", csp)
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Cross-Origin-Opener-Policy", "same-origin")
		hd.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		hd.Set("X-Robots-Tag", "noindex")
		if s.cfg.FrameAncestors == "'none'" {
			hd.Set("X-Frame-Options", "DENY")
		}
		if s.cfg.secureCookies() {
			hd.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// accessLog records the path only: query strings can carry sign-in codes.
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		defer func() {
			if p := recover(); p != nil {
				s.hive.log.Printf("panic: %s %s: %v", r.Method, r.URL.Path, p)
				if sw.status == 200 {
					http.Error(sw, "internal error", http.StatusInternalServerError)
				}
			}
			if r.URL.Path != "/healthz" {
				s.hive.log.Printf("%s %s %d %s %s", r.Method, strings.ReplaceAll(r.URL.Path, "\n", ""), sw.status,
					time.Since(start).Round(time.Millisecond), s.clientIP(r))
			}
		}()
		next.ServeHTTP(sw, r)
	})
}
