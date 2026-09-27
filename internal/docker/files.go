package docker

import (
	"context"
	"strings"
)

// ReadFile reads the content of a file inside a sandbox.
func (c *Client) ReadFile(ctx context.Context, id, path string) (string, error) {
	result, err := c.execWithStdin(ctx, id, []string{"cat", path}, nil)
	if err != nil {
		return "", err
	}
	return result.stdout, nil
}

// WriteFile writes content to a file inside a sandbox (creates parent dirs as needed).
func (c *Client) WriteFile(ctx context.Context, id, path, content string) error {
	if _, err := c.execWithStdin(ctx, id, []string{"sh", "-c", "mkdir -p $(dirname '" + path + "')"}, nil); err != nil {
		return err
	}
	_, err := c.execWithStdin(ctx, id, []string{"sh", "-c", "cat > '" + path + "'"}, strings.NewReader(content))
	return err
}

// DeleteFile deletes a file or directory inside a sandbox.
func (c *Client) DeleteFile(ctx context.Context, id, path string) error {
	_, err := c.execWithStdin(ctx, id, []string{"rm", "-rf", path}, nil)
	return err
}

// ListDir lists the contents of a directory inside a sandbox.
func (c *Client) ListDir(ctx context.Context, id, path string) (string, error) {
	result, err := c.execWithStdin(ctx, id, []string{"ls", "-la", path}, nil)
	if err != nil {
		return "", err
	}
	return result.stdout, nil
}
