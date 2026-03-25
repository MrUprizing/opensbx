package config

import "testing"

func TestNormalizeBaseDomain(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: "localhost"},
		{name: "whitespace", in: "   ", want: "localhost"},
		{name: "keeps domain", in: "opensbx.run", want: "opensbx.run"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeBaseDomain(tt.in)
			if got != tt.want {
				t.Fatalf("normalizeBaseDomain(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestIsLocalBaseDomain(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{name: "localhost", in: "localhost", want: true},
		{name: "sub localhost", in: "dev.localhost", want: true},
		{name: "ipv4 loopback", in: "127.0.0.1", want: true},
		{name: "ipv6 loopback", in: "::1", want: true},
		{name: "public domain", in: "opensbx.run", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isLocalBaseDomain(tt.in)
			if got != tt.want {
				t.Fatalf("isLocalBaseDomain(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestNormalizeLogFile(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: "opensbx.log"},
		{name: "whitespace", in: "   ", want: "opensbx.log"},
		{name: "keeps custom path", in: "logs/server.log", want: "logs/server.log"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeLogFile(tt.in)
			if got != tt.want {
				t.Fatalf("normalizeLogFile(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
