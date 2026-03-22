package proxy

import (
	"fmt"
	"net/url"

	"opensbx/internal/database"
)

// resolve looks up the sandbox by name and returns the target URL (http://127.0.0.1:{hostPort}).
func (s *Server) resolve(name string) (*url.URL, error) {
	// Check cache first.
	if target, ok := s.cache.get(name); ok {
		return target, nil
	}

	// DB lookup.
	sb, err := s.repo.FindByName(name)
	if err != nil {
		return nil, fmt.Errorf("lookup failed: %w", err)
	}
	if sb == nil {
		return nil, fmt.Errorf("not found")
	}

	// Resolve the host port for the main port.
	hostPort, err := resolveHostPort(sb)
	if err != nil {
		return nil, err
	}

	target := &url.URL{
		Scheme: "http",
		Host:   "127.0.0.1:" + hostPort,
	}

	s.cache.set(name, target)
	return target, nil
}

// resolveHostPort returns the Docker-assigned host port for the sandbox's port.
// If Port is not set but there is exactly one port in the map, it uses that.
func resolveHostPort(sb *database.Sandbox) (string, error) {
	if sb.Port != "" {
		hp, ok := sb.Ports[sb.Port]
		if !ok {
			return "", fmt.Errorf("port %q not found in port map %v", sb.Port, sb.Ports)
		}
		return hp, nil
	}

	// Fallback: use the only port if there is exactly one.
	if len(sb.Ports) == 1 {
		for _, hp := range sb.Ports {
			return hp, nil
		}
	}

	return "", fmt.Errorf("no port configured and sandbox has %d ports", len(sb.Ports))
}
