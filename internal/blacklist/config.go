package blacklist

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is the runtime configuration. All fields can be set through
// environment variables; see LoadConfig.
type Config struct {
	Addr          string
	DBPath        string
	APIToken      string
	CORSOrigins   []string
	MaxCheckItems int
	MaxBodyBytes  int64
	TrustProxy    bool
}

// LoadConfig reads configuration from the environment. Unknown or malformed
// numeric values are rejected instead of silently falling back to defaults.
func LoadConfig() (Config, error) {
	cfg := Config{
		Addr:         envString("ADDR", DefaultAddr),
		DBPath:       envString("DB_PATH", DefaultDBPath),
		APIToken:     envString("API_TOKEN", ""),
		CORSOrigins:  envList("CORS_ORIGINS"),
		TrustProxy:   false,
		MaxBodyBytes: defaultMaxBodyBytes,
	}

	maxCheck, err := envInt("MAX_CHECK_ITEMS", defaultMaxCheckItems)
	if err != nil {
		return Config{}, err
	}
	if maxCheck < 1 || maxCheck > 100000 {
		return Config{}, fmt.Errorf("MAX_CHECK_ITEMS must be between 1 and 100000")
	}
	cfg.MaxCheckItems = maxCheck

	maxBody, err := envInt64("MAX_BODY_BYTES", defaultMaxBodyBytes)
	if err != nil {
		return Config{}, err
	}
	if maxBody < 1024 {
		return Config{}, fmt.Errorf("MAX_BODY_BYTES must be at least 1024")
	}
	cfg.MaxBodyBytes = maxBody

	trustProxy, err := envBool("TRUST_PROXY", false)
	if err != nil {
		return Config{}, err
	}
	cfg.TrustProxy = trustProxy

	return cfg, nil
}

func envString(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envList(key string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func envInt(key string, def int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %q", key, raw)
	}
	return v, nil
}

func envInt64(key string, def int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %q", key, raw)
	}
	return v, nil
}

func envBool(key string, def bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("invalid %s: %q", key, raw)
	}
	return v, nil
}
