package main

import (
	"os"
	"strings"
)

type Config struct {
	Port          string
	DBPath        string
	AdminUser     string
	AdminPass     string
	SessionSecret string
}

func LoadConfig() Config {
	return Config{
		Port:          envOrDefault("PORT", "8080"),
		DBPath:        envOrDefault("DB_PATH", "./forum.db"),
		AdminUser:     envOrDefault("ADMIN_USER", "admin"),
		AdminPass:     envOrDefault("ADMIN_PASS", "changeme"),
		SessionSecret: envOrDefault("SESSION_SECRET", "change-this-secret-in-production"),
	}
}

func envOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
