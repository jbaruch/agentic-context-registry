package dependency

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestLocalAuthorizationRetainsDirectoryAcrossBoundaries(t *testing.T) {
	for _, ancestor := range []string{"project-key", "local", "base"} {
		for _, destination := range []string{"project", "source", "concurrent-directory"} {
			for _, boundary := range []string{"before-stage", "after-stage", "after-project", "activation", "removal", "rollback"} {
				t.Run(ancestor+"/"+destination+"/"+boundary, func(t *testing.T) {
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
					replaced := filepath.Dir(filename)
					if ancestor == "local" {
						replaced = filepath.Dir(replaced)
					}
					if ancestor == "base" {
						replaced, err = filepath.EvalSymlinks(os.Getenv("ACR_STATE_HOME"))
						if err != nil {
							t.Fatal(err)
						}
					}
					relative, err := filepath.Rel(replaced, filename)
					if err != nil {
						t.Fatal(err)
					}
					if !filepath.IsLocal(relative) {
						t.Fatalf("fixture record escaped replaced ancestor: base=%q record=%q relative=%q", replaced, filename, relative)
					}
					targetRoot := project
					if destination == "source" {
						targetRoot = source
					}
					if destination == "concurrent-directory" {
						targetRoot = t.TempDir()
					}
					target := filepath.Join(targetRoot, "redirected-auth")
					displaced := filepath.Join(t.TempDir(), "displaced")
					var redirectedRecord string
					var saved []byte
					swap := func() error {
						var err error
						saved, err = os.ReadFile(filename)
						if err != nil {
							return err
						}
						redirectedRecord = filepath.Join(target, relative)
						if err := os.MkdirAll(filepath.Dir(redirectedRecord), 0o700); err != nil {
							return err
						}
						if err := os.WriteFile(redirectedRecord, saved, 0o600); err != nil {
							return err
						}
						if err := os.Rename(replaced, displaced); err != nil {
							return err
						}
						if destination == "concurrent-directory" {
							if err := os.Rename(target, replaced); err != nil {
								return err
							}
							redirectedRecord = filepath.Join(replaced, relative)
							return nil
						}
						return os.Symlink(target, replaced)
					}
					next := localAuthorizationFixture(t, project, source)
					if boundary == "removal" {
						next = nil
					}
					injected := errors.New("project operation failed")
					callbackRan := false
					writes := 0
					err = changeLocalAuthorizationWith(project, identity, next, func() error {
						callbackRan = true
						if boundary == "after-project" || boundary == "removal" || boundary == "rollback" {
							if err := swap(); err != nil {
								return err
							}
						}
						if boundary == "rollback" {
							return injected
						}
						return nil
					}, func(directory *localAuthorizationDirectory, name string, data []byte) error {
						writes++
						if boundary == "before-stage" && writes == 1 || boundary == "activation" && writes == 2 {
							if err := swap(); err != nil {
								return err
							}
						}
						if err := writeAuthorization(directory, name, data); err != nil {
							return err
						}
						if boundary == "after-stage" && writes == 1 {
							return swap()
						}
						return nil
					})
					if err == nil || !strings.Contains(err.Error(), "changed concurrently") {
						t.Fatalf("replacement succeeded: %v", err)
					}
					if boundary == "before-stage" || boundary == "after-stage" {
						if callbackRan {
							t.Fatal("project operation ran after directory replacement")
						}
					} else if boundary != "rollback" && !strings.Contains(err.Error(), "project operation completed") {
						t.Fatalf("missing partial-state report: %v", err)
					}
					if boundary == "rollback" && !errors.Is(err, injected) {
						t.Fatalf("lost project failure: %v", err)
					}
					if got := readTestFile(t, redirectedRecord); got != string(saved) {
						t.Fatal("modified redirected concurrent-owned record")
					}
					// No mutation follows the old absolute path; the displaced directory's
					// record also remains at the bytes it had when the replacement occurred.
					if got := readTestFile(t, filepath.Join(displaced, relative)); got != string(saved) {
						t.Fatal("modified displaced record after boundary failure")
					}
				})
			}
		}
	}
}

func TestLocalAuthorizationPreservesIdenticalConcurrentRecord(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "activation", true: "rollback"}[fail], func(t *testing.T) {
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
			var pending string
			var replacement os.FileInfo
			err = changeLocalAuthorization(project, identity, nil, func() error {
				pending = readTestFile(t, filename)
				if err := os.Rename(filename, filepath.Join(t.TempDir(), "original-record")); err != nil {
					return err
				}
				if err := os.WriteFile(filename, []byte(pending), 0o600); err != nil {
					return err
				}
				var err error
				replacement, err = os.Stat(filename)
				if err != nil {
					return err
				}
				if fail {
					return errors.New("project failed")
				}
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "concurrently") {
				t.Fatalf("adopted identical concurrent record: %v", err)
			}
			current, statErr := os.Stat(filename)
			if statErr != nil || !os.SameFile(current, replacement) || readTestFile(t, filename) != pending {
				t.Fatalf("changed concurrent record: %v", statErr)
			}
		})
	}
}

func TestLocalAuthorizationPreservesRecordReplacedBeforeWrite(t *testing.T) {
	for _, boundary := range []string{"stage", "after-stage", "activate", "rollback"} {
		t.Run(boundary, func(t *testing.T) {
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
			var contents string
			var concurrent os.FileInfo
			writes := 0
			operationRan := false
			injected := errors.New("project failure")
			err = changeLocalAuthorizationWith(project, identity, localAuthorizationFixture(t, project, source), func() error {
				operationRan = true
				if boundary == "rollback" {
					return injected
				}
				return nil
			}, func(directory *localAuthorizationDirectory, name string, data []byte) error {
				writes++
				if boundary == "stage" && writes == 1 || boundary == "after-stage" && writes == 1 || boundary != "stage" && boundary != "after-stage" && writes == 2 {
					if boundary == "after-stage" {
						if err := writeAuthorization(directory, name, data); err != nil {
							return err
						}
					}
					contents = readTestFile(t, filename)
					if err := os.Rename(filename, filepath.Join(t.TempDir(), "old")); err != nil {
						return err
					}
					if err := os.WriteFile(filename, []byte(contents), 0o600); err != nil {
						return err
					}
					var err error
					concurrent, err = os.Stat(filename)
					if err != nil {
						return err
					}
				}
				if boundary == "after-stage" {
					return nil
				}
				return writeAuthorization(directory, name, data)
			})
			if err == nil || !strings.Contains(err.Error(), "concurrently") {
				t.Fatalf("overwrote concurrent record: %v", err)
			}
			if (boundary == "stage" || boundary == "after-stage") && operationRan {
				t.Fatal("project operation ran after staging conflict")
			}
			if boundary == "activate" && !strings.Contains(err.Error(), "project operation completed") {
				t.Fatalf("lost partial-state report: %v", err)
			}
			if boundary == "rollback" && !errors.Is(err, injected) {
				t.Fatalf("lost project failure: %v", err)
			}
			info, statErr := os.Stat(filename)
			if statErr != nil || !os.SameFile(concurrent, info) || readTestFile(t, filename) != contents {
				t.Fatalf("changed concurrent record: %v", statErr)
			}
		})
	}
}

func TestLocalAuthorizationRecordPermissionsIgnoreUmask(t *testing.T) {
	project, source, service, _ := localFixture(t)
	result, err := service.InstallLocal(context.Background(), project, source, false)
	if err != nil {
		t.Fatal(err)
	}
	identity := result.Dependencies[0].Source
	err = changeLocalAuthorizationWith(project, identity, localAuthorizationFixture(t, project, source), func() error { return nil }, func(directory *localAuthorizationDirectory, name string, data []byte) error {
		prior := syscall.Umask(0o777)
		defer syscall.Umask(prior)
		return writeAuthorization(directory, name, data)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authorizeLocal(project, Declaration{Source: identity, Requested: RequestedLocal, Path: source}); err != nil {
		t.Fatalf("restrictive umask left unusable record: %v", err)
	}
}

// Exercise the same preservation assertions even when the host temp directory
// has no aliases (for example, Linux CI's /tmp).
func TestLocalAuthorizationTempRootAliases(t *testing.T) {
	for _, spelling := range []string{"canonical", "alias"} {
		t.Run(spelling, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, "target")
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
			temp := target
			if spelling == "alias" {
				temp = filepath.Join(root, "alias")
				if err := os.Symlink(target, temp); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("TMPDIR", temp)
			t.Setenv("GOTMPDIR", temp)
			t.Run("directories", TestLocalAuthorizationRetainsDirectoryAcrossBoundaries)
			t.Run("umask", TestLocalAuthorizationRecordPermissionsIgnoreUmask)
			t.Run("staging", TestLocalAuthorizationChecksStagingIdentityBeforePromotion)
			t.Run("early-refusal", TestLocalAuthorizationPreservesStagingOnEarlyRefusal)
		})
	}
}

// Match the install caller: preserve the declared path, bind the canonical root.
func localAuthorizationFixture(t *testing.T, project, source string) *localAuthorization {
	t.Helper()
	root, err := localRoot(project, source)
	if err != nil {
		t.Fatal(err)
	}
	return &localAuthorization{SchemaVersion: 1, Path: source, SourceRoot: root}
}
