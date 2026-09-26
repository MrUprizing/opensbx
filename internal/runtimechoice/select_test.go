package runtimechoice

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSelectExplicitRuntimeBypassesInteractiveInput(t *testing.T) {
	for _, runtime := range []string{"docker", "container"} {
		t.Run(runtime, func(t *testing.T) {
			got, err := Select(context.Background(), runtime, "darwin", true, strings.NewReader("2\n"), nil)
			if err != nil || got != runtime {
				t.Fatalf("Select() = %q, %v; want %q", got, err, runtime)
			}
		})
	}
	if _, err := Select(context.Background(), "docker ", "darwin", true, strings.NewReader("2\n"), nil); err == nil {
		t.Fatal("invalid explicit runtime accepted")
	}
}

func TestSelectDefaultsToDockerWithoutInteractiveDarwinTTY(t *testing.T) {
	for _, tc := range []struct {
		name string
		os   string
		tty  bool
	}{{"linux tty", "linux", true}, {"darwin non-tty", "darwin", false}} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Select(context.Background(), "", tc.os, tc.tty, strings.NewReader("2\n"), nil)
			if err != nil || got != "docker" {
				t.Fatalf("Select() = %q, %v; want docker", got, err)
			}
		})
	}
}

func TestSelectInteractiveDefaultsRetriesInvalidAndAcceptsAppleContainer(t *testing.T) {
	var output strings.Builder
	got, err := Select(context.Background(), "", "darwin", true, strings.NewReader("what\n Apple Container \n"), &output)
	if err != nil || got != "container" {
		t.Fatalf("Select() = %q, %v; want container", got, err)
	}
	if !strings.Contains(output.String(), "Please enter") || strings.Count(output.String(), "Select runtime:") != 2 {
		t.Fatalf("unexpected prompt/retry output %q", output.String())
	}
	got, err = Select(context.Background(), "", "darwin", true, strings.NewReader("\n"), &strings.Builder{})
	if err != nil || got != "docker" {
		t.Fatalf("empty selection = %q, %v; want docker", got, err)
	}
}

func TestSelectInteractiveEOFAndCancellation(t *testing.T) {
	if got, err := Select(context.Background(), "", "darwin", true, strings.NewReader(""), &strings.Builder{}); err != nil || got != "docker" {
		t.Fatalf("EOF selection = %q, %v; want docker", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Select(ctx, "", "darwin", true, strings.NewReader(""), &strings.Builder{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled selection error = %v, want context.Canceled", err)
	}
}
