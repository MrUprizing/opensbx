package config

import (
	"flag"
	"net"
	"os"
	"strings"
)

// Config holds all application configuration.
type Config struct {
	Addr                          string   // HTTP listen address, e.g. ":8080"
	APIKey                        string   // API key for authentication (env API_KEY). Empty = auth disabled.
	ProxyAddrs                    []string // Reverse proxy listen addresses, e.g. [":80", ":3000"]
	BaseDomain                    string   // Base domain for subdomain routing, e.g. "localhost"
	MCPDisableLocalhostProtection bool     // Disable MCP SDK localhost Host-header guard for non-local domains.
}

// PrimaryProxyAddr returns the first proxy address, used for generating URLs.
func (c *Config) PrimaryProxyAddr() string {
	if len(c.ProxyAddrs) == 0 {
		return ":80"
	}
	return c.ProxyAddrs[0]
}

// Load parses flags and env vars. Flags take precedence over env vars.
func Load() *Config {
	addr := flag.String("addr", envOrDefault("ADDR", ":8080"), "HTTP listen address")
	proxyAddr := flag.String("proxy-addr", envOrDefault("PROXY_ADDR", ":80,:3000"), "Comma-separated proxy listen addresses (first is used for URL generation)")
	baseDomain := flag.String("base-domain", envOrDefault("BASE_DOMAIN", "localhost"), "Base domain for subdomain routing")
	flag.Parse()

	normalizedBaseDomain := normalizeBaseDomain(*baseDomain)

	return &Config{
		Addr:                          *addr,
		APIKey:                        os.Getenv("API_KEY"),
		ProxyAddrs:                    parseAddrs(*proxyAddr),
		BaseDomain:                    normalizedBaseDomain,
		MCPDisableLocalhostProtection: !isLocalBaseDomain(normalizedBaseDomain),
	}
}

// parseAddrs splits a comma-separated list of addresses and trims whitespace.
func parseAddrs(raw string) []string {
	parts := strings.Split(raw, ",")
	addrs := make([]string, 0, len(parts))
	for _, p := range parts {
		if a := strings.TrimSpace(p); a != "" {
			addrs = append(addrs, a)
		}
	}
	return addrs
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func normalizeBaseDomain(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "localhost"
	}
	return v
}

func isLocalBaseDomain(raw string) bool {
	host := strings.Trim(strings.TrimSpace(raw), "[]")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
