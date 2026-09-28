package runtimeio

import (
	"context"
	"fmt"
	"opensbx/internal/database"
	"opensbx/internal/sandbox"
)

type provisioned struct {
	a       *Adapter
	public  sandbox.SandboxID
	native  string
	image   *prepared
	created sandbox.Created
}

type adoptionKey struct{}

// AwaitAdoption keeps a runtime create intent alive through the service boundary.
func AwaitAdoption(ctx context.Context) bool {
	value, _ := ctx.Value(adoptionKey{}).(bool)
	return value
}

func (p *provisioned) Sandbox() sandbox.Created { return p.created }
func (p *provisioned) Adopt(ctx context.Context, identity sandbox.Provenance) (adoptErr error) {
	defer func() {
		if adoptErr != nil {
			if failed, ok := p.a.engine.(interface{ CreationFailed(string) }); ok {
				failed.CreationFailed(p.native)
			}
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	row, err := p.a.repo.FindByID(string(p.public))
	if err != nil {
		return err
	}
	if row == nil {
		return fmt.Errorf("created sandbox %s lacks ownership metadata", p.public)
	}
	if row.NativeID != p.native {
		return fmt.Errorf("sandbox %s native identity mismatch", p.public)
	}
	if identity.Root != p.image.identity.Root || identity.Manifest != p.image.identity.Manifest {
		return fmt.Errorf("sandbox image identity mismatch")
	}
	row.ImageRoot = identity.Root
	row.ImageManifest = identity.Manifest
	row.Image = identity.Root
	row.NativeImage = p.image.ref
	row.CacheVersion = identity.CacheVersion
	if adopter, ok := p.a.engine.(interface {
		AdoptCreation(context.Context, database.Sandbox) error
	}); ok {
		return adopter.AdoptCreation(ctx, *row)
	}
	return p.a.repo.UpdateProvenance(*row, true)
}
func (p *provisioned) Rollback(ctx context.Context) error {
	// This private handle is bound to the exact successful Create result, not
	// an ID supplied by an API caller or read from inconsistent database state.
	return p.a.engine.DiscardCreated(ctx, p.native)
}
func (p *provisioned) Recover(ctx context.Context, identity sandbox.Provenance, cause error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	row, err := p.a.repo.FindByID(string(p.public))
	if err != nil {
		return err
	}
	if row == nil {
		return fmt.Errorf("sandbox %s ownership disappeared during recovery", p.public)
	}
	row.NativeID = p.native
	row.Image = identity.Root
	row.ImageRoot = identity.Root
	row.ImageManifest = identity.Manifest
	row.NativeImage = p.image.ref
	row.CacheVersion = identity.CacheVersion
	row.RecoveryError = cause.Error()
	return p.a.repo.UpdateProvenance(*row, false)
}
