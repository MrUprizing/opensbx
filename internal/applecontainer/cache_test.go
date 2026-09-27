package applecontainer

import (
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"opensbx/internal/images"
	"opensbx/internal/sandbox"
	"opensbx/internal/testsupport"
)

func TestMaterializeUsesCanonicalDigestReferenceAndVerifiesSavedManifest(t *testing.T) {
	ctx := context.Background()
	archive, platform := testsupport.OCIArchivePlatform(t, v1.Platform{OS: "linux", Architecture: "arm64"})
	store, err := images.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Import(ctx, archive, "example.test/team/node:stable", platform); err != nil {
		t.Fatal(err)
	}
	image, err := store.ResolveImage(ctx, "example.test/team/node:stable", sandbox.Platform{OS: platform.OS, Architecture: platform.Architecture})
	if err != nil {
		t.Fatal(err)
	}
	wantRef, err := CacheReference(image.ManifestDigest)
	if err != nil {
		t.Fatal(err)
	}
	runner := &scriptedRunner{t: t}
	var archiveInput string
	runner.run = func(args []string, _ io.Reader, _, _ io.Writer) error {
		switch {
		case len(args) == 4 && reflect.DeepEqual(args[:3], []string{"image", "load", "--input"}):
			archiveInput = args[3]
			if _, err := os.Stat(archiveInput); err != nil {
				t.Fatalf("load archive absent: %v", err)
			}
		case len(args) == 7 && reflect.DeepEqual(args[:3], []string{"image", "save", "--output"}) && args[4] == "--platform" && args[5] == "linux/arm64" && args[6] == wantRef:
			if archiveInput == "" {
				t.Fatal("save preceded local archive load")
			}
			input, err := os.ReadFile(archiveInput)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(args[3], input, 0600); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unexpected cache materialization argv: %#v", args)
		}
		return nil
	}
	client, _ := testClient(t, runner)
	got, err := client.Materialize(ctx, image)
	if err != nil {
		t.Fatal(err)
	}
	if got != wantRef || got != "opensbx.invalid/cache@"+image.ManifestDigest {
		t.Fatalf("materialized cache reference=%q want %q", got, wantRef)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("native image CLI calls=%d want load+save only (%v)", len(runner.calls), runner.calls)
	}
}

func TestMaterializeRejectsUnsupportedVariantBeforeNativeCalls(t *testing.T) {
	for _, platform := range []sandbox.Platform{
		{OS: "linux", Architecture: "arm64", Variant: "v8"},
		{OS: "darwin", Architecture: "arm64"},
		{OS: "linux", Architecture: "amd64"},
	} {
		t.Run(platform.OS+"/"+platform.Architecture+"/"+platform.Variant, func(t *testing.T) {
			runner := &scriptedRunner{t: t}
			client, _ := testClient(t, runner)
			_, err := client.Materialize(context.Background(), sandbox.Image{Platform: platform})
			if err != sandbox.ErrUnsupported {
				t.Fatalf("unsupported platform error=%v", err)
			}
			if len(runner.calls) != 0 {
				t.Fatalf("unsupported platform reached native runtime: %v", runner.calls)
			}
		})
	}
}

func TestMaterializeRejectsInvalidDigestBeforeNativeCalls(t *testing.T) {
	runner := &scriptedRunner{t: t}
	client, _ := testClient(t, runner)
	_, err := client.Materialize(context.Background(), sandbox.Image{Platform: sandbox.Platform{OS: "linux", Architecture: "arm64"}, ManifestDigest: "sha256:bad"})
	if err == nil {
		t.Fatal("invalid manifest digest accepted")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("invalid digest reached native runtime: %v", runner.calls)
	}
}

func TestCacheReferenceIsCanonicalAndRejectsMalformedDigest(t *testing.T) {
	valid := "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	got, err := CacheReference(valid)
	if err != nil || got != "opensbx.invalid/cache@"+valid {
		t.Fatalf("CacheReference(%q)=%q err=%v", valid, got, err)
	}
	for _, invalid := range []string{"", "sha256:bad", "sha512:" + strings.Repeat("0", 128), "-sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"} {
		if got, err := CacheReference(invalid); err == nil {
			t.Errorf("CacheReference(%q) unexpectedly accepted %q", invalid, got)
		}
	}
}

func TestAppleCacheCapabilitiesReportsValidatedRuntimeLimits(t *testing.T) {
	runner := &scriptedRunner{t: t}
	runner.run = func(args []string, _ io.Reader, out, _ io.Writer) error {
		if !reflect.DeepEqual(args, []string{"system", "status", "--format", "json"}) {
			t.Fatalf("unexpected capability probe args %v", args)
		}
		_, _ = io.WriteString(out, `{"Status":"running","Client":{"Version":"1.4.1"},"Server":{"Version":"1.4.1"}}`)
		return nil
	}
	client, _ := testClient(t, runner)
	caps, err := client.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.Runtime != "container" || caps.Version != "1.4.1/oci-load-v1" || caps.Platform.OS != "linux" || caps.Platform.Architecture != "arm64" || caps.Pause || caps.FractionalCPU {
		t.Fatalf("Apple capabilities=%+v", caps)
	}
}

func TestAppleCapabilitiesPropagatesHealthFailureWithoutInventingLimits(t *testing.T) {
	runner := &scriptedRunner{t: t}
	runner.run = func([]string, io.Reader, io.Writer, io.Writer) error { return errors.New("user service unavailable") }
	client, _ := testClient(t, runner)
	caps, err := client.Capabilities(context.Background())
	if err == nil || !strings.Contains(err.Error(), "CLI execution failed") || strings.Contains(err.Error(), "user service unavailable") {
		t.Fatalf("capability health error=%v", err)
	}
	if caps.Runtime != "" || caps.Pause || caps.FractionalCPU {
		t.Fatalf("failed health probe returned usable capabilities: %+v", caps)
	}
}

func TestMaterializeStopsOnArchiveAndNativeImportErrors(t *testing.T) {
	ctx := context.Background()
	image := sandbox.Image{
		ManifestDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Platform:       sandbox.Platform{OS: "linux", Architecture: "arm64"},
		Archive:        func(string, string) error { return errors.New("offline OCI archive failure") },
	}
	t.Run("archive failure", func(t *testing.T) {
		runner := &scriptedRunner{t: t}
		client, _ := testClient(t, runner)
		_, err := client.Materialize(ctx, image)
		if err == nil || !strings.Contains(err.Error(), "offline OCI archive failure") {
			t.Fatalf("archive failure=%v", err)
		}
		if len(runner.calls) != 0 {
			t.Fatalf("native runtime called despite archive failure: %v", runner.calls)
		}
	})
	t.Run("native image load failure", func(t *testing.T) {
		image.Archive = func(path, _ string) error { return os.WriteFile(path, []byte("archive"), 0600) }
		runner := &scriptedRunner{t: t}
		runner.run = func(args []string, _ io.Reader, _, _ io.Writer) error {
			if args[1] != "load" {
				t.Fatalf("image save ran after load error: %v", args)
			}
			return errors.New("native import rejected")
		}
		client, _ := testClient(t, runner)
		if _, err := client.Materialize(ctx, image); err == nil || !strings.Contains(err.Error(), "CLI execution failed") {
			t.Fatalf("load error=%v", err)
		}
		if len(runner.calls) != 1 {
			t.Fatalf("native command sequence after load error=%v", runner.calls)
		}
	})
	t.Run("native save failure", func(t *testing.T) {
		image.Archive = func(path, _ string) error { return os.WriteFile(path, []byte("archive"), 0600) }
		runner := &scriptedRunner{t: t}
		runner.run = func(args []string, _ io.Reader, _, _ io.Writer) error {
			if args[1] == "save" {
				return errors.New("native save rejected")
			}
			return nil
		}
		client, _ := testClient(t, runner)
		if _, err := client.Materialize(ctx, image); err == nil || !strings.Contains(err.Error(), "CLI execution failed") {
			t.Fatalf("save error=%v", err)
		}
		if len(runner.calls) != 2 {
			t.Fatalf("native command sequence after save error=%v", runner.calls)
		}
	})
}

func TestMaterializeRejectsNativeSaveThatChangesOCIManifestOrConfig(t *testing.T) {
	ctx := context.Background()
	platform := v1.Platform{OS: "linux", Architecture: "arm64"}
	wrongArchive, _ := testsupport.OCIArchivePlatform(t, platform)
	store, err := images.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Import(ctx, wrongArchive, "example.test/team/wrong:latest", platform); err != nil {
		t.Fatal(err)
	}
	image, err := store.ResolveImage(ctx, "example.test/team/wrong:latest", sandbox.Platform{OS: platform.OS, Architecture: platform.Architecture})
	if err != nil {
		t.Fatal(err)
	}
	otherArchive, _ := testsupport.OCIArchivePlatform(t, platform)
	image.Archive = func(path, _ string) error {
		b, err := os.ReadFile(otherArchive)
		if err != nil {
			return err
		}
		return os.WriteFile(path, b, 0600)
	}
	runner := &scriptedRunner{t: t}
	var loadedArchive string
	runner.run = func(args []string, _ io.Reader, _, _ io.Writer) error {
		switch args[1] {
		case "load":
			loadedArchive = args[3]
		case "save":
			b, err := os.ReadFile(loadedArchive)
			if err != nil {
				return err
			}
			return os.WriteFile(args[3], b, 0600)
		default:
			t.Fatalf("unexpected verification command %v", args)
		}
		return nil
	}
	client, _ := testClient(t, runner)
	if _, err := client.Materialize(ctx, image); err == nil || !strings.Contains(err.Error(), "verification failed") {
		t.Fatalf("mismatched native save error=%v", err)
	}
}

func TestMaterializeRejectsMalformedNativeSaveArchive(t *testing.T) {
	ctx := context.Background()
	archive, platform := testsupport.OCIArchivePlatform(t, v1.Platform{OS: "linux", Architecture: "arm64"})
	store, err := images.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Import(ctx, archive, "example.test/team/malformed-save:latest", platform); err != nil {
		t.Fatal(err)
	}
	image, err := store.ResolveImage(ctx, "example.test/team/malformed-save:latest", sandbox.Platform{OS: platform.OS, Architecture: platform.Architecture})
	if err != nil {
		t.Fatal(err)
	}
	runner := &scriptedRunner{t: t}
	runner.run = func(args []string, _ io.Reader, _, _ io.Writer) error {
		if args[1] == "save" {
			return os.WriteFile(args[3], []byte("not an OCI archive"), 0600)
		}
		return nil
	}
	client, _ := testClient(t, runner)
	if _, err := client.Materialize(ctx, image); err == nil {
		t.Fatal("malformed image save was accepted as a verified native cache entry")
	}
	if len(runner.calls) != 2 {
		t.Fatalf("malformed saved archive command sequence=%v, want load then one save", runner.calls)
	}
}

func TestMaterializePropagatesArchiveCancellationWithoutNativeCommands(t *testing.T) {
	archive, platform := testsupport.OCIArchivePlatform(t, v1.Platform{OS: "linux", Architecture: "arm64"})
	store, err := images.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const ref = "example.test/team/canceled-cache:latest"
	if err := store.Import(context.Background(), archive, ref, platform); err != nil {
		t.Fatal(err)
	}
	image, err := store.ResolveImage(context.Background(), ref, sandbox.Platform{OS: platform.OS, Architecture: platform.Architecture})
	if err != nil {
		t.Fatal(err)
	}
	image.Archive = func(string, string) error { return context.Canceled }
	runner := &scriptedRunner{t: t}
	client, _ := testClient(t, runner)
	if _, err := client.Materialize(context.Background(), image); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled OCI archive export error=%v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("canceled cache materialization reached native commands: %v", runner.calls)
	}
}
