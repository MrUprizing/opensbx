package config

import (
	"flag"
	"os"
	"reflect"
	"testing"
)

func TestParseAddrsTrimsAndSkipsEmptyEntries(t *testing.T) {
	got := parseAddrs(" :80, ,127.0.0.1:3000 ")
	want := []string{":80", "127.0.0.1:3000"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseAddrs() = %v, want %v", got, want)
	}
	if got := parseAddrs(" , "); len(got) != 0 {
		t.Fatalf("parseAddrs() for empty entries = %v, want empty", got)
	}
}

func TestPrimaryProxyAddrUsesFirstConfiguredAddressOrDefault(t *testing.T) {
	if got := (&Config{}).PrimaryProxyAddr(); got != ":80" {
		t.Fatalf("PrimaryProxyAddr() with no addresses = %q, want :80", got)
	}
	if got := (&Config{ProxyAddrs: []string{":443", ":8080"}}).PrimaryProxyAddr(); got != ":443" {
		t.Fatalf("PrimaryProxyAddr() = %q, want :443", got)
	}
}

func TestLoadUsesEnvironmentDefaultsAndNormalizesValues(t *testing.T) {
	oldCommandLine, oldArgs := flag.CommandLine, os.Args
	defer func() { flag.CommandLine, os.Args = oldCommandLine, oldArgs }()
	flag.CommandLine = flag.NewFlagSet("config-test", flag.ContinueOnError)

	t.Setenv("ADDR", "127.0.0.1:9090")
	t.Setenv("API_KEY", "secret")
	t.Setenv("PROXY_ADDR", " :8443, :3000 ")
	t.Setenv("BASE_DOMAIN", "  api.example.test  ")
	t.Setenv("LOG_FILE", "  /tmp/opensbx-test.log  ")
	os.Args = []string{"config-test"}

	got := Load()
	if got.Addr != "127.0.0.1:9090" || got.APIKey != "secret" {
		t.Fatalf("Load() address/key = %q/%q", got.Addr, got.APIKey)
	}
	if !reflect.DeepEqual(got.ProxyAddrs, []string{":8443", ":3000"}) {
		t.Fatalf("Load() ProxyAddrs = %v", got.ProxyAddrs)
	}
	if got.BaseDomain != "api.example.test" || got.LogFile != "/tmp/opensbx-test.log" {
		t.Fatalf("Load() normalized fields = domain %q log %q", got.BaseDomain, got.LogFile)
	}
	if !got.MCPDisableLocalhostProtection {
		t.Fatal("Load() should disable MCP localhost protection for a public domain")
	}
}

func TestLoadFlagsOverrideEnvironmentAndLocalDomainKeepsProtection(t *testing.T) {
	oldCommandLine, oldArgs := flag.CommandLine, os.Args
	defer func() { flag.CommandLine, os.Args = oldCommandLine, oldArgs }()
	flag.CommandLine = flag.NewFlagSet("config-test", flag.ContinueOnError)

	t.Setenv("ADDR", ":9000")
	t.Setenv("PROXY_ADDR", ":80,:3000")
	t.Setenv("BASE_DOMAIN", "api.example.test")
	t.Setenv("LOG_FILE", "opensbx.log")
	t.Setenv("API_KEY", "")
	os.Args = []string{"config-test", "-addr", ":7777", "-base-domain", "dev.localhost"}

	got := Load()
	if got.Addr != ":7777" || got.BaseDomain != "dev.localhost" {
		t.Fatalf("Load() did not honor flags: %+v", got)
	}
	if got.MCPDisableLocalhostProtection {
		t.Fatal("Load() should keep localhost protection for local domains")
	}
}

func TestLoadUsesDefaultsWhenEnvironmentIsUnset(t *testing.T) {
	oldCommandLine, oldArgs := flag.CommandLine, os.Args
	defer func() { flag.CommandLine, os.Args = oldCommandLine, oldArgs }()
	flag.CommandLine = flag.NewFlagSet("config-test", flag.ContinueOnError)
	os.Args = []string{"config-test"}
	for _, key := range []string{"ADDR", "API_KEY", "PROXY_ADDR", "BASE_DOMAIN", "LOG_FILE"} {
		t.Setenv(key, "")
	}
	got := Load()
	if got.Addr != ":8080" || got.BaseDomain != "localhost" || got.LogFile != "opensbx.log" || got.APIKey != "" {
		t.Fatalf("Load() default fields = %+v", got)
	}
	if !reflect.DeepEqual(got.ProxyAddrs, []string{":80", ":3000"}) || got.MCPDisableLocalhostProtection {
		t.Fatalf("Load() default proxy/protection fields = %+v", got)
	}
}

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
