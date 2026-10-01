package sandbox

import "testing"

func FuzzPortParsingRoundTripsValidatedPorts(f *testing.F) {
	for _, seed := range []string{"1", "80/tcp", "53/udp", "65535/udp", "0", "65536/tcp", "80/http", "80/tcp/extra", "１２/tcp"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		port, err := ParsePort(raw)
		if err != nil {
			return
		}
		if port.Number == 0 || port.Protocol != "tcp" && port.Protocol != "udp" {
			t.Fatalf("ParsePort(%q) accepted invalid typed port %+v", raw, port)
		}
		roundTrip, err := ParsePort(port.String())
		if err != nil || roundTrip != port {
			t.Fatalf("ParsePort(%q) => %+v; String() round trip => %+v, %v", raw, port, roundTrip, err)
		}
	})
}
