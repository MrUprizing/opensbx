package config

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const DefaultAddr = "127.0.0.1:18089"

type Config struct {
	Runtime  string
	Addr     string
	APIKey   string
	LogFile  string
	DataDir  string
	LegacyDB string
}

func DefaultDataDir() string {
	if v := os.Getenv("OPENSBX_DATA_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}
	return filepath.Join(home, ".local", "share", "opensbx")
}

func Load() *Config {
	cfg, err := Parse(os.Args[1:])
	if err != nil {
		panic(err)
	}
	return cfg
}

// Parse reads server options without mutating the process-global flag set.
func Parse(args []string) (*Config, error) {
	for _, key := range []string{"PROXY_ADDR", "BASE_DOMAIN"} {
		if _, set := os.LookupEnv(key); set {
			return nil, fmt.Errorf("%s is no longer supported: use ADDR=%s; sandbox URLs share that port", key, DefaultAddr)
		}
	}
	for _, arg := range args {
		key := strings.SplitN(strings.TrimLeft(arg, "-"), "=", 2)[0]
		if key == "proxy-addr" || key == "base-domain" {
			return nil, fmt.Errorf("legacy proxy/domain options were removed: use -addr %s", DefaultAddr)
		}
	}
	fs := flag.NewFlagSet("opensbx", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	addr := fs.String("addr", envOrDefault("ADDR", DefaultAddr), "Loopback HTTP address for API, MCP and sandbox URLs")
	logFile := fs.String("log-file", envOrDefault("LOG_FILE", "opensbx.log"), "Path to log file")
	runtime := fs.String("runtime", "", "Sandbox runtime: docker or container")
	dataDir := fs.String("data-dir", DefaultDataDir(), "OpenSBX local data directory")
	legacyDB := fs.String("legacy-db", "", "Explicit legacy execution database path; back it up first and select its original runtime")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() != 0 {
		return nil, fmt.Errorf("unexpected argument %q; run opensbx -h for available commands", fs.Arg(0))
	}
	if err := ValidateAddr(*addr); err != nil {
		return nil, err
	}
	return &Config{Runtime: *runtime, Addr: *addr, APIKey: os.Getenv("API_KEY"), LogFile: normalizeLogFile(*logFile), DataDir: *dataDir, LegacyDB: *legacyDB}, nil
}

func ValidateAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("listen address must be a loopback IP and port: %w", err)
	}
	ip := net.ParseIP(host)
	n, err := strconv.Atoi(port)
	if ip == nil || (host != "127.0.0.1" && host != "::1") || err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("listen address %q must use 127.0.0.1 or ::1 and port 0..65535 for localhost routing", addr)
	}
	return nil
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func normalizeLogFile(raw string) string {
	if v := strings.TrimSpace(raw); v != "" {
		return v
	}
	return "opensbx.log"
}
