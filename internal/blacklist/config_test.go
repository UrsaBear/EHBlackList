package blacklist

import "testing"

var configEnvKeys = []string{
	"ADDR", "DB_PATH", "API_TOKEN", "CORS_ORIGINS",
	"MAX_CHECK_ITEMS", "MAX_BODY_BYTES", "TRUST_PROXY",
}

func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range configEnvKeys {
		t.Setenv(key, "")
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	clearConfigEnv(t)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != DefaultAddr || cfg.DBPath != DefaultDBPath {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.APIToken != "" || len(cfg.CORSOrigins) != 0 || cfg.TrustProxy {
		t.Fatalf("unexpected security defaults: %+v", cfg)
	}
	if cfg.MaxCheckItems != defaultMaxCheckItems || cfg.MaxBodyBytes != defaultMaxBodyBytes {
		t.Fatalf("unexpected limit defaults: %+v", cfg)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("ADDR", ":9999")
	t.Setenv("DB_PATH", "C:/tmp/blacklist.db")
	t.Setenv("API_TOKEN", "  secret  ")
	t.Setenv("CORS_ORIGINS", "https://a.example, , https://b.example")
	t.Setenv("MAX_CHECK_ITEMS", "42")
	t.Setenv("MAX_BODY_BYTES", "2048")
	t.Setenv("TRUST_PROXY", "true")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":9999" || cfg.DBPath != "C:/tmp/blacklist.db" {
		t.Fatalf("unexpected address defaults: %+v", cfg)
	}
	if cfg.APIToken != "secret" {
		t.Fatalf("API_TOKEN = %q, want secret", cfg.APIToken)
	}
	if len(cfg.CORSOrigins) != 2 || cfg.CORSOrigins[0] != "https://a.example" || cfg.CORSOrigins[1] != "https://b.example" {
		t.Fatalf("CORS origins = %#v", cfg.CORSOrigins)
	}
	if cfg.MaxCheckItems != 42 || cfg.MaxBodyBytes != 2048 || !cfg.TrustProxy {
		t.Fatalf("limits = %+v", cfg)
	}
}

func TestLoadConfigRejectsInvalidValues(t *testing.T) {
	cases := []struct {
		key   string
		value string
	}{
		{"MAX_CHECK_ITEMS", "abc"},
		{"MAX_CHECK_ITEMS", "0"},
		{"MAX_CHECK_ITEMS", "100001"},
		{"MAX_BODY_BYTES", "abc"},
		{"MAX_BODY_BYTES", "100"},
		{"TRUST_PROXY", "maybe"},
	}
	for _, tc := range cases {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv(tc.key, tc.value)
			if _, err := LoadConfig(); err == nil {
				t.Fatalf("expected error for %s=%s", tc.key, tc.value)
			}
		})
	}
}
