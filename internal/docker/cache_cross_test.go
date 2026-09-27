package docker

import (
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1"
	"opensbx/internal/applecontainer"
	"opensbx/internal/database"
	"opensbx/internal/images"
	"opensbx/internal/sandbox"
	"opensbx/internal/testsupport"
)

type appleCacheRunner struct {
	t         *testing.T
	archive   []byte
	reference string
	commands  [][]string
}

func (r *appleCacheRunner) Run(_ context.Context, args []string, _ io.Reader, _, _ io.Writer) error {
	r.commands = append(r.commands, append([]string(nil), args...))
	switch {
	case len(args) == 4 && reflect.DeepEqual(args[:3], []string{"image", "load", "--input"}):
		body, err := os.ReadFile(args[3])
		if err != nil {
			return err
		}
		r.archive = body
	case len(args) == 7 && reflect.DeepEqual(args[:3], []string{"image", "save", "--output"}) && args[4] == "--platform" && args[5] == "linux/arm64":
		r.reference = args[6]
		return os.WriteFile(args[3], r.archive, 0600)
	default:
		return errors.New("unexpected Apple cache CLI operation")
	}
	return nil
}
func (r *appleCacheRunner) Start([]string, io.Reader, io.Writer, io.Writer) (applecontainer.Process, error) {
	return nil, errors.New("unexpected Apple async CLI operation")
}

func TestDockerAndAppleCachesMaterializeTheSameResolvedOCIManifest(t *testing.T) {
	ctx := context.Background()
	archive, platform := testsupport.OCIArchivePlatform(t, v1.Platform{OS: "linux", Architecture: "arm64"})
	store, err := images.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const sourceRef = "example.test/team/shared:stable"
	if err := store.Import(ctx, archive, sourceRef, platform); err != nil {
		t.Fatal(err)
	}
	image, err := store.ResolveImage(ctx, sourceRef, sandbox.Platform{OS: platform.OS, Architecture: platform.Architecture})
	if err != nil {
		t.Fatal(err)
	}
	dockerClient, daemon := newDockerFixture(t)
	daemon.cacheDigest = image.ConfigDigest
	dockerHandle, err := dockerClient.Materialize(ctx, image)
	if err != nil {
		t.Fatal(err)
	}
	if dockerHandle != image.ConfigDigest {
		t.Fatalf("Docker cache handle=%q want native config digest %q", dockerHandle, image.ConfigDigest)
	}
	db := database.New(":memory:")
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	runner := &appleCacheRunner{t: t}
	appleClient := applecontainer.New(database.NewRepository(db), runner, nil)
	appleHandle, err := appleClient.Materialize(ctx, image)
	if err != nil {
		t.Fatal(err)
	}
	wantAppleHandle, err := applecontainer.CacheReference(image.ManifestDigest)
	if err != nil {
		t.Fatal(err)
	}
	if appleHandle != wantAppleHandle || runner.reference != wantAppleHandle {
		t.Fatalf("Apple handle=%q save reference=%q want %q", appleHandle, runner.reference, wantAppleHandle)
	}
	if image.RootDigest == "" || image.ManifestDigest == "" || image.ConfigDigest == "" || image.ManifestDigest == image.ConfigDigest || image.RootDigest == image.ConfigDigest {
		t.Fatalf("resolved manager identities collapsed: %+v", image)
	}
	if len(runner.commands) != 2 {
		t.Fatalf("Apple cache commands=%v", runner.commands)
	}
	if !strings.Contains(runner.reference, image.ManifestDigest) {
		t.Fatalf("Apple cache identity lost selected manifest: %q vs %q", runner.reference, image.ManifestDigest)
	}
	if daemon.cacheLoaded == false {
		t.Fatal("Docker adapter did not import manager-supplied OCI content")
	}
}
