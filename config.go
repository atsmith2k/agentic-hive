package main

import (
	"crypto/rand"
	"fmt"
	"log"
	"os"
	"strings"
)

// Values that earlier versions shipped as defaults. They are public, so they are always rejected.
const (
	publishedAdminPass     = "changeme"
	publishedSessionSecret = "change-this-secret-in-production"
	minSessionSecretLen    = 32
)

type Config struct {
	Port          string
	DBPath        string
	AdminUser     string
	AdminPass     string
	SessionSecret string
}

// LoadConfig reads configuration from the environment. ADMIN_PASS and
// SESSION_SECRET have no defaults; with DEV_MODE=1, missing values are
// replaced by random ones for local development.
func LoadConfig() (Config, error) {
	cfg := Config{
		Port:          envOrDefault("PORT", "8080"),
		DBPath:        envOrDefault("DB_PATH", "./forum.db"),
		AdminUser:     envOrDefault("ADMIN_USER", "admin"),
		AdminPass:     strings.TrimSpace(os.Getenv("ADMIN_PASS")),
		SessionSecret: strings.TrimSpace(os.Getenv("SESSION_SECRET")),
	}

	if strings.TrimSpace(os.Getenv("DEV_MODE")) == "1" {
		if cfg.AdminPass == "" {
			cfg.AdminPass = rand.Text()
			log.Printf("DEV_MODE: generated admin password for this run: %s", cfg.AdminPass)
		}
		if cfg.SessionSecret == "" {
			cfg.SessionSecret = rand.Text() + rand.Text()
			log.Printf("DEV_MODE: generated a random SESSION_SECRET; sessions end when the server restarts")
		}
	}

	return cfg, cfg.validate()
}

func (c Config) validate() error {
	var problems []string
	switch c.AdminPass {
	case "":
		problems = append(problems, "ADMIN_PASS is not set")
	case publishedAdminPass:
		problems = append(problems, "ADMIN_PASS is the published default")
	}
	switch {
	case c.SessionSecret == "":
		problems = append(problems, "SESSION_SECRET is not set")
	case c.SessionSecret == publishedSessionSecret:
		problems = append(problems, "SESSION_SECRET is the published default")
	case len(c.SessionSecret) < minSessionSecretLen:
		problems = append(problems, fmt.Sprintf("SESSION_SECRET must be at least %d characters", minSessionSecretLen))
	}
	if len(problems) > 0 {
		return fmt.Errorf("refusing to start: %s. Set them in the environment (e.g. SESSION_SECRET=$(openssl rand -hex 32)), or use DEV_MODE=1 for local development",
			strings.Join(problems, "; "))
	}
	return nil
}

func envOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
