// Package runtimechoice selects one runtime before any listeners are opened.
package runtimechoice

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
)

// Select keeps terminal detection and input injectable. EOF selects Docker;
// cancellation aborts startup. Noninteractive processes never read input.
func Select(ctx context.Context, explicit, goos string, tty bool, in io.Reader, out io.Writer) (string, error) {
	if explicit != "" {
		if explicit != "docker" && explicit != "container" {
			return "", fmt.Errorf("invalid runtime %q: choose docker or container", explicit)
		}
		return explicit, nil
	}
	if goos != "darwin" || !tty {
		return "docker", nil
	}
	reader := bufio.NewReader(in)
	for {
		if _, err := fmt.Fprint(out, "Select runtime: [1] Docker (default) / [2] Apple container: "); err != nil {
			return "", err
		}
		type result struct {
			line string
			err  error
		}
		ch := make(chan result, 1)
		go func() { line, err := reader.ReadString('\n'); ch <- result{line, err} }()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case r := <-ch:
			if r.err != nil && r.err != io.EOF {
				return "", r.err
			}
			switch strings.ToLower(strings.TrimSpace(r.line)) {
			case "", "1", "docker":
				return "docker", nil
			case "2", "container", "apple container":
				return "container", nil
			}
			if r.err == io.EOF {
				return "", fmt.Errorf("invalid runtime selection at end of input")
			}
			if _, err := fmt.Fprintln(out, "Please enter 1 (Docker) or 2 (Apple container)."); err != nil {
				return "", err
			}
		}
	}
}
