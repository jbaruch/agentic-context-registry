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
		for _, change := range []string{"stable", "identical-replacement", "changed-replacement", "symlink", "in-place-content", "in-place-permissions"} {
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
				var foreignMode os.FileMode
				marker := filepath.Join(project, "completed-operation")
				err = changeLocalAuthorizationWith(project, identity, localAuthorizationFixture(t, project, source), func() error {
					return os.WriteFile(marker, []byte("completed\n"), 0o644)
				}, func(directory *localAuthorizationDirectory, name string, data []byte) error {
					return writeAuthorizationWithStageHook(directory, name, data, func(temporary string) error {
						calls++
						selected := boundary == "stage" && calls == 1 || boundary == "activation" && calls == 2
						if !selected || change == "stable" {
							return nil
						}
						foreignPath = filepath.Join(filepath.Dir(filename), temporary)
						if strings.HasPrefix(change, "in-place-") {
							before, err := os.Lstat(foreignPath)
							if err != nil {
								return err
							}
							foreignBytes = string(data)
							if change == "in-place-content" {
								foreignBytes = strings.Replace(foreignBytes, `"schemaVersion":1`, `"schemaVersion":2`, 1)
								err = os.WriteFile(foreignPath, []byte(foreignBytes), 0o600)
							} else {
								err = os.Chmod(foreignPath, 0o644)
							}
							if err != nil {
								return err
							}
							foreign, err = os.Lstat(foreignPath)
							if err == nil {
								foreignMode = foreign.Mode()
								if !os.SameFile(before, foreign) {
									t.Fatal("in-place mutation replaced its inode")
								}
							}
							return err
						}
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
						if err == nil {
							foreignMode = foreign.Mode()
						}
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
					if _, err := authorizeLocal(project, Declaration{Source: identity, Requested: RequestedLocal, Path: source}); err != nil {
						t.Fatalf("successful activation left unusable authorization: %v", err)
					}
					return
				}
				if !errors.Is(err, errAuthorizationStageChanged) {
					t.Fatalf("lost staging identity cause: %v", err)
				}
				if change == "in-place-permissions" && !strings.Contains(err.Error(), "permissions 0600") {
					t.Fatalf("lost permission failure cause: %v", err)
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
				if statErr != nil || !os.SameFile(foreign, current) || current.Mode() != foreignMode || readTestFile(t, foreignPath) != foreignBytes {
					t.Fatalf("removed or changed foreign staging path: %v", statErr)
				}
				if _, err := authorizeLocal(project, Declaration{Source: identity, Requested: RequestedLocal, Path: source}); err != nil {
					t.Fatalf("failed staging did not retain usable original authorization: %v", err)
				}
				current, statErr = os.Lstat(filename)
				if statErr != nil || os.SameFile(foreign, current) || readTestFile(t, filename) != original {
					t.Fatalf("promoted foreign staging inode or failed rollback: %v", statErr)
				}
			})
		}
	}
}

func TestLocalAuthorizationPreservesStagingOnEarlyRefusal(t *testing.T) {
	for _, boundary := range []string{"initial", "activation"} {
		for _, refusal := range []string{"record", "ancestor", "hook"} {
			for _, mutation := range []string{"unchanged", "content", "mode"} {
				t.Run(boundary+"/"+refusal+"/"+mutation, func(t *testing.T) {
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
					marker := filepath.Join(project, "completed-operation")
					hookCause := errors.New("after-close failure")
					calls := 0
					var stagePath, stageBytes, recordPath, recordBytes string
					var stageInfo, recordInfo os.FileInfo
					err = changeLocalAuthorizationWith(project, identity, localAuthorizationFixture(t, project, source), func() error {
						return os.WriteFile(marker, []byte("completed\n"), 0o644)
					}, func(directory *localAuthorizationDirectory, name string, data []byte) error {
						return writeAuthorizationWithStageHook(directory, name, data, func(temporary string) error {
							calls++
							if !(boundary == "initial" && calls == 1 || boundary == "activation" && calls == 2) {
								return nil
							}
							stagePath = filepath.Join(filepath.Dir(filename), temporary)
							before, err := os.Lstat(stagePath)
							if err != nil {
								return err
							}
							stageBytes = string(data)
							switch mutation {
							case "content":
								stageBytes = strings.Replace(stageBytes, `"schemaVersion":1`, `"schemaVersion":2`, 1)
								if stageBytes == string(data) {
									t.Fatal("content mutation did not change bytes")
								}
								err = os.WriteFile(stagePath, []byte(stageBytes), 0o600)
							case "mode":
								err = os.Chmod(stagePath, 0o644)
							}
							if err != nil {
								return err
							}
							stageInfo, err = os.Lstat(stagePath)
							if err != nil {
								return err
							}
							if !os.SameFile(before, stageInfo) {
								t.Fatal("in-place mutation replaced inode")
							}
							if refusal == "hook" {
								return hookCause
							}
							recordBytes = readTestFile(t, filename)
							recordPath = filename
							if refusal == "record" {
								before, err := os.Lstat(filename)
								if err != nil {
									return err
								}
								if err := os.WriteFile(filename+"-replacement", []byte(recordBytes), 0o600); err != nil {
									return err
								}
								if err := os.Rename(filename+"-replacement", filename); err != nil {
									return err
								}
								recordInfo, err = os.Lstat(filename)
								if err == nil && os.SameFile(before, recordInfo) {
									t.Fatal("record replacement retained inode")
								}
								return err
							}
							parent := filepath.Dir(filename)
							if err := os.Rename(parent, parent+"-displaced"); err != nil {
								return err
							}
							stagePath = filepath.Join(parent+"-displaced", temporary)
							recordPath = filepath.Join(parent+"-displaced", name)
							if err := os.Mkdir(parent, 0o700); err != nil {
								return err
							}
							recordInfo, err = os.Lstat(recordPath)
							return err
						})
					})
					if err == nil {
						t.Fatal("early refusal succeeded")
					}
					cause := "authorization " + refusal + " changed concurrently"
					if refusal == "ancestor" {
						cause = "authorization directory changed concurrently"
					}
					if refusal == "hook" {
						if !errors.Is(err, hookCause) {
							t.Fatalf("lost original hook cause: %v", err)
						}
						if readTestFile(t, filename) != original {
							t.Fatal("failed to restore prior authorization")
						}
						if _, err := authorizeLocal(project, Declaration{Source: identity, Requested: RequestedLocal, Path: source}); err != nil {
							t.Fatalf("prior authorization unusable: %v", err)
						}
					} else {
						if !strings.Contains(err.Error(), cause) {
							t.Fatalf("lost original refusal: %v", err)
						}
						current, inspectErr := os.Lstat(recordPath)
						if inspectErr != nil || !os.SameFile(recordInfo, current) || current.Mode() != recordInfo.Mode() || readTestFile(t, recordPath) != recordBytes {
							t.Fatalf("changed concurrent/displaced record: %v", inspectErr)
						}
						if refusal == "ancestor" {
							entries, readErr := os.ReadDir(filepath.Dir(filename))
							if readErr != nil || len(entries) != 0 {
								t.Fatalf("wrote into replacement ancestor: %v %v", entries, readErr)
							}
						}
					}
					if boundary == "activation" {
						if !strings.Contains(err.Error(), "project operation completed") || !strings.Contains(err.Error(), "state may have changed") || readTestFile(t, marker) != "completed\n" {
							t.Fatalf("lost completed project state or warning: %v", err)
						}
					} else if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("project ran after initial refusal: %v", err)
					}
					current, inspectErr := os.Lstat(stagePath)
					if mutation == "unchanged" {
						if !errors.Is(inspectErr, os.ErrNotExist) {
							t.Fatalf("unchanged owned stage not cleaned: %v", inspectErr)
						}
					} else {
						if !errors.Is(err, errAuthorizationStageChanged) {
							t.Errorf("lost staging cause: %v", err)
						}
						if inspectErr != nil || !os.SameFile(stageInfo, current) || current.Mode() != stageInfo.Mode() || readTestFile(t, stagePath) != stageBytes {
							t.Fatalf("modified staging not preserved: %v", inspectErr)
						}
					}
				})
			}
		}
	}
}
