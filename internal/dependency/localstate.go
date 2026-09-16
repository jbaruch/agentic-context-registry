package dependency

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

func checkExpectedState(root string, expected State) error {
	live, err := LoadState(root)
	if err != nil {
		return err
	}
	liveProject, liveLock, err := MarshalState(live)
	if err != nil {
		return err
	}
	expectedProject, expectedLock, err := MarshalState(expected)
	if err != nil {
		return err
	}
	if !bytes.Equal(liveProject, expectedProject) || !bytes.Equal(liveLock, expectedLock) {
		return errors.New("project dependency state changed concurrently; retry against current state")
	}
	return nil
}

// writeExpectedState captures byte before-images then verifies the caller's
// state against them. The shared journal checks these images under its claim
// and rolls back only its own writes, preserving concurrent project edits.
func writeExpectedState(root string, expected, desired State) (err error) {
	projectData, lockData, err := MarshalState(desired)
	if err != nil {
		return err
	}
	opened, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, opened.Close()) }()
	edits := make([]realize.FileTransactionEdit, 0, 2)
	for index, filename := range []string{ProjectFilename, LockFilename} {
		before, err := snapshotFile(opened, filename)
		if err != nil {
			return err
		}
		after := projectData
		if index == 1 {
			after = lockData
		}
		edits = append(edits, realize.FileTransactionEdit{Path: filename, Operation: "splice", Before: before.contents, BeforeMode: uint32(before.mode), BeforeAbsent: !before.exists, After: after, AfterMode: 0o644})
	}
	if err := checkExpectedState(root, expected); err != nil {
		return err
	}
	if err := realize.ApplyFileTransaction(root, edits); err != nil {
		return fmt.Errorf("write local dependency state: %w", err)
	}
	return nil
}
