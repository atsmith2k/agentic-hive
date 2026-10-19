package main

import (
	"strings"
	"testing"
)

const goodSecret = "0123456789abcdef0123456789abcdef"

func setEnv(t *testing.T, adminPass, sessionSecret, devMode string) {
	t.Helper()
	t.Setenv("ADMIN_PASS", adminPass)
	t.Setenv("SESSION_SECRET", sessionSecret)
	t.Setenv("DEV_MODE", devMode)
}

func TestLoadConfigRejectsInsecureSettings(t *testing.T) {
	cases := []struct {
		name, adminPass, sessionSecret, wantErr string
	}{
		{"both unset", "", "", "ADMIN_PASS is not set"},
		{"secret unset", "a-real-password", "", "SESSION_SECRET is not set"},
		{"published admin password", "changeme", goodSecret, "ADMIN_PASS is the published default"},
		{"published session secret", "a-real-password", "change-this-secret-in-production", "SESSION_SECRET is the published default"},
		{"short session secret", "a-real-password", "too-short", "at least 32 characters"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, tc.adminPass, tc.sessionSecret, "")
			_, err := LoadConfig()
			if err == nil {
				t.Fatalf("LoadConfig() succeeded, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("LoadConfig() error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadConfigAcceptsExplicitSettings(t *testing.T) {
	setEnv(t, "a-real-password", goodSecret, "")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.AdminPass != "a-real-password" || cfg.SessionSecret != goodSecret {
		t.Fatalf("LoadConfig() did not keep the configured values")
	}
}

func TestLoadConfigDevModeGeneratesRandomSecrets(t *testing.T) {
	setEnv(t, "", "", "1")
	first, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() in DEV_MODE error = %v", err)
	}
	second, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() in DEV_MODE error = %v", err)
	}
	if first.AdminPass == "" || first.AdminPass == second.AdminPass {
		t.Fatalf("DEV_MODE admin password should be random and non-empty")
	}
	if len(first.SessionSecret) < 32 || first.SessionSecret == second.SessionSecret {
		t.Fatalf("DEV_MODE session secret should be random and at least 32 characters")
	}
}

func TestLoadConfigDevModeStillRejectsPublishedDefaults(t *testing.T) {
	setEnv(t, "changeme", "", "1")
	if _, err := LoadConfig(); err == nil {
		t.Fatalf("LoadConfig() accepted ADMIN_PASS=changeme in DEV_MODE")
	}
}
