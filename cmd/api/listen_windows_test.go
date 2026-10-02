//go:build windows

package main

import (
	"fmt"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsAddressInUseRecognizesDirectAndWrappedWinsockErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "direct", err: windows.WSAEADDRINUSE},
		{name: "wrapped", err: fmt.Errorf("bind listener: %w", windows.WSAEADDRINUSE)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !isAddressInUse(tc.err) {
				t.Fatalf("isAddressInUse(%v) = false; want Winsock address-in-use classification", tc.err)
			}
			got := listenError("127.0.0.1:18089", tc.err)
			if !strings.Contains(got.Error(), "already in use") || !strings.Contains(got.Error(), "-addr") {
				t.Fatalf("listenError(%v) = %v; want actionable port-conflict guidance", tc.err, got)
			}
		})
	}
}
