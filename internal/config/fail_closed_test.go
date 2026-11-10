package config

import "testing"

// Security audit 2026-11-10 (MF #9): a missing APP_ENV meant "development" (unsigned Meta and
// TikTok webhooks accepted), and REQUIRE_JWT=false opened every route with no check of where it
// ran. Missing settings now mean production, and auth can only be off in development.
func TestFailClosedDefaults(t *testing.T) {
	t.Setenv("POSTGRES_URL", "postgres://x")
	t.Setenv("APP_ENV", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.App.Environment != "production" {
		t.Fatalf("default environment %q, want production", cfg.App.Environment)
	}

	if err := checkFailClosed(&Config{App: AppConfig{Environment: "production"}, Security: SecurityConfig{RequireJWT: false}}); err == nil {
		t.Fatal("auth off in production must refuse to start")
	}
	if err := checkFailClosed(&Config{App: AppConfig{Environment: "development"}, Security: SecurityConfig{RequireJWT: false}}); err != nil {
		t.Fatalf("auth off is allowed in development: %v", err)
	}
	if err := checkFailClosed(&Config{App: AppConfig{Environment: "staging"}, Security: SecurityConfig{RequireJWT: true}}); err != nil {
		t.Fatal(err)
	}
}
