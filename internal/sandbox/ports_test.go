package sandbox

import "testing"

func TestParsePortAndStringNormalizeTypedPorts(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"80", "80/tcp"}, {"65535/udp", "65535/udp"}, {"1/tcp", "1/tcp"},
	} {
		port, err := ParsePort(tc.input)
		if err != nil || port.String() != tc.want {
			t.Errorf("ParsePort(%q)=%+v err=%v want %q", tc.input, port, err, tc.want)
		}
	}
	if got := (Port{}).String(); got != "" {
		t.Fatalf("zero port string=%q want empty", got)
	}
}

func TestParsePortsRejectsMalformedBoundariesAndConvertsRoundTrip(t *testing.T) {
	for _, invalid := range []string{"", "0", "65536/tcp", "abc/tcp", "80/http", "80/tcp/extra"} {
		if _, err := ParsePort(invalid); err == nil {
			t.Errorf("ParsePort(%q) unexpectedly succeeded", invalid)
		}
	}
	ports, err := ParsePorts([]string{"3000", "53/udp"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"3000/tcp", "53/udp"}
	got := PortStrings(ports)
	if len(got) != len(want) {
		t.Fatalf("PortStrings=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("PortStrings=%v want %v", got, want)
		}
	}
	if _, err := ParsePorts([]string{"3000/tcp", "bad"}); err == nil {
		t.Fatal("invalid member in port list accepted")
	}
}
