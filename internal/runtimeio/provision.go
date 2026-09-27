package runtimeio

import (
	"context"
	"errors"
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

func (p *provisioned) Sandbox() sandbox.Created { return p.created }
func (p *provisioned) Adopt(ctx context.Context, identity sandbox.Provenance) error {
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
	return p.a.repo.Save(*row)
}
func (p *provisioned) Rollback(ctx context.Context) error {
	// This private handle is bound to the exact successful Create result, not
	// an ID supplied by an API caller or read from inconsistent database state.
	if err := p.a.engine.DiscardCreated(ctx, p.native); err != nil {
		return err
	}
	return errors.Join(p.a.repo.DeleteCommandsBySandbox(string(p.public)), p.a.repo.Delete(string(p.public)))
}
func (p *provisioned) Recover(ctx context.Context, identity sandbox.Provenance, cause error) error {
	row, err := p.a.repo.FindByID(string(p.public))
	if err != nil {
		return err
	}
	if row == nil {
		row = &database.Sandbox{ID: string(p.public), Name: p.created.Name}
	}
	row.NativeID = p.native
	row.Image = identity.Root
	row.ImageRoot = identity.Root
	row.ImageManifest = identity.Manifest
	row.NativeImage = p.image.ref
	row.CacheVersion = identity.CacheVersion
	row.RecoveryError = cause.Error()
	return p.a.repo.Save(*row)
}
