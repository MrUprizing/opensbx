package config

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateAddrRequiresLoopbackIPAndValidPort(t *testing.T) {
	for _, tc := range []struct {
		addr  string
		valid bool
	}{
		{"127.0.0.1:8080", true}, {"[::1]:0", true}, {"localhost:8080", false},
		{"0.0.0.0:8080", false}, {"192.168.1.2:8080", false}, {"127.0.0.1:65536", false},
		{"127.0.0.1", false},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			err := ValidateAddr(tc.addr)
			if (err == nil) != tc.valid {
				t.Fatalf("ValidateAddr(%q) error = %v, want valid=%v", tc.addr, err, tc.valid)
			}
		})
	}
}

func TestLoadAppliesExplicitFlagsAndEnvironmentDefaults(t *testing.T) {
	oldCommandLine, oldArgs := flag.CommandLine, os.Args
	defer func() { flag.CommandLine, os.Args = oldCommandLine, oldArgs }()
	flag.CommandLine = flag.NewFlagSet("config-test", flag.ContinueOnError)
	os.Args = []string{"config-test", "-addr", "127.0.0.1:9099", "-runtime", "docker", "-data-dir", filepath.Join(t.TempDir(), "explicit"), "-legacy-db", filepath.Join(t.TempDir(), "sandbox.db"), "-log-file", "custom.log"}
	t.Setenv("ADDR", "127.0.0.1:8088")
	t.Setenv("LOG_FILE", "env.log")
	t.Setenv("API_KEY", "local-secret")
	for _, key := range []string{"PROXY_ADDR", "BASE_DOMAIN"} {
		old, had := os.LookupEnv(key)
		_ = os.Unsetenv(key)
		t.Cleanup(func() {
			if had {
				_ = os.Setenv(key, old)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
	got := Load()
	if got.Addr != "127.0.0.1:9099" || got.Runtime != "docker" || got.LegacyDB == "" || got.LogFile != "custom.log" || got.APIKey != "local-secret" {
		t.Fatalf("explicit config=%+v", got)
	}
	if got.DataDir != filepath.Join(filepath.Dir(got.LegacyDB), "explicit") && !strings.HasSuffix(got.DataDir, "explicit") {
		t.Fatalf("data directory flag ignored: %q", got.DataDir)
	}
}

func TestDefaultDataDirUsesExplicitLocalOverride(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "user-owned-state")
	t.Setenv("OPENSBX_DATA_DIR", custom)
	if got := DefaultDataDir(); got != custom {
		t.Fatalf("DefaultDataDir()=%q want %q", got, custom)
	}
}

func TestLoadUsesLoopbackDefaultsAndNormalizedLogFile(t *testing.T) {
	oldCommandLine, oldArgs := flag.CommandLine, os.Args
	defer func() { flag.CommandLine, os.Args = oldCommandLine, oldArgs }()
	flag.CommandLine = flag.NewFlagSet("config-test", flag.ContinueOnError)
	os.Args = []string{"config-test"}
	for _, key := range []string{"ADDR", "API_KEY", "LOG_FILE", "OPENSBX_DATA_DIR", "PROXY_ADDR", "BASE_DOMAIN"} {
		previous, existed := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if existed {
				_ = os.Setenv(key, previous)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
	got := Load()
	if got.Addr != "127.0.0.1:18089" || got.APIKey != "" || got.LogFile != "opensbx.log" {
		t.Fatalf("unexpected defaults: %+v", got)
	}
}

func TestLoadRejectsRetiredProxyAndDomainEnvironment(t *testing.T) {
	for _, key := range []string{"PROXY_ADDR", "BASE_DOMAIN"} {
		t.Run(key, func(t *testing.T) {
			oldCommandLine, oldArgs := flag.CommandLine, os.Args
			defer func() { flag.CommandLine, os.Args = oldCommandLine, oldArgs }()
			flag.CommandLine = flag.NewFlagSet("config-test", flag.ContinueOnError)
			os.Args = []string{"config-test"}
			t.Setenv(key, "legacy-value")
			defer func() {
				if recover() == nil {
					t.Fatal("Load() did not reject retired setting")
				}
			}()
			Load()
		})
	}
}

func TestNormalizeLogFile(t *testing.T) {
	for _, tc := range []struct{ in, want string }{{"", "opensbx.log"}, {"   ", "opensbx.log"}, {" logs/server.log ", "logs/server.log"}} {
		if got := normalizeLogFile(tc.in); got != tc.want {
			t.Errorf("normalizeLogFile(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
