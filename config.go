package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Config is everything the server reads from the environment. See .env.example.
type Config struct {
	Host    string // interface to bind; loopback by default because a proxy terminates TLS
	Port    string
	BaseURL string // public origin, e.g. https://hive.johnnycode.ai (no trailing slash)
	DBPath  string
	// DatabaseURL selects Postgres. When it is empty the hive uses the
	// SQLite file at DBPath.
	DatabaseURL string
	DevMode     bool

	AllowedDomains []string // email domains whose mailboxes may sign in
	AllowedEmails  []string // individual addresses allowed in addition to the domains
	AdminEmails    []string

	RemailBaseURL string
	RemailAPIKey  string
	RemailFrom    string

	FrameAncestors string // CSP frame-ancestors source list
	PublicRead     bool   // threads and context readable without a key
	AgentInvites   bool   // agents may invite other agents
	TrustProxy     bool   // take the client address from X-Forwarded-For

	// LiveData says which hosts a thread's view may read through the hive:
	// "declared" (any host the thread names, unless an admin blocked it),
	// "approved" (only hosts an admin approved) or "off".
	LiveData string
	// LibraryCDN is where the libraries a view names are fetched from, once.
	LibraryCDN string
}

func LoadConfig() (Config, error) {
	cfg := Config{
		Host:           envOr("HOST", "127.0.0.1"),
		Port:           envOr("PORT", "8080"),
		BaseURL:        strings.TrimRight(env("BASE_URL"), "/"),
		DBPath:         envOr("DB_PATH", defaultDBPath()),
		DatabaseURL:    env("DATABASE_URL"),
		DevMode:        env("DEV_MODE") == "1",
		AllowedDomains: envList("ALLOWED_EMAIL_DOMAINS", "johnnycode.ai"),
		AllowedEmails:  envList("ALLOWED_EMAILS", ""),
		AdminEmails:    envList("ADMIN_EMAILS", ""),
		RemailBaseURL:  strings.TrimRight(envOr("REMAIL_BASE_URL", "https://remail.foo"), "/"),
		RemailAPIKey:   env("REMAIL_API_KEY"),
		RemailFrom:     env("REMAIL_FROM"),
		FrameAncestors: envOr("FRAME_ANCESTORS", "'none'"),
		PublicRead:     env("PUBLIC_READ") == "1",
		AgentInvites:   envOr("AGENT_INVITES", "1") == "1",
		LiveData:       strings.ToLower(envOr("LIVE_DATA", "declared")),
		LibraryCDN:     strings.TrimRight(envOr("LIBRARY_CDN", "https://cdn.jsdelivr.net/npm"), "/"),
	}
	if v := env("TRUST_PROXY"); v != "" {
		cfg.TrustProxy = v == "1"
	} else {
		cfg.TrustProxy = cfg.Host == "127.0.0.1" || cfg.Host == "localhost" || cfg.Host == "::1"
	}
	if cfg.DevMode && cfg.BaseURL == "" {
		cfg.BaseURL = "http://localhost:" + cfg.Port
	}
	return cfg, cfg.validate()
}

func (c Config) validate() error {
	var problems []string
	u, err := url.Parse(c.BaseURL)
	switch {
	case c.BaseURL == "":
		problems = append(problems, "BASE_URL is not set (the public https origin, used in sign-in links)")
	case err != nil || u.Host == "" || u.Path != "" || u.RawQuery != "":
		problems = append(problems, "BASE_URL must be an origin such as https://hive.example.com")
	case u.Scheme != "https" && !c.DevMode:
		problems = append(problems, "BASE_URL must use https")
	}
	if c.DatabaseURL != "" {
		if u, err := url.Parse(c.DatabaseURL); err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
			problems = append(problems, "DATABASE_URL must be a postgres:// connection string with a host and a database name")
		}
	}
	if c.LiveData != "" && c.LiveData != "declared" && c.LiveData != "approved" && c.LiveData != "off" {
		problems = append(problems, "LIVE_DATA must be declared, approved or off")
	}
	if len(c.AllowedDomains) == 0 && len(c.AllowedEmails) == 0 {
		problems = append(problems, "nobody can sign in: set ALLOWED_EMAIL_DOMAINS or ALLOWED_EMAILS")
	}
	if !c.DevMode {
		if c.RemailAPIKey == "" {
			problems = append(problems, "REMAIL_API_KEY is not set (a sending_only Remail key)")
		}
		if c.RemailFrom == "" {
			problems = append(problems, "REMAIL_FROM is not set (an address on a domain verified in Remail)")
		}
	}
	for _, a := range c.AdminEmails {
		if !c.EmailAllowed(a) {
			problems = append(problems, fmt.Sprintf("ADMIN_EMAILS entry %q is not allowed to sign in", a))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("refusing to start: %s. See .env.example; DEV_MODE=1 relaxes this for local development",
			strings.Join(problems, "; "))
	}
	return nil
}

// EmailAllowed reports whether a normalized address may sign in.
func (c Config) EmailAllowed(email string) bool {
	for _, e := range c.AllowedEmails {
		if e == email {
			return true
		}
	}
	_, domain, ok := strings.Cut(email, "@")
	if !ok {
		return false
	}
	for _, d := range c.AllowedDomains {
		if d == domain {
			return true
		}
	}
	return false
}

func (c Config) IsAdmin(email string) bool {
	for _, a := range c.AdminEmails {
		if a == email {
			return true
		}
	}
	return false
}

// storeName describes where the data lives, without credentials.
func (c Config) storeName() string {
	if c.DatabaseURL == "" {
		return "SQLite file " + c.DBPath
	}
	if u, err := url.Parse(c.DatabaseURL); err == nil {
		return "Postgres database " + strings.TrimPrefix(u.Path, "/") + " on " + u.Host
	}
	return "Postgres"
}

func (c Config) secureCookies() bool { return strings.HasPrefix(c.BaseURL, "https://") }

// defaultDBPath keeps the database outside the checkout, so a deploy that
// replaces the release directory does not replace the data.
func defaultDBPath() string {
	if dir := env("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "agentic-hive", "hive.db")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && home != "/" {
		return filepath.Join(home, ".local", "state", "agentic-hive", "hive.db")
	}
	return filepath.Join("data", "hive.db")
}

func env(key string) string { return strings.TrimSpace(os.Getenv(key)) }

func envOr(key, fallback string) string {
	if v := env(key); v != "" {
		return v
	}
	return fallback
}

// envList reads a comma-separated list, lowercased, without empty entries.
func envList(key, fallback string) []string {
	var out []string
	for _, v := range strings.Split(envOr(key, fallback), ",") {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
			out = append(out, v)
		}
	}
	return out
}
