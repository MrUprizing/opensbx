package service

import (
	"net"
	"testing"

	"opensbx/internal/database"
)

func TestFriendlyURLUsesBoundAPIListenerPortOnlyForTCP(t *testing.T) {
	s := &Service{}
	s.SetAddress(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 43127})
	for _, tc := range []struct {
		name string
		row  database.Sandbox
		want string
	}{
		{"main TCP port", database.Sandbox{Name: "demo", Port: "3000/tcp", Ports: database.JSONMap{"3000/tcp": "32768"}}, "http://demo.localhost:43127"},
		{"single port inferred as main", database.Sandbox{Name: "demo", Ports: database.JSONMap{"3000/tcp": "32768"}}, "http://demo.localhost:43127"},
		{"UDP port omitted", database.Sandbox{Name: "demo", Port: "3000/udp", Ports: database.JSONMap{"3000/udp": "32768"}}, ""},
		{"missing published port omitted", database.Sandbox{Name: "demo", Port: "3000/tcp", Ports: database.JSONMap{"3000/tcp": ""}}, ""},
		{"invalid published port omitted", database.Sandbox{Name: "demo", Port: "3000/tcp", Ports: database.JSONMap{"3000/tcp": "not-a-port"}}, ""},
		{"out-of-range published port omitted", database.Sandbox{Name: "demo", Port: "3000/tcp", Ports: database.JSONMap{"3000/tcp": "70000"}}, ""},
		{"zero published port omitted", database.Sandbox{Name: "demo", Port: "3000/tcp", Ports: database.JSONMap{"3000/tcp": "0"}}, ""},
		{"unnamed sandbox omitted", database.Sandbox{Port: "3000/tcp", Ports: database.JSONMap{"3000/tcp": "32768"}}, ""},
		{"multiple ports need explicit main port", database.Sandbox{Name: "demo", Ports: database.JSONMap{"3000/tcp": "32768", "4000/tcp": "32769"}}, ""},
		{"invalid main protocol omitted", database.Sandbox{Name: "demo", Port: "3000/icmp", Ports: database.JSONMap{"3000/icmp": "32768"}}, ""},
		{"multiple slash main port omitted", database.Sandbox{Name: "demo", Port: "3000/tcp/extra", Ports: database.JSONMap{"3000/tcp/extra": "32768"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.url(tc.row); got != tc.want {
				t.Fatalf("friendly URL=%q want %q", got, tc.want)
			}
		})
	}
}

func TestFriendlyURLIsOmittedBeforeListenerAddressIsKnown(t *testing.T) {
	if got := (&Service{}).url(database.Sandbox{Name: "demo", Port: "3000/tcp", Ports: database.JSONMap{"3000/tcp": "32768"}}); got != "" {
		t.Fatalf("URL before listener binding=%q, want omitted", got)
	}
}
