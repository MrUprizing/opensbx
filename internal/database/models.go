package database

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"
)

// JSONMap is a map[string]string that serializes to/from JSON in SQLite.
type JSONMap map[string]string

func (j JSONMap) Value() (driver.Value, error) {
	if j == nil {
		return "null", nil
	}
	b, err := json.Marshal(j)
	return string(b), err
}

func (j *JSONMap) Scan(src any) error {
	if src == nil {
		*j = nil
		return nil
	}
	var bytes []byte
	switch v := src.(type) {
	case string:
		bytes = []byte(v)
	case []byte:
		bytes = v
	default:
		return fmt.Errorf("unsupported type for JSONMap: %T", src)
	}
	return json.Unmarshal(bytes, j)
}

// Sandbox persists the container ID, metadata, and its assigned host ports.
type Sandbox struct {
	ID            string     `gorm:"primaryKey"` // Stable public ID; legacy IDs are retained.
	NativeID      string     // Private backend reference; empty on legacy rows means ID.
	ImageRoot     string     // Canonical OCI root, empty for unadopted legacy images.
	ImageManifest string     // Selected platform manifest, distinct from native image ID.
	NativeImage   string     // Verified private cache handle.
	CacheVersion  string     // Runtime version and cache translation provenance.
	RecoveryError string     // Private post-create compensation failure; never a public DTO.
	RuntimeKind   string     // Empty for legacy rows; recovery never guesses their runtime.
	AttemptToken  string     // Native ownership label for newly managed resources.
	ExpiresAt     *time.Time // Absolute deadline, not the next transient retry time.
	Name          string
	Image         string
	Ports         JSONMap `gorm:"type:json"` // e.g. {"3000/tcp": "32768"}
	Port          string  // container port exposed, e.g. "3000/tcp"
}

// Operation is a write-ahead intent, not evidence that a native call completed.
// NativeName and Token identify even a create whose response was lost.
type Operation struct {
	ID          string `gorm:"primaryKey"`
	RuntimeKind string `gorm:"index"`
	PublicID    string `gorm:"index"`
	NativeID    string `gorm:"index"`
	NativeName  string
	Token       string
	Kind        string
	Deadline    *time.Time
}

// Command persists an executed command's metadata and result.
type Command struct {
	ID         string `gorm:"primaryKey"` // cmd_<hex>
	SandboxID  string `gorm:"index"`      // container ID
	Name       string // executable name
	Args       string `gorm:"type:json"` // JSON-encoded []string
	Cwd        string // working directory
	ExitCode   *int   // nil while running
	StartedAt  int64  // unix milliseconds
	FinishedAt *int64 // unix milliseconds
}
