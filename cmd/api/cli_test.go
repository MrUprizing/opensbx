package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
)

func TestRootHelpAliasesAndCommandHelp(t *testing.T) {
	var shortHelp, longHelp bytes.Buffer
	if err := runCLI([]string{"-h"}, &shortHelp); err != nil {
		t.Fatal(err)
	}
	if err := runCLI([]string{"-help"}, &longHelp); err != nil {
		t.Fatal(err)
	}
	if shortHelp.String() != longHelp.String() {
		t.Fatalf("-h and -help differ:\n-h: %s\n-help: %s", shortHelp.String(), longHelp.String())
	}
	for _, expected := range []string{"start", "stop", "image", "18089"} {
		if !strings.Contains(shortHelp.String(), expected) {
			t.Errorf("root help omitted %q: %s", expected, shortHelp.String())
		}
	}

	for _, args := range [][]string{{"start", "-h"}, {"help", "start"}} {
		var output bytes.Buffer
		if err := runCLI(args, &output); err != nil {
			t.Fatalf("runCLI(%q): %v", args, err)
		}
		if !strings.Contains(output.String(), "Usage: opensbx start") || !strings.Contains(output.String(), "background") {
			t.Errorf("start help for %q was incomplete: %s", args, output.String())
		}
	}
	var stopHelp bytes.Buffer
	if err := runCLI([]string{"stop", "--help"}, &stopHelp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stopHelp.String(), "graceful shutdown") {
		t.Fatalf("stop help was incomplete: %s", stopHelp.String())
	}
}

func TestCheckAddressAvailableExplainsPortConflict(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	err = checkAddressAvailable(listener.Addr().String())
	if err == nil || !strings.Contains(err.Error(), "already in use") || !strings.Contains(err.Error(), "-addr") {
		t.Fatalf("checkAddressAvailable() = %v, want actionable port-conflict error", err)
	}
}

func TestListenErrorPreservesNonAddressInUseCause(t *testing.T) {
	underlying := errors.New("synthetic listener failure")
	wrapped := fmt.Errorf("listener setup: %w", underlying)
	got := listenError("127.0.0.1:18089", wrapped)
	if !errors.Is(got, underlying) {
		t.Fatalf("listenError() = %v; want underlying cause %v to remain discoverable", got, underlying)
	}
	if isAddressInUse(wrapped) {
		t.Fatalf("isAddressInUse(%v) = true for an unrelated listener failure", wrapped)
	}
}
