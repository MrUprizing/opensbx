package sandbox

import (
	"context"
	"regexp"
	"testing"
)

func TestNewIDGeneratesUniquePublicSandboxIDs(t *testing.T) {
	pattern := regexp.MustCompile(`^sbx-[0-9a-f]{32}$`)
	first, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	if !pattern.MatchString(string(first)) || !pattern.MatchString(string(second)) {
		t.Fatalf("invalid sandbox identity %q / %q", first, second)
	}
	if first == second {
		t.Fatalf("public ID collision: %q", first)
	}
}

func TestCreationContextPreservesExplicitIdentityAndImageProvenance(t *testing.T) {
	ctx := context.Background()
	if got := CreationID(ctx, "legacy-native"); got != "legacy-native" {
		t.Fatalf("missing creation ID fallback=%q", got)
	}
	if got := CreationImage(ctx, "legacy-tag"); got != "legacy-tag" {
		t.Fatalf("missing image provenance fallback=%q", got)
	}
	ctx = WithCreationID(ctx, SandboxID("sbx-public"))
	ctx = WithCreationImage(ctx, "sha256:oci-root")
	if got := CreationID(ctx, "native-container"); got != "sbx-public" {
		t.Fatalf("creation ID=%q", got)
	}
	if got := CreationImage(ctx, "native-cache"); got != "sha256:oci-root" {
		t.Fatalf("creation image=%q", got)
	}
	if got := CreationID(WithCreationID(context.Background(), ""), "fallback"); got != "fallback" {
		t.Fatalf("empty creation ID did not fall back: %q", got)
	}
	if got := CreationImage(WithCreationImage(context.Background(), ""), "fallback"); got != "fallback" {
		t.Fatalf("empty root did not fall back: %q", got)
	}
}
