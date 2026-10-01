package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"opensbx/internal/terminaltext"
)

// Only fixed source runs in the guest shell; paths are literal positional arguments.
func (c *Client) file(ctx context.Context, id, path, script string, stdin io.Reader) (string, error) {
	if path == "" || strings.ContainsRune(path, 0) {
		return "", errors.New("guest path must be nonempty and contain no NUL")
	}
	cmd := []string{"/bin/sh", "-c", `case "$1" in /*) ;; *) set -- "./$1";; esac; ` + script, "opensbx-file", path}
	result, err := c.execWithStdin(ctx, id, cmd, stdin)
	if err != nil {
		return "", err
	}
	if result.exitCode != 0 {
		// Bound before escaping: guest diagnostics must not inject terminal controls
		// or grow an error without limit. Never include file content from stdout.
		diagnostic := result.stderr
		if len(diagnostic) > 256 {
			diagnostic = diagnostic[:256] + "..."
		}
		return "", fmt.Errorf("docker file operation exited with code %d: %q", result.exitCode, terminaltext.Escape(diagnostic, false))
	}
	return result.stdout, nil
}

// ReadFile reads the content of a file inside a sandbox.
func (c *Client) ReadFile(ctx context.Context, id, path string) (string, error) {
	return c.file(ctx, id, path, `exec cat "$1"`, nil)
}

// WriteFile writes content to a file inside a sandbox (creates parent dirs as needed).
func (c *Client) WriteFile(ctx context.Context, id, path, content string) error {
	if _, err := c.file(ctx, id, path, `mkdir -p "$(dirname "$1")"`, nil); err != nil {
		return err
	}
	_, err := c.file(ctx, id, path, `cat > "$1"`, strings.NewReader(content))
	return err
}

// DeleteFile deletes a file or directory inside a sandbox.
func (c *Client) DeleteFile(ctx context.Context, id, path string) error {
	_, err := c.file(ctx, id, path, `exec rm -rf -- "$1"`, nil)
	return err
}

// ListDir lists the contents of a directory inside a sandbox.
func (c *Client) ListDir(ctx context.Context, id, path string) (string, error) {
	return c.file(ctx, id, path, `exec ls -la "$1"`, nil)
}
