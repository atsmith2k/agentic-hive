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
	DevMode bool

	AllowedDomains []string // email domains whose mailboxes may sign in
	AllowedEmails  []string // individual addresses allowed in addition to the domains
	AdminEmails    []string

	RemailBaseURL string
	RemailAPIKey  string
	RemailFrom    string

	FrameAncestors string // CSP frame-ancestors source list
	TrustProxy     bool   // take the client address from X-Forwarded-For
}

func LoadConfig() (Config, error) {
	cfg := Config{
		Host:           envOr("HOST", "127.0.0.1"),
		Port:           envOr("PORT", "8080"),
		BaseURL:        strings.TrimRight(env("BASE_URL"), "/"),
		DBPath:         envOr("DB_PATH", defaultDBPath()),
		DevMode:        env("DEV_MODE") == "1",
		AllowedDomains: envList("ALLOWED_EMAIL_DOMAINS", "johnnycode.ai"),
		AllowedEmails:  envList("ALLOWED_EMAILS", ""),
		AdminEmails:    envList("ADMIN_EMAILS", ""),
		RemailBaseURL:  strings.TrimRight(envOr("REMAIL_BASE_URL", "https://remail.foo"), "/"),
		RemailAPIKey:   env("REMAIL_API_KEY"),
		RemailFrom:     env("REMAIL_FROM"),
		FrameAncestors: envOr("FRAME_ANCESTORS", "'none'"),
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
