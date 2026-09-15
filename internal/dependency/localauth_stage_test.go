package dependency

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalAuthorizationChecksStagingIdentityBeforePromotion(t *testing.T) {
	for _, boundary := range []string{"stage", "activation"} {
		for _, change := range []string{"stable", "identical-replacement", "changed-replacement", "symlink"} {
			t.Run(boundary+"/"+change, func(t *testing.T) {
				project, source, service, _ := localFixture(t)
				result, err := service.InstallLocal(context.Background(), project, source, false)
				if err != nil {
					t.Fatal(err)
				}
				identity := result.Dependencies[0].Source
				filename, _, err := localAuthorizationPath(project, identity)
				if err != nil {
					t.Fatal(err)
				}
				original := readTestFile(t, filename)
				calls := 0
				var foreign os.FileInfo
				var foreignPath, foreignBytes string
				marker := filepath.Join(project, "completed-operation")
				err = changeLocalAuthorizationWith(project, identity, &localAuthorization{SchemaVersion: 1, Path: source, SourceRoot: source}, func() error {
					return os.WriteFile(marker, []byte("completed\n"), 0o644)
				}, func(directory *localAuthorizationDirectory, name string, data []byte) error {
					return writeAuthorizationWithStageHook(directory, name, data, func(temporary string) error {
						calls++
						selected := boundary == "stage" && calls == 1 || boundary == "activation" && calls == 2
						if !selected || change == "stable" {
							return nil
						}
						foreignPath = filepath.Join(filepath.Dir(filename), temporary)
						if err := directory.root().Rename(temporary, temporary+"-displaced"); err != nil {
							return err
						}
						foreignBytes = string(data)
						if change == "changed-replacement" {
							foreignBytes = "concurrent owner bytes\n"
						}
						if change == "symlink" {
							target := filepath.Join(t.TempDir(), "foreign")
							if err := os.WriteFile(target, []byte(foreignBytes), 0o600); err != nil {
								return err
							}
							if err := os.Symlink(target, foreignPath); err != nil {
								return err
							}
						} else if err := os.WriteFile(foreignPath, []byte(foreignBytes), 0o600); err != nil {
							return err
						}
						var err error
						foreign, err = os.Lstat(foreignPath)
						return err
					})
				})
				if change == "stable" {
					if err != nil || calls != 2 {
						t.Fatalf("stable activation: calls=%d err=%v", calls, err)
					}
					if readTestFile(t, marker) != "completed\n" {
						t.Fatal("project operation did not complete")
					}
					return
				}
				if !errors.Is(err, errAuthorizationStageChanged) {
					t.Fatalf("lost staging identity cause: %v", err)
				}
				if boundary == "activation" {
					if !strings.Contains(err.Error(), "project operation completed") || !strings.Contains(err.Error(), "state may have changed") {
						t.Fatalf("lost partial-state report: %v", err)
					}
					if readTestFile(t, marker) != "completed\n" {
						t.Fatal("lost completed project operation")
					}
				} else if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("project ran after failed staging: %v", err)
				}
				current, statErr := os.Lstat(foreignPath)
				if statErr != nil || !os.SameFile(foreign, current) || readTestFile(t, foreignPath) != foreignBytes {
					t.Fatalf("removed or changed foreign staging path: %v", statErr)
				}
				current, statErr = os.Lstat(filename)
				if statErr != nil || os.SameFile(foreign, current) || readTestFile(t, filename) != original {
					t.Fatalf("promoted foreign staging inode or failed rollback: %v", statErr)
				}
			})
		}
	}
}
