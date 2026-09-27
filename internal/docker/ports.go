package docker

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/moby/moby/api/types/network"
)

// normalizePort ensures a port spec has a protocol suffix.
// "3000" → "3000/tcp", "3000/tcp" → "3000/tcp" (unchanged).
func normalizePort(port string) string {
	if port == "" {
		return ""
	}
	if !strings.Contains(port, "/") {
		return port + "/tcp"
	}
	return port
}

// normalizePorts normalizes a slice of port specs.
func normalizePorts(ports []string) []string {
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		if n := normalizePort(p); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// buildExposedPorts converts a slice of port specs to network.PortSet.
func buildExposedPorts(ports []string) network.PortSet {
	if len(ports) == 0 {
		return nil
	}
	ps := make(network.PortSet)
	for _, p := range ports {
		parsed, err := network.ParsePort(p)
		if err != nil {
			continue
		}
		ps[parsed] = struct{}{}
	}
	if len(ps) == 0 {
		return nil
	}
	return ps
}

// buildPortBindings creates port bindings that only listen on 127.0.0.1 (loopback).
// This ensures container ports are only reachable through the reverse proxy, not directly.
func buildPortBindings(ports []string) network.PortMap {
	if len(ports) == 0 {
		return nil
	}
	pm := make(network.PortMap)
	for _, p := range ports {
		parsed, err := network.ParsePort(p)
		if err != nil {
			continue
		}
		pm[parsed] = []network.PortBinding{{HostIP: netip.MustParseAddr("127.0.0.1")}}
	}
	if len(pm) == 0 {
		return nil
	}
	return pm
}

// extractPorts converts network.PortMap to map["80/tcp"]"32768".
func extractPorts(pm network.PortMap) map[string]string {
	out := make(map[string]string)
	for port, bindings := range pm {
		if len(bindings) > 0 {
			out[port.String()] = bindings[0].HostPort
		}
	}
	return out
}

// portKeys returns the container port keys from a port map (e.g. ["3000/tcp", "8080/tcp"]).
func portKeys(pm map[string]string) []string {
	keys := make([]string, 0, len(pm))
	for k := range pm {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// portKey builds a port key like "3000/tcp".
func portKey(port uint16, proto string) string {
	if proto == "" {
		proto = "tcp"
	}
	return portValue(port) + "/" + proto
}

// portValue converts a uint16 port to its string representation.
func portValue(port uint16) string {
	return fmt.Sprintf("%d", port)
}
