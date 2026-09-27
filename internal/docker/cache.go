package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	moby "github.com/moby/moby/client"
	"opensbx/internal/sandbox"
)

func ValidateEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	switch u.Scheme {
	case "unix":
		if u.Host == "" && filepath.IsAbs(u.Path) {
			return nil
		}
	case "npipe":
		if u.Host == "" && strings.HasPrefix(u.Path, "//./pipe/") {
			return nil
		}
	case "tcp", "http", "https":
		host := u.Hostname()
		ip := net.ParseIP(host)
		if u.User == nil && (host == "localhost" || ip != nil && ip.IsLoopback()) && u.Port() != "" && u.Path == "" {
			return nil
		}
	}
	return fmt.Errorf("Docker endpoint %q is not a local socket or loopback endpoint", raw)
}
func (c *Client) Capabilities(ctx context.Context) (sandbox.Capabilities, error) {
	info, err := c.cli.Info(ctx, moby.InfoOptions{})
	if err != nil {
		return sandbox.Capabilities{}, err
	}
	arch := info.Info.Architecture
	switch arch {
	case "aarch64":
		arch = "arm64"
	case "x86_64":
		arch = "amd64"
	}
	return sandbox.Capabilities{Runtime: "docker", Version: info.Info.ServerVersion + "/docker-archive-v1", Platform: sandbox.Platform{OS: info.Info.OSType, Architecture: arch}, Pause: true, FractionalCPU: true}, nil
}
func (c *Client) Materialize(ctx context.Context, image sandbox.Image) (string, error) {
	// Docker's native ID is the config digest, NOT the OCI root/manifest digest.
	if result, err := c.cli.ImageInspect(ctx, image.ConfigDigest); err == nil && result.ID == image.ConfigDigest {
		return result.ID, nil
	}
	dir, err := os.MkdirTemp("", "opensbx-cache-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "image.tar")
	archive, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	tag, err := name.NewTag("localhost/opensbx-cache:" + strings.TrimPrefix(image.ManifestDigest, "sha256:"))
	if err != nil {
		archive.Close()
		return "", err
	}
	err = tarball.Write(tag, image.Content, archive)
	closeErr := archive.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	loaded, err := c.cli.ImageLoad(ctx, f)
	if err != nil {
		return "", err
	}
	defer loaded.Close()
	dec := json.NewDecoder(io.LimitReader(loaded, 16<<20))
	for {
		var message struct {
			Error string `json:"error"`
		}
		err = dec.Decode(&message)
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if message.Error != "" {
			return "", errors.New(message.Error)
		}
	}
	result, err := c.cli.ImageInspect(ctx, image.ConfigDigest)
	if err != nil {
		return "", err
	}
	if result.ID != image.ConfigDigest {
		return "", errors.New("Docker cache config digest mismatch")
	}
	return result.ID, nil
}
