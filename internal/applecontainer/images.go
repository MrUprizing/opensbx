package applecontainer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"opensbx/internal/docker"
	"opensbx/models"

	"github.com/distribution/reference"
)

type imageVariant struct {
	Platform struct {
		Architecture string
		OS           string
	}
	Size   int64
	Config struct {
		Created      string
		Architecture string
		OS           string
	}
}
type imageInfo struct {
	ID            string
	Configuration struct {
		Name       string
		Descriptor struct{ Digest string }
	}
	Variants []imageVariant
}

func imageRef(s string) (string, error) {
	if s == "" || strings.HasPrefix(s, "-") || strings.ContainsAny(s, "\x00\r\n") {
		return "", errors.New("invalid image reference")
	}
	ref, err := reference.ParseNormalizedNamed(s)
	if err != nil {
		return "", errors.New("invalid image reference")
	}
	return reference.TagNameOnly(ref).String(), nil
}
func (c *Client) images(ctx context.Context) ([]imageInfo, error) {
	b, err := c.run(ctx, nil, "image", "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	var images []imageInfo
	if err := json.Unmarshal(b, &images); err != nil {
		return nil, errors.New("invalid Apple image list JSON")
	}
	for _, im := range images {
		if im.Configuration.Name == "" || im.Configuration.Descriptor.Digest == "" {
			return nil, errors.New("incomplete Apple image JSON")
		}
	}
	return images, nil
}
func (c *Client) findImage(ctx context.Context, id string) (imageInfo, error) {
	var zero imageInfo
	if id == "" || strings.HasPrefix(id, "-") || strings.ContainsAny(id, "\x00\r\n") {
		return zero, errors.New("invalid image reference")
	}
	normalized, refErr := imageRef(id)
	all, err := c.images(ctx)
	if err != nil {
		return zero, err
	}
	var matches []imageInfo
	for _, im := range all {
		name, _ := imageRef(im.Configuration.Name)
		if refErr == nil && name == normalized {
			return im, nil
		}
		if id == im.ID || id == im.Configuration.Descriptor.Digest {
			matches = append(matches, im)
		}
	}
	if len(matches) > 1 {
		return zero, errors.New("image ID has multiple references; specify an exact image tag")
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	return zero, docker.ErrImageNotFound
}
func nativeVariant(im imageInfo) (imageVariant, error) {
	for _, v := range im.Variants {
		if v.Platform.Architecture == "arm64" && v.Platform.OS == "linux" {
			return v, nil
		}
	}
	return imageVariant{}, errors.New("image has no locally available linux/arm64 variant; pull it before creating a sandbox")
}
func (c *Client) PullImage(ctx context.Context, image string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	name, err := imageRef(image)
	if err != nil {
		return err
	}
	_, err = c.run(ctx, nil, "image", "pull", "--platform", "linux/arm64", name)
	return err
}
func (c *Client) ListImages(ctx context.Context) ([]models.ImageSummary, error) {
	all, err := c.images(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]models.ImageSummary, 0, len(all))
	for _, im := range all {
		var size int64
		for _, v := range im.Variants {
			size += v.Size
		}
		out = append(out, models.ImageSummary{ID: im.Configuration.Descriptor.Digest, Tags: []string{im.Configuration.Name}, Size: size})
	}
	return out, nil
}
func (c *Client) InspectImage(ctx context.Context, id string) (models.ImageDetail, error) {
	im, err := c.findImage(ctx, id)
	if err != nil {
		return models.ImageDetail{}, err
	}
	b, err := c.run(ctx, nil, "image", "inspect", im.Configuration.Name)
	if err != nil {
		return models.ImageDetail{}, err
	}
	var result []imageInfo
	if err := json.Unmarshal(b, &result); err != nil || len(result) != 1 {
		return models.ImageDetail{}, errors.New("invalid Apple image inspect JSON")
	}
	im = result[0]
	v, err := nativeVariant(im)
	if err != nil {
		if len(im.Variants) == 0 {
			return models.ImageDetail{}, err
		}
		v = im.Variants[0]
	}
	return models.ImageDetail{ID: im.Configuration.Descriptor.Digest, Tags: []string{im.Configuration.Name}, Size: v.Size, Created: v.Config.Created, Architecture: v.Platform.Architecture, OS: v.Platform.OS}, nil
}
func (c *Client) RemoveImage(ctx context.Context, id string, force bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	im, err := c.findImage(ctx, id)
	if force && errors.Is(err, docker.ErrImageNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	// Apple --force only ignores a missing reference. Never emulate Docker's
	// force semantics by removing dependent containers or other image tags.
	args := []string{"image", "delete"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, im.Configuration.Name)
	_, err = c.run(ctx, nil, args...)
	return err
}
