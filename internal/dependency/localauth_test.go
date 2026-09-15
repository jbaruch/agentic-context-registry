package dependency

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbaruch/agentic-context-registry/internal/cli"
)

func TestLocalAuthorizationFailureRollsBackOnlyItsOwnRecord(t *testing.T) {
	project, source, service, _ := localFixture(t)
	result, err := service.InstallLocal(context.Background(), project, source, false)
	if err != nil {
		t.Fatal(err)
	}
	declaration := Declaration{Source: result.Dependencies[0].Source, Requested: RequestedLocal, Path: source}
	filename, _, err := localAuthorizationPath(project, declaration.Source)
	if err != nil {
		t.Fatal(err)
	}
	original := readTestFile(t, filename)
	injected := errors.New("injected project write failure")
	err = changeLocalAuthorization(project, declaration.Source, &localAuthorization{SchemaVersion: 1, Path: source, SourceRoot: source}, func() error {
		_, err := authorizeLocal(project, declaration)
		requireLocalCode(t, err, cli.CodeLocalSourceUnauthorized)
		return injected
	})
	if !errors.Is(err, injected) || readTestFile(t, filename) != original {
		t.Fatalf("failed operation did not restore prior auth: %v", err)
	}
	// An unexpected external writer must never be overwritten by rollback.
	err = changeLocalAuthorization(project, declaration.Source, nil, func() error {
		if err := os.WriteFile(filename, []byte("concurrent record"), 0o600); err != nil {
			t.Fatal(err)
		}
		return injected
	})
	if !errors.Is(err, injected) || !strings.Contains(err.Error(), "concurrently") || readTestFile(t, filename) != "concurrent record" {
		t.Fatalf("concurrent record overwritten: %v", err)
	}
}

func TestLocalNewAuthorizationNeverGrantsOnFailure(t *testing.T) {
	project, source, _, _ := localFixture(t)
	declaration := Declaration{Source: "github:owner/plugin", Requested: RequestedLocal, Path: source}
	filename, _, err := localAuthorizationPath(project, declaration.Source)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("project failed")
	err = changeLocalAuthorization(project, declaration.Source, &localAuthorization{SchemaVersion: 1, Path: source, SourceRoot: source}, func() error { return injected })
	if !errors.Is(err, injected) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filename); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new grant survived failed install: %v", err)
	}
	_, err = authorizeLocal(project, declaration)
	requireLocalCode(t, err, cli.CodeLocalSourceUnauthorized)
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(filename), ".authorization-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("staging leaked: %v %v", matches, err)
	}
}

func TestLocalAuthorizationRejectsProjectOrSourceStoreAndSymlink(t *testing.T) {
	project, source, service, _ := localFixture(t)
	for _, base := range []string{filepath.Join(project, "state"), filepath.Join(source, "state")} {
		t.Setenv("ACR_STATE_HOME", base)
		_, err := service.InstallLocal(context.Background(), project, source, false)
		requireLocalCode(t, err, cli.CodeLocalAuthorizationUnwritable)
		if _, err := os.Stat(base); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("created unsafe store: %v", err)
		}
	}
	t.Setenv("ACR_STATE_HOME", t.TempDir())
	result, err := service.InstallLocal(context.Background(), project, source, false)
	if err != nil {
		t.Fatal(err)
	}
	filename, _, err := localAuthorizationPath(project, result.Dependencies[0].Source)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "record")
	writeLocalTestFile(t, filepath.Dir(outside), "record", "untouched", 0o600)
	if err := os.Remove(filename); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filename); err != nil {
		t.Fatal(err)
	}
	_, err = service.InstallLocal(context.Background(), project, source, false)
	requireLocalCode(t, err, cli.CodeLocalAuthorizationUnwritable)
	if readTestFile(t, outside) != "untouched" {
		t.Fatal("wrote through authorization symlink")
	}
}

func TestLocalAuthorizationPermissions(t *testing.T) {
	project, source, service, _ := localFixture(t)
	base := filepath.Join(t.TempDir(), "new-state")
	t.Setenv("ACR_STATE_HOME", base)
	result, err := service.InstallLocal(context.Background(), project, source, false)
	if err != nil {
		t.Fatal(err)
	}
	filename, _, err := localAuthorizationPath(project, result.Dependencies[0].Source)
	if err != nil {
		t.Fatal(err)
	}
	for directory := filepath.Dir(filename); withinDirectory(base, directory); directory = filepath.Dir(directory) {
		info, err := os.Stat(directory)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("directory permissions: %v %v", info, err)
		}
	}
	info, err := os.Stat(filename)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("record permissions: %v %v", info, err)
	}
}

func TestLocalAuthorizationFinalizationFailureReportsCompletedState(t *testing.T) {
	project, source, _, _ := localFixture(t)
	declaration := Declaration{Source: "github:owner/plugin", Requested: RequestedLocal, Path: source}
	injected := errors.New("injected authorization activation failure")
	writes := 0
	err := changeLocalAuthorizationWith(project, declaration.Source, &localAuthorization{SchemaVersion: 1, Path: source, SourceRoot: source}, func() error {
		return os.WriteFile(filepath.Join(project, "owner-change"), []byte("completed operation\n"), 0o644)
	}, func(filename string, data []byte) error {
		writes++
		if writes == 2 {
			return injected
		}
		return writeAuthorization(filename, data)
	})
	if !errors.Is(err, injected) || !strings.Contains(err.Error(), "project operation completed") || !strings.Contains(err.Error(), "state may have changed") {
		t.Fatalf("misleading partial failure: %v", err)
	}
	if readTestFile(t, filepath.Join(project, "owner-change")) != "completed operation\n" {
		t.Fatal("overwrote completed project operation")
	}
	_, err = authorizeLocal(project, declaration)
	requireLocalCode(t, err, cli.CodeLocalSourceUnauthorized)
}

func TestLocalAuthorizationRejectsSymlinkedStoreSubdirectory(t *testing.T) {
	project, source, service, _ := localFixture(t)
	base := os.Getenv("ACR_STATE_HOME")
	if err := os.Symlink(project, filepath.Join(base, "local")); err != nil {
		t.Fatal(err)
	}
	_, err := service.InstallLocal(context.Background(), project, source, false)
	requireLocalCode(t, err, cli.CodeLocalAuthorizationUnwritable)
	entries, err := os.ReadDir(project)
	if err != nil || len(entries) != 0 {
		t.Fatalf("wrote through store alias: %v %v", entries, err)
	}
}

func TestLocalAuthorizationLockNeverCreatesThroughSymlink(t *testing.T) {
	project, source, service, _ := localFixture(t)
	filename, _, err := localAuthorizationPath(project, "github:owner/plugin")
	if err != nil {
		t.Fatal(err)
	}
	if err := makePrivateDirectory(filepath.Dir(filename)); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(source, "unexpected-auth-lock")
	if err := os.Symlink(target, filename+".lock"); err != nil {
		t.Fatal(err)
	}
	_, err = service.InstallLocal(context.Background(), project, source, false)
	requireLocalCode(t, err, cli.CodeLocalAuthorizationUnwritable)
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created source file through lock symlink: %v", err)
	}
}
