package api

import "testing"

func TestExtractMCPMethods(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "single request", body: `{"jsonrpc":"2.0","method":"tools/list"}`, want: "tools/list"},
		{name: "batch request", body: `[{"method":"tools/list"},{"method":"tools/call"}]`, want: "tools/list,tools/call"},
		{name: "batch deduplicates", body: `[{"method":"tools/list"},{"method":"tools/list"}]`, want: "tools/list"},
		{name: "invalid json", body: `{`, want: ""},
		{name: "empty", body: ``, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractMCPMethods([]byte(tt.body))
			if got != tt.want {
				t.Fatalf("extractMCPMethods(%q) = %q, want %q", tt.body, got, tt.want)
			}
		})
	}
}
