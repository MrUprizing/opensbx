package applecontainer

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"gorm.io/gorm"
	"opensbx/internal/database"
	"opensbx/internal/runtimeio"
	"opensbx/internal/sandbox"
)

func TestCreateOwnershipFailureAndRollbackFailureAttemptRecoveryPersistence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		persistent bool
	}{
		{name: "transient ownership write failure"},
		{name: "persistent ownership write failure", persistent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := database.New(t.TempDir() + "/apple-recovery.db")
			sqlDB, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sqlDB.Close() })
			repo := database.NewRepository(db)
			ownershipAttempts := 0
			if err := db.Callback().Create().Before("gorm:create").Register("test:fail-native-ownership-write", func(tx *gorm.DB) {
				if tx.Statement.Table != "sandboxes" {
					return
				}
				ownershipAttempts++
				if ownershipAttempts == 1 {
					tx.AddError(errors.New("transient ownership insert failure"))
				} else if tc.persistent {
					tx.AddError(errors.New("recovery record insert failure"))
				}
			}); err != nil {
				t.Fatal(err)
			}

			const imageRef = "opensbx.invalid/cache@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
			const publicID = sandbox.SandboxID("sbx-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
			const imageRoot = "sha256:root-artifact"
			nativeID := ""
			rollbackErr := errors.New("native delete failed")
			runner := &scriptedRunner{t: t}
			runner.run = func(args []string, _ io.Reader, out, _ io.Writer) error {
				switch {
				case reflect.DeepEqual(args, []string{"image", "list", "--format", "json"}):
					_, _ = io.WriteString(out, `[{"ID":"sha256:native","Configuration":{"Name":"`+imageRef+`","Descriptor":{"Digest":"sha256:native"}},"Variants":[{"Platform":{"Architecture":"arm64","OS":"linux"},"Size":1}]}]`)
				case len(args) > 0 && args[0] == "create":
					if nativeID != "" || !validID(args[4]) {
						t.Fatalf("native create used unexpected/repeated ID: %v", args)
					}
					nativeID = args[4]
				case len(args) == 2 && args[0] == "start":
					if args[1] != nativeID {
						t.Fatalf("start ID=%q want exact created native ID %q", args[1], nativeID)
					}
				case reflect.DeepEqual(args, []string{"list", "--all", "--format", "json"}):
					if nativeID == "" {
						t.Fatal("rollback inventory requested before native creation")
					}
					_, _ = io.WriteString(out, listJSON(nativeID, "running", "[]"))
				case len(args) == 3 && reflect.DeepEqual(args[:2], []string{"delete", "--force"}):
					if args[2] != nativeID {
						t.Fatalf("rollback deleted %q, want exact native resource %q", args[2], nativeID)
					}
					return rollbackErr
				default:
					t.Fatalf("unexpected Apple CLI call: %#v", args)
				}
				return nil
			}
			client := New(repo, runner, nil)
			ctx := sandbox.WithCreationImage(sandbox.WithCreationID(context.Background(), publicID), imageRoot)
			_, err = client.Create(ctx, runtimeio.CreateSandboxRequest{Image: imageRef})
			if err == nil || !strings.Contains(err.Error(), "transient ownership insert failure") || !strings.Contains(err.Error(), "CLI execution failed") {
				t.Fatalf("Create() did not preserve ownership and rollback errors: %v", err)
			}
			row, findErr := repo.FindByID(string(publicID))
			if tc.persistent {
				if ownershipAttempts != 2 || !strings.Contains(err.Error(), "recovery record insert failure") {
					t.Fatalf("persistent recovery write was not attempted/joined: writes=%d err=%v calls=%v", ownershipAttempts, err, runner.calls)
				}
				if findErr != nil || row != nil {
					t.Fatalf("persistent database failure unexpectedly left row=%+v err=%v", row, findErr)
				}
				return
			}
			if nativeID == "" || ownershipAttempts != 2 || findErr != nil || row == nil {
				t.Fatalf("transient CreateOwnership + rollback failure did not attempt/persist recovery ownership: writes=%d nativeID=%q row=%+v lookupErr=%v createErr=%v calls=%v", ownershipAttempts, nativeID, row, findErr, err, runner.calls)
			}
			if row.ID != string(publicID) || row.NativeID != nativeID || row.Image != imageRoot {
				t.Fatalf("recovery identity lost public/native/artifact identity: %+v", row)
			}
			if row.ImageRoot != imageRoot || row.ImageManifest != "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" || row.NativeImage != imageRef {
				t.Fatalf("recovery provenance is incomplete: %+v", row)
			}
			if !strings.Contains(row.RecoveryError, "transient ownership insert failure") || !strings.Contains(row.RecoveryError, "CLI execution failed") {
				t.Fatalf("recovery error omitted original/cleanup causes: %q", row.RecoveryError)
			}
		})
	}
}
