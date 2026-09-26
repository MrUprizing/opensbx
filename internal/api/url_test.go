package api

import "testing"

func TestBuildSandboxURL_LocalPortBoundaries(t *testing.T) {
	for _, tc := range []struct{ addr, want string }{
		{"", "http://demo.localhost"},
		{":80", "http://demo.localhost"},
		{":443", "http://demo.localhost"},
		{":8080", "http://demo.localhost:8080"},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			if got := buildSandboxURL("demo", "localhost", tc.addr); got != tc.want {
				t.Fatalf("buildSandboxURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildSandboxURL_LocalDomain(t *testing.T) {
	got := buildSandboxURL("eager-turing", "localhost", ":3000")
	want := "http://eager-turing.localhost:3000"
	if got != want {
		t.Fatalf("buildSandboxURL() = %q, want %q", got, want)
	}
}

func TestBuildSandboxURL_PublicDomain(t *testing.T) {
	got := buildSandboxURL("admiring-heyrovsky", "opensbx.run", ":3000")
	want := "https://admiring-heyrovsky.opensbx.run"
	if got != want {
		t.Fatalf("buildSandboxURL() = %q, want %q", got, want)
	}
}

func TestBuildSandboxURL_EmptyBaseDomainFallsBackToLocalhost(t *testing.T) {
	got := buildSandboxURL("demo", "", ":3000")
	want := "http://demo.localhost:3000"
	if got != want {
		t.Fatalf("buildSandboxURL() = %q, want %q", got, want)
	}
}
