package database

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"opensbx/internal/sandbox"

	"gorm.io/gorm"
)

func (r *Repository) WithContext(ctx context.Context) *Repository {
	return &Repository{db: r.db.WithContext(ctx), native: r.native}
}

func (r *Repository) Operations(runtime string) ([]Operation, error) {
	var ops []Operation
	err := r.db.Where("runtime_kind = ?", runtime).Order("id").Find(&ops).Error
	return ops, err
}

func (r *Repository) FindOperation(id string) (*Operation, error) {
	var op Operation
	err := r.db.First(&op, "id = ?", id).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	return &op, err
}

func (r *Repository) Pending(runtime, native string) (*Operation, error) {
	// Binding a create response can itself fail. Match its public identity or
	// attempt token too, so a later mutation cannot bypass that older intent.
	owner, err := r.FindByNativeID(native)
	if err != nil {
		return nil, err
	}
	query := r.db.Where("runtime_kind = ?", runtime)
	if owner == nil {
		query = query.Where("native_id = ?", native)
	} else {
		query = query.Where("(native_id = ? OR public_id = ? OR (token <> '' AND token = ?))", native, owner.ID, owner.AttemptToken)
	}
	var op Operation
	err = query.First(&op).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	return &op, err
}

func (r *Repository) RequireIdle(runtime, native string) error {
	row, err := r.FindByNativeID(native)
	if err != nil {
		return err
	}
	if row == nil || (row.RuntimeKind != "" && row.RuntimeKind != runtime) {
		return sandbox.ErrNotFound
	}
	op, err := r.Pending(runtime, native)
	if err != nil {
		return err
	}
	if op != nil {
		return fmt.Errorf("sandbox %s has pending %s; run startup recovery", row.ID, op.Kind)
	}
	return nil
}

func (r *Repository) FindByAttempt(runtime, token string) (*Sandbox, error) {
	if token == "" {
		return nil, nil
	}
	var row Sandbox
	err := r.db.Where("runtime_kind = ? AND attempt_token = ?", runtime, token).First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	return &row, err
}

// ValidateOperation binds recovery to the durable attempt and current owner.
// It must run before native effects; a pending intent is not a license to act
// on an unrelated row that happens to reuse a public identity.
func (r *Repository) ValidateOperation(op Operation) error {
	var stored Operation
	if err := r.db.First(&stored, "id = ?", op.ID).Error; err != nil {
		return err
	}
	if stored.Kind != op.Kind || stored.RuntimeKind != op.RuntimeKind || stored.Token != op.Token {
		return fmt.Errorf("operation %s changed", op.ID)
	}
	if op.PublicID == "" && op.Kind == "create" {
		return nil
	}
	row, err := r.PublicView().FindByID(op.PublicID)
	if err != nil {
		return err
	}
	if row == nil {
		if op.Kind == "create" {
			return nil
		}
		return fmt.Errorf("operation %s lacks ownership metadata", op.ID)
	}
	if (op.NativeID != "" && row.RuntimeID() != op.NativeID) || (row.RuntimeKind != "" && row.RuntimeKind != op.RuntimeKind) || row.AttemptToken != op.Token {
		return fmt.Errorf("operation %s ownership changed", op.ID)
	}
	return nil
}

func (r *Repository) BeginCreate(op Operation) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		if op.PublicID != "" {
			var owner Sandbox
			err := tx.First(&owner, "id = ?", op.PublicID).Error
			if err == nil {
				return fmt.Errorf("sandbox %s already has ownership metadata", op.PublicID)
			}
			if err != gorm.ErrRecordNotFound {
				return err
			}
			var count int64
			if err := tx.Model(&Operation{}).Where("public_id = ?", op.PublicID).Count(&count).Error; err != nil {
				return err
			}
			if count != 0 {
				return fmt.Errorf("sandbox %s has pending recovery work", op.PublicID)
			}
		}
		return tx.Create(&op).Error
	})
}

// BeginOperation requires durable ownership and refuses to overwrite pending work.
// Repeating delete/stop may safely finish the same idempotent operation.
func (r *Repository) BeginOperation(runtime, native, kind string, deadline *time.Time) (*Operation, error) {
	var op Operation
	err := r.db.Transaction(func(tx *gorm.DB) error {
		repo := &Repository{db: tx, native: true}
		row, err := repo.FindByNativeID(native)
		if err != nil {
			return err
		}
		if row == nil {
			return sandbox.ErrNotFound
		}
		if row.RuntimeKind != "" && row.RuntimeKind != runtime {
			return fmt.Errorf("sandbox runtime mismatch")
		}
		pending, err := repo.Pending(runtime, native)
		if err != nil {
			return err
		}
		if pending != nil {
			if err := repo.ValidateOperation(*pending); err != nil {
				return err
			}
			if kind == "delete" && pending.Kind == "create" {
				op = *pending
				if row.AttemptToken != op.Token {
					return fmt.Errorf("creation ownership mismatch")
				}
				op.Kind = "delete"
				op.NativeID = native
				op.PublicID = row.ID
				return tx.Model(&Operation{}).Where("id = ?", op.ID).Updates(map[string]any{"kind": op.Kind, "native_id": native, "public_id": row.ID}).Error
			}
			if pending.Kind == kind && (kind == "delete" || kind == "stop") {
				op = *pending
				return nil
			}
			return fmt.Errorf("sandbox %s has pending %s; run startup recovery before another mutation", row.ID, pending.Kind)
		}
		op = Operation{ID: runtime + ":" + rand.Text(), RuntimeKind: runtime, PublicID: row.ID, NativeID: native, Token: row.AttemptToken, Kind: kind, Deadline: deadline}
		return tx.Create(&op).Error
	})
	return &op, err
}

func (r *Repository) BindCreation(op *Operation, native string) error {
	public := op.PublicID
	if public == "" {
		public = native
	}
	if err := r.db.Model(&Operation{}).Where("id = ?", op.ID).Updates(map[string]any{"native_id": native, "public_id": public}).Error; err != nil {
		return err
	}
	op.NativeID = native
	op.PublicID = public
	return nil
}

func (r *Repository) ClearOperation(op Operation) error {
	return r.db.Delete(&Operation{}, "id = ?", op.ID).Error
}

func (r *Repository) CommitCreation(op Operation, row Sandbox, awaitAdoption bool) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		if awaitAdoption {
			return nil
		}
		return tx.Delete(&Operation{}, "id = ?", op.ID).Error
	})
}

// CompleteOperation atomically persists the observed state and retires its intent.
func (r *Repository) CompleteOperation(op Operation, ports JSONMap, deadline *time.Time) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		repo := &Repository{db: tx}
		if err := repo.ValidateOperation(op); err != nil {
			return err
		}
		if ports != nil {
			if err := repo.UpdatePorts(op.PublicID, ports); err != nil {
				return err
			}
		}
		result := tx.Model(&Sandbox{}).Where("id = ? AND (native_id = ? OR ((native_id = '' OR native_id IS NULL) AND id = ?))", op.PublicID, op.NativeID, op.NativeID).Updates(map[string]any{"expires_at": deadline, "runtime_kind": op.RuntimeKind})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("operation %s ownership changed", op.ID)
		}
		return repo.ClearOperation(op)
	})
}

// DeleteOperation is idempotent, including recovery of an uncommitted creation.
func (r *Repository) DeleteOperation(op Operation) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		repo := &Repository{db: tx}
		if op.PublicID != "" {
			row, err := repo.FindByID(op.PublicID)
			if err != nil {
				return err
			}
			if row != nil {
				if row.RuntimeID() != op.NativeID || (row.RuntimeKind != "" && row.RuntimeKind != op.RuntimeKind) || row.AttemptToken != op.Token {
					return fmt.Errorf("operation %s ownership changed", op.ID)
				}
				if err := repo.DeleteSandbox(row.ID); err != nil {
					return err
				}
			}
		}
		return repo.ClearOperation(op)
	})
}

func (r *Repository) SetDeadline(id, runtime string, deadline *time.Time) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		repo := &Repository{db: tx, native: true}
		row, err := repo.FindByNativeID(id)
		if err != nil {
			return err
		}
		if row == nil {
			return sandbox.ErrNotFound
		}
		if row.RuntimeKind != "" && row.RuntimeKind != runtime {
			return fmt.Errorf("sandbox runtime mismatch")
		}
		op, err := repo.Pending(runtime, id)
		if err != nil {
			return err
		}
		if op != nil {
			return fmt.Errorf("sandbox %s has pending %s; run startup recovery", row.ID, op.Kind)
		}
		return tx.Model(&Sandbox{}).Where("id = ?", row.ID).Updates(map[string]any{"expires_at": deadline, "runtime_kind": runtime}).Error
	})
}

// UpdateProvenance never upserts a deleted owner or writes a stale deadline.
func (r *Repository) UpdateProvenance(row Sandbox, adopt bool) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		var pending int64
		if err := tx.Model(&Operation{}).Where("public_id = ? AND kind <> ?", row.ID, "create").Count(&pending).Error; err != nil {
			return err
		}
		if adopt && pending != 0 {
			return fmt.Errorf("sandbox %s has pending lifecycle work", row.ID)
		}
		result := tx.Model(&Sandbox{}).Where("id = ? AND native_id = ?", row.ID, row.NativeID).Updates(map[string]any{"image": row.Image, "image_root": row.ImageRoot, "image_manifest": row.ImageManifest, "native_image": row.NativeImage, "cache_version": row.CacheVersion, "recovery_error": row.RecoveryError})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("sandbox %s ownership disappeared or changed", row.ID)
		}
		if adopt {
			return tx.Where("public_id = ? AND native_id = ? AND kind = ?", row.ID, row.NativeID, "create").Delete(&Operation{}).Error
		}
		return nil
	})
}
