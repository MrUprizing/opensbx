package api

import (
	"fmt"
	"net"
	"strings"
)

func buildSandboxURL(name, baseDomain, proxyAddr string) string {
	if name == "" {
		return ""
	}

	baseDomain = strings.TrimSpace(baseDomain)
	if baseDomain == "" {
		baseDomain = "localhost"
	}

	if isLocalBaseDomain(baseDomain) {
		if proxyAddr == "" || proxyAddr == ":80" || proxyAddr == ":443" {
			return fmt.Sprintf("http://%s.%s", name, baseDomain)
		}
		return fmt.Sprintf("http://%s.%s%s", name, baseDomain, proxyAddr)
	}

	return fmt.Sprintf("https://%s.%s", name, baseDomain)
}

func isLocalBaseDomain(baseDomain string) bool {
	host := strings.Trim(strings.TrimSpace(baseDomain), "[]")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
