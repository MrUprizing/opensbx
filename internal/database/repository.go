package database

import (
	"gorm.io/gorm"
)

// Repository provides CRUD operations for persisted sandboxes.
type Repository struct {
	db     *gorm.DB
	native bool
}

// NativeView translates only the adapter's view. Stored primary keys and
// command backreferences remain public IDs. Public lookups never accept aliases.
func (r *Repository) NativeView() *Repository { return &Repository{db: r.db, native: true} }
func (r *Repository) PublicView() *Repository { return &Repository{db: r.db} }
func (s Sandbox) RuntimeID() string {
	if s.NativeID != "" {
		return s.NativeID
	}
	return s.ID
}

func (r *Repository) FindByNativeID(id string) (*Sandbox, error) {
	var s Sandbox
	err := r.db.Where("native_id = ? OR ((native_id = '' OR native_id IS NULL) AND id = ?)", id, id).First(&s).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}
func (r *Repository) storedID(id string) (string, error) {
	if !r.native {
		return id, nil
	}
	s, err := r.FindByNativeID(id)
	if err != nil {
		return "", err
	}
	if s != nil {
		return s.ID, nil
	}
	return id, nil
}
func (r *Repository) commandView(cmd *Command) error {
	if !r.native {
		return nil
	}
	var s Sandbox
	err := r.db.First(&s, "id = ?", cmd.SandboxID).Error
	if err == gorm.ErrRecordNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	cmd.SandboxID = s.RuntimeID()
	return nil
}

// NewRepository creates a Repository backed by the given database.
func NewRepository(db *gorm.DB) *Repository {
	return &Repository{db: db}
}

// Save creates or updates a sandbox record.
func (r *Repository) Save(s Sandbox) error {
	if r.native && (s.NativeID == "" || s.ID == s.NativeID) {
		stored, err := r.FindByNativeID(s.ID)
		if err != nil {
			return err
		}
		if stored != nil {
			s.ID = stored.ID
			if s.NativeID == "" {
				s.NativeID = stored.NativeID
			}
		}
	}
	return r.db.Save(&s).Error
}

// CreateOwnership is insert-only: a newly created native resource must never
// overwrite an existing public identity or its historical metadata.
func (r *Repository) CreateOwnership(s Sandbox) error { return r.db.Create(&s).Error }

// FindByID looks up a public ID, or a native reference in an adapter-only view.
func (r *Repository) FindByID(id string) (*Sandbox, error) {
	if r.native {
		s, err := r.FindByNativeID(id)
		if s != nil {
			s.ID = s.RuntimeID()
		}
		return s, err
	}
	var s Sandbox
	if err := r.db.First(&s, "id = ?", id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}

// FindAll returns all persisted sandboxes.
func (r *Repository) FindAll() ([]Sandbox, error) {
	var sandboxes []Sandbox
	if err := r.db.Find(&sandboxes).Error; err != nil {
		return nil, err
	}
	if r.native {
		for i := range sandboxes {
			sandboxes[i].ID = sandboxes[i].RuntimeID()
		}
	}
	return sandboxes, nil
}

// UpdatePorts updates the port mappings for an existing sandbox.
func (r *Repository) UpdatePorts(id string, ports JSONMap) error {
	id, err := r.storedID(id)
	if err != nil {
		return err
	}
	return r.db.Model(&Sandbox{}).Where("id = ?", id).Update("ports", ports).Error
}

// FindByName returns a sandbox by its name, or nil if not found.
func (r *Repository) FindByName(name string) (*Sandbox, error) {
	var s Sandbox
	if err := r.db.First(&s, "name = ?", name).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	if r.native {
		s.ID = s.RuntimeID()
	}
	return &s, nil
}

// Delete removes a sandbox record using the selected view's identity.
func (r *Repository) Delete(id string) error {
	id, err := r.storedID(id)
	if err != nil {
		return err
	}
	return r.db.Delete(&Sandbox{}, "id = ?", id).Error
}

// DeleteSandbox removes ownership and history in the same transaction.
func (r *Repository) DeleteSandbox(id string) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		repo := &Repository{db: tx, native: r.native}
		if err := repo.DeleteCommandsBySandbox(id); err != nil {
			return err
		}
		return repo.Delete(id)
	})
}

// SaveCommand creates a new command record.
func (r *Repository) SaveCommand(cmd Command) error {
	id, err := r.storedID(cmd.SandboxID)
	if err != nil {
		return err
	}
	cmd.SandboxID = id
	return r.db.Create(&cmd).Error
}

// FindCommandByID returns a command by ID, or nil if not found.
func (r *Repository) FindCommandByID(id string) (*Command, error) {
	var cmd Command
	if err := r.db.First(&cmd, "id = ?", id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	if err := r.commandView(&cmd); err != nil {
		return nil, err
	}
	return &cmd, nil
}

// FindCommandsBySandbox returns all commands for a sandbox, ordered by started_at.
func (r *Repository) FindCommandsBySandbox(sandboxID string) ([]Command, error) {
	sandboxID, err := r.storedID(sandboxID)
	if err != nil {
		return nil, err
	}
	var cmds []Command
	if err := r.db.Where("sandbox_id = ?", sandboxID).Order("started_at ASC").Find(&cmds).Error; err != nil {
		return nil, err
	}
	for i := range cmds {
		if err := r.commandView(&cmds[i]); err != nil {
			return nil, err
		}
	}
	return cmds, nil
}

// UpdateCommandFinished marks a command as finished with its exit code.
func (r *Repository) UpdateCommandFinished(id string, exitCode int, finishedAt int64) error {
	return r.db.Model(&Command{}).Where("id = ?", id).Updates(map[string]any{
		"exit_code":   exitCode,
		"finished_at": finishedAt,
	}).Error
}

// DeleteCommandsBySandbox removes all command records for a sandbox.
func (r *Repository) DeleteCommandsBySandbox(sandboxID string) error {
	sandboxID, err := r.storedID(sandboxID)
	if err != nil {
		return err
	}
	return r.db.Where("sandbox_id = ?", sandboxID).Delete(&Command{}).Error
}
