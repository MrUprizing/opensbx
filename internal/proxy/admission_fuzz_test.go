package proxy

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func FuzzLocalHandlerRejectsInvalidControlPlaneAdmission(f *testing.F) {
	f.Add("localhost:8080", "<none>", "<none>")
	f.Add("evil.test:8080", "http://localhost:8080", "<none>")
	f.Add("localhost:8080", "https://localhost:8080", "<none>")
	f.Add("localhost:8080", "http://attacker.localhost:8080", "cross-site")
	f.Add("demo.localhost:8080", "http://localhost:8080", "<none>")
	f.Fuzz(func(t *testing.T, host, origin, fetchSite string) {
		controlCalls, sandboxCalls := 0, 0
		control := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { controlCalls++ })
		sandboxes := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { sandboxCalls++ })
		handler := LocalHandler(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8080}, control, sandboxes)
		req := httptest.NewRequest(http.MethodGet, "http://localhost:8080/v1/health", nil)
		req.Host = host
		if origin != "<none>" {
			req.Header.Set("Origin", origin)
		}
		if fetchSite != "<none>" {
			req.Header.Set("Sec-Fetch-Site", fetchSite)
		}
		handler.ServeHTTP(httptest.NewRecorder(), req)

		canonicalHost, port, authorityErr := canonicalAuthority(host)
		if controlCalls > 0 {
			if authorityErr != nil || port != "8080" {
				t.Fatalf("control plane admitted malformed authority %q", host)
			}
			if canonicalHost != "localhost" && canonicalHost != "127.0.0.1" {
				t.Fatalf("control plane admitted non-loopback authority %q", host)
			}
			if !controlOriginAllowed(req, canonicalHost, port) {
				t.Fatalf("control plane admitted disallowed origin/site for host=%q origin=%q site=%q", host, origin, fetchSite)
			}
		}
		if sandboxCalls > 0 && (authorityErr != nil || port != "8080" || !sandboxHost.MatchString(canonicalHost)) {
			t.Fatalf("sandbox handler admitted invalid sandbox authority %q", host)
		}
		if controlCalls > 1 || sandboxCalls > 1 || controlCalls > 0 && sandboxCalls > 0 {
			t.Fatalf("request dispatched ambiguously: control=%d sandboxes=%d host=%q", controlCalls, sandboxCalls, host)
		}
	})
}
