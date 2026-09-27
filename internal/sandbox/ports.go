package sandbox

import (
	"fmt"
	"strconv"
	"strings"
)

func ParsePort(raw string) (Port, error) {
	parts := strings.Split(raw, "/")
	if len(parts) > 2 {
		return Port{}, fmt.Errorf("invalid port")
	}
	n, err := strconv.ParseUint(parts[0], 10, 16)
	if err != nil || n == 0 {
		return Port{}, fmt.Errorf("port must be between 1 and 65535")
	}
	protocol := "tcp"
	if len(parts) == 2 {
		protocol = parts[1]
	}
	if protocol != "tcp" && protocol != "udp" {
		return Port{}, fmt.Errorf("port protocol must be tcp or udp")
	}
	return Port{Number: uint16(n), Protocol: protocol}, nil
}
func (p Port) String() string {
	if p.Number == 0 {
		return ""
	}
	return strconv.Itoa(int(p.Number)) + "/" + p.Protocol
}
func ParsePorts(raw []string) ([]Port, error) {
	out := make([]Port, 0, len(raw))
	for _, s := range raw {
		p, err := ParsePort(s)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}
func PortStrings(ports []Port) []string {
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		out = append(out, p.String())
	}
	return out
}
