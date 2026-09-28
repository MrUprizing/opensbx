package database

import "gorm.io/gorm"

// FindRoutingState reads public ownership and its pending work in one short DB
// snapshot. Admission policy belongs to the service, not to native adapters.
func (r *Repository) FindRoutingState(name, runtime string) (owner *Sandbox, pending bool, err error) {
	err = r.db.Transaction(func(tx *gorm.DB) error {
		var err error
		owner, err = (&Repository{db: tx}).FindByName(name)
		if err != nil || owner == nil {
			return err
		}
		var count int64
		// Public IDs bind ownership across the DB. Native references and attempt
		// tokens are scoped to the selected runtime, including legacy rows whose
		// RuntimeKind is empty and creates whose native-ID binding failed.
		if err := tx.Model(&Operation{}).Where(
			"public_id = ? OR (runtime_kind = ? AND (native_id = ? OR (token <> '' AND token = ?)))",
			owner.ID, runtime, owner.RuntimeID(), owner.AttemptToken,
		).Count(&count).Error; err != nil {
			return err
		}
		pending = count != 0
		return nil
	})
	return
}
