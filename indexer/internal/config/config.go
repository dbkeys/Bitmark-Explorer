package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type RPCConfig struct {
	URL             string
	User            string
	Pass            string
	Timeout         time.Duration
	InsecureTLS     bool
	MaxResponseSize int64
}

type Config struct {
	PostgresDSN    string
	Confirmations  int
	BatchBlocks    int
	PollInterval   time.Duration
	LogVerbose     bool
	BaseUnitFactor int64
	ZMQEndpoint    string // ZMQ hashblock publisher; empty disables real-time mode
	RPC            RPCConfig
}

// Load reads settings.conf (if present) then applies environment variable
// overrides, and returns the validated Config.
func Load() (Config, error) {
	fileVals, _ := parseConfFile("settings.conf")
	return build(makeLookup(fileVals))
}

// LoadFromEnv reads configuration from environment variables only.
func LoadFromEnv() (Config, error) {
	return build(makeLookup(nil))
}

// makeLookup returns a lookup function that checks the environment first,
// then falls back to the provided file values map.
func makeLookup(fileVals map[string]string) func(k, def string) string {
	return func(k, def string) string {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
		if fileVals != nil {
			if v := strings.TrimSpace(fileVals[k]); v != "" {
				return v
			}
		}
		return def
	}
}

func build(get func(k, def string) string) (Config, error) {
	cfg := Config{
		PostgresDSN:    get("PG_DSN", ""),
		Confirmations:  parseInt(get("CONFIRMATIONS", "0"), 0),
		BatchBlocks:    parseInt(get("BATCH_SIZE", "500"), 500),
		PollInterval:   parseDur(get("POLL_INTERVAL", "2s"), 2*time.Second),
		LogVerbose:     parseBool(get("VERBOSE", "false"), false),
		BaseUnitFactor: 100_000_000, // 1e8 base units — not user-configurable
		ZMQEndpoint:    get("ZMQ_ENDPOINT", ""),
		RPC: RPCConfig{
			URL:             get("RPC_URL", ""),
			User:            get("RPC_USER", ""),
			Pass:            get("RPC_PASS", ""),
			Timeout:         parseDur(get("RPC_TIMEOUT", "15s"), 15*time.Second),
			InsecureTLS:     parseBool(get("RPC_INSECURE_TLS", "false"), false),
			MaxResponseSize: parseInt64(get("RPC_MAX_RESPONSE_BYTES", "83886080"), 80<<20),
		},
	}

	if cfg.PostgresDSN == "" {
		return Config{}, errors.New("PG_DSN is required")
	}
	if cfg.RPC.User == "" || cfg.RPC.Pass == "" {
		return Config{}, errors.New("RPC_USER and RPC_PASS are required")
	}
	if cfg.RPC.URL == "" {
		return Config{}, errors.New("RPC_URL is required")
	}
	if !strings.HasPrefix(cfg.RPC.URL, "http://") && !strings.HasPrefix(cfg.RPC.URL, "https://") {
		return Config{}, fmt.Errorf("RPC_URL must start with http:// or https:// (got %q)", cfg.RPC.URL)
	}
	if cfg.Confirmations < 0 {
		cfg.Confirmations = 0
	}
	if cfg.BatchBlocks < 1 {
		cfg.BatchBlocks = 1
	}
	if cfg.PollInterval < 200*time.Millisecond {
		cfg.PollInterval = 200 * time.Millisecond
	}

	return cfg, nil
}

// parseConfFile reads a KEY=VALUE file, ignoring blank lines and # comments.
func parseConfFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	vals := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.IndexByte(line, '=')
		if idx < 1 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		vals[key] = val
	}
	return vals, scanner.Err()
}

func parseInt(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func parseInt64(s string, def int64) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return def
	}
	return n
}

func parseDur(s string, def time.Duration) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}

func parseBool(s string, def bool) bool {
	s = strings.ToLower(s)
	if s == "" {
		return def
	}
	return s == "1" || s == "true" || s == "yes" || s == "y"
}
