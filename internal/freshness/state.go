package freshness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	// StateSchemaVersion is the freshness throttle artifact version.
	StateSchemaVersion = 1
	// Window limits remote freshness checks to one attempt per project and policy.
	Window = 24 * time.Hour
)

// Clock supplies the current instant to freshness execution.
type Clock func() time.Time

// Outcome records the last attempted freshness result.
type Outcome string

const (
	OutcomeOK       Outcome = "ok"
	OutcomeOffline  Outcome = "offline"
	OutcomeAuth     Outcome = "auth"
	OutcomeFailed   Outcome = "failed"
	OutcomeConflict Outcome = "conflict"
)

// State is the schema-versioned, per-machine throttle record.
type State struct {
	SchemaVersion int       `json:"schemaVersion"`
	Project       string    `json:"project"`
	LastCheckedAt time.Time `json:"lastCheckedAt"`
	LastPolicy    Policy    `json:"lastPolicy"`
	LastOutcome   Outcome   `json:"lastOutcome"`
}

// Store owns freshness state beneath BaseDirectory. The directory is outside
// the project and can be injected in tests.
type Store struct {
	BaseDirectory string
}

// DefaultStore resolves ACR_STATE_HOME or the user's cache directory.
func DefaultStore() (Store, error) {
	if configured := os.Getenv("ACR_STATE_HOME"); configured != "" {
		return Store{BaseDirectory: configured}, nil
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return Store{}, fmt.Errorf("resolve user cache directory: %w; set ACR_STATE_HOME to a writable directory", err)
	}
	return Store{BaseDirectory: filepath.Join(cache, "acr")}, nil
}

// ProjectIdentity canonicalizes root and returns its lower-hex key plus the
// schema-qualified identity stored in the record.
func ProjectIdentity(root string) (key, identity string, err error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", "", fmt.Errorf("resolve absolute project path %q: %w", root, err)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", "", fmt.Errorf("canonicalize project path %q: %w", root, err)
	}
	digest := sha256.Sum256([]byte("acr-project-v1\x00" + canonical))
	key = hex.EncodeToString(digest[:])
	return key, "sha256:" + key, nil
}

// Paths returns the state and advisory-lock paths for one canonical project.
func (store Store) Paths(root string) (statePath, lockPath string, err error) {
	key, _, err := ProjectIdentity(root)
	if err != nil {
		return "", "", err
	}
	directory := filepath.Join(store.BaseDirectory, "freshness")
	return filepath.Join(directory, key+".json"), filepath.Join(directory, key+".lock"), nil
}

// ReadState reads the default store without migrating or rewriting state.
func ReadState(root string) (State, bool, error) {
	store, err := DefaultStore()
	if err != nil {
		return State{}, false, err
	}
	return store.Read(root)
}

// Read returns a usable matching-version record. Missing, corrupt, invalid,
// and unsupported-version records are all treated as no usable prior state.
func (store Store) Read(root string) (State, bool, error) {
	statePath, _, err := store.Paths(root)
	if err != nil {
		return State{}, false, err
	}
	content, err := os.ReadFile(statePath)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, fmt.Errorf("read freshness state %q: %w", statePath, err)
	}
	var state State
	if err := json.Unmarshal(content, &state); err != nil {
		return State{}, false, nil
	}
	_, identity, err := ProjectIdentity(root)
	if err != nil {
		return State{}, false, err
	}
	if state.SchemaVersion != StateSchemaVersion || state.Project != identity || state.LastCheckedAt.IsZero() || !validPolicy(state.LastPolicy) || !validOutcome(state.LastOutcome) {
		return State{}, false, nil
	}
	state.LastCheckedAt = state.LastCheckedAt.UTC()
	return state, true, nil
}

// Write replaces any previous record with a current schema record.
func (store Store) Write(root string, checkedAt time.Time, policy Policy, outcome Outcome) error {
	statePath, _, err := store.Paths(root)
	if err != nil {
		return err
	}
	if !validPolicy(policy) {
		return fmt.Errorf("write freshness state: unsupported policy %q", policy)
	}
	if !validOutcome(outcome) {
		return fmt.Errorf("write freshness state: unsupported outcome %q", outcome)
	}
	_, identity, err := ProjectIdentity(root)
	if err != nil {
		return err
	}
	state := State{
		SchemaVersion: StateSchemaVersion,
		Project:       identity,
		LastCheckedAt: checkedAt.UTC(),
		LastPolicy:    policy,
		LastOutcome:   outcome,
	}
	content, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode freshness state: %w", err)
	}
	content = append(content, '\n')
	directory := filepath.Dir(statePath)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create freshness state directory %q: %w", directory, err)
	}
	temporary, err := os.CreateTemp(directory, ".state-*")
	if err != nil {
		return fmt.Errorf("create temporary freshness state: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("set freshness state permissions: %w", err)
	}
	written, err := temporary.Write(content)
	if err != nil {
		temporary.Close()
		return fmt.Errorf("write temporary freshness state: %w", err)
	}
	if written != len(content) {
		temporary.Close()
		return fmt.Errorf("write temporary freshness state: wrote %d of %d bytes", written, len(content))
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync temporary freshness state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary freshness state: %w", err)
	}
	if err := os.Rename(temporaryPath, statePath); err != nil {
		return fmt.Errorf("replace freshness state %q: %w", statePath, err)
	}
	return nil
}

// Throttled reports whether a matching-policy attempt remains inside Window.
func Throttled(state State, policy Policy, now time.Time) bool {
	return state.LastPolicy == policy && !now.Before(state.LastCheckedAt) && now.Sub(state.LastCheckedAt) < Window
}

func validPolicy(policy Policy) bool {
	return policy == PolicyOutdated || policy == PolicyInstall
}

func validOutcome(outcome Outcome) bool {
	switch outcome {
	case OutcomeOK, OutcomeOffline, OutcomeAuth, OutcomeFailed, OutcomeConflict:
		return true
	default:
		return false
	}
}
