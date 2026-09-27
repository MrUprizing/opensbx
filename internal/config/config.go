package config

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

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
	for _, key := range []string{"PROXY_ADDR", "BASE_DOMAIN"} {
		if _, set := os.LookupEnv(key); set {
			panic(key + " is no longer supported: use ADDR=127.0.0.1:8080; sandbox URLs share that port")
		}
	}
	addr := flag.String("addr", envOrDefault("ADDR", "127.0.0.1:8080"), "Loopback HTTP address for API, MCP and sandbox URLs")
	logFile := flag.String("log-file", envOrDefault("LOG_FILE", "opensbx.log"), "Path to log file")
	runtime := flag.String("runtime", "", "Sandbox runtime: docker or container")
	dataDir := flag.String("data-dir", DefaultDataDir(), "OpenSBX local data directory")
	legacyDB := flag.String("legacy-db", "", "Explicit legacy execution database path; back it up first and select its original runtime")
	for _, arg := range os.Args[1:] {
		key := strings.SplitN(strings.TrimLeft(arg, "-"), "=", 2)[0]
		if key == "proxy-addr" || key == "base-domain" {
			panic("legacy proxy/domain options were removed: use -addr 127.0.0.1:8080")
		}
	}
	flag.Parse()
	if err := ValidateAddr(*addr); err != nil {
		panic(err)
	}
	return &Config{Runtime: *runtime, Addr: *addr, APIKey: os.Getenv("API_KEY"), LogFile: normalizeLogFile(*logFile), DataDir: *dataDir, LegacyDB: *legacyDB}
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
