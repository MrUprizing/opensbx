package sandbox

import (
	"context"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

type SandboxID string
type Platform struct{ OS, Architecture, Variant string }
type Port struct {
	Number   uint16
	Protocol string
}
type ResourceLimits struct {
	MemoryMB int64
	CPUs     float64
}
type Capabilities struct {
	Runtime, Version     string
	Platform             Platform
	Pause, FractionalCPU bool
}

// Cache is private materialization, not image catalog lifecycle. A handle must
// identify verified content, never a caller's mutable registry tag.
type Cache interface {
	Capabilities(context.Context) (Capabilities, error)
	Materialize(context.Context, Image) (PreparedImage, error)
}
type Image struct {
	RootDigest, ManifestDigest, ConfigDigest string
	Platform                                 Platform
	Content                                  v1.Image
	Validate                                 func(context.Context) error
	// Archive writes a complete selected-platform archive to a new path.
	Archive func(path, reference string) error
}
