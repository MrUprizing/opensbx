package images

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func TestPullUsesRepositoryScopedSyntheticDockerCredentials(t *testing.T) {
	const username, password = "opensbx-test-user", "not-a-real-password"
	var authenticatedRequests atomic.Int32
	var requireAuth atomic.Bool
	backend := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requireAuth.Load() {
			gotUser, gotPassword, ok := r.BasicAuth()
			if !ok || gotUser != username || gotPassword != password {
				w.Header().Set("WWW-Authenticate", `Basic realm="synthetic-registry"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			authenticatedRequests.Add(1)
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	configDir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", configDir)
	t.Setenv("HOME", t.TempDir())
	config := struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}{Auths: map[string]struct {
		Auth string `json:"auth"`
	}{host: {Auth: base64.StdEncoding.EncodeToString([]byte(username + ":" + password))}}}
	body, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), body, 0600); err != nil {
		t.Fatal(err)
	}

	ref := host + "/team/credentialed:latest"
	parsed, err := name.ParseReference(ref, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := authn.DefaultKeychain.Resolve(parsed.Context())
	if err != nil {
		t.Fatal(err)
	}
	resolvedConfig, err := resolved.Authorization()
	if err != nil || resolvedConfig.Username != username || resolvedConfig.Password != password {
		t.Fatalf("Docker keychain resolved unexpected synthetic credentials: config=%+v err=%v", resolvedConfig, err)
	}
	image, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	configFile, err := image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	configFile.OS, configFile.Architecture = "linux", "amd64"
	image, err = mutate.ConfigFile(image, configFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(parsed, image, remote.WithTransport(server.Client().Transport)); err != nil {
		t.Fatal(err)
	}
	requireAuth.Store(true)
	authenticatedRequests.Store(0)
	store, err := Open(t.TempDir(), WithTransport(server.Client().Transport))
	if err != nil {
		t.Fatal(err)
	}
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	if err := store.Pull(context.Background(), ref, platform); err != nil {
		t.Fatalf("pull with isolated synthetic Docker credentials: %v", err)
	}
	if got := authenticatedRequests.Load(); got == 0 {
		t.Fatal("registry pull never retried with the configured repository credential")
	}
	if _, err := store.Resolve(context.Background(), ref, platform); err != nil {
		t.Fatalf("resolve image after authenticated pull: %v", err)
	}
}
