package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

type creationIDKey struct{}
type creationImageKey struct{}

func WithCreationImage(ctx context.Context, rootDigest string) context.Context {
	return context.WithValue(ctx, creationImageKey{}, rootDigest)
}
func CreationImage(ctx context.Context, native string) string {
	if root, ok := ctx.Value(creationImageKey{}).(string); ok && root != "" {
		return root
	}
	return native
}

func NewID() (SandboxID, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return SandboxID("sbx-" + hex.EncodeToString(b[:])), nil
}

// WithCreationID carries application-assigned identity without adding a public
// JSON request field. Adapters retain their independently allocated native ID.
func WithCreationID(ctx context.Context, id SandboxID) context.Context {
	return context.WithValue(ctx, creationIDKey{}, id)
}

// CreationID preserves the low-level adapter contract when called without an
// application identity; existing resources and their public IDs never change.
func CreationID(ctx context.Context, native string) string {
	if id, ok := ctx.Value(creationIDKey{}).(SandboxID); ok && id != "" {
		return string(id)
	}
	return native
}
