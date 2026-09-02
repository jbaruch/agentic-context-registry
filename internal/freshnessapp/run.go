package freshnessapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/jbaruch/agentic-context-registry/internal/dependency"
	"github.com/jbaruch/agentic-context-registry/internal/freshness"
	"github.com/jbaruch/agentic-context-registry/internal/preserve"
	"github.com/jbaruch/agentic-context-registry/internal/realize"
)

const (
	CodeOutdated        = "freshness_outdated"
	CodeOffline         = "freshness_offline"
	CodeAuth            = "freshness_auth"
	CodeBusy            = "freshness_busy"
	CodeUpdateFailed    = "freshness_update_failed"
	CodeConflict        = "freshness_conflict"
	CodeStateUnwritable = "freshness_state_unwritable"
)

// Notice is one session-start diagnostic. The CLI maps it onto its notice
// channel without writing a normal result message.
type Notice struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Result is one throttled freshness attempt.
type Result struct {
	Policy          freshness.Policy                `json:"policy"`
	Throttled       bool                            `json:"throttled,omitempty"`
	Outdated        []dependency.OutdatedDependency `json:"outdated"`
	RestartRequired bool                            `json:"restartRequired,omitempty"`
	Agents          []string                        `json:"agents,omitempty"`
	Notices         []Notice                        `json:"notices,omitempty"`
}

// RunError carries the stable notice code, process exit code, and state
// outcome for one failed attempt.
type RunError struct {
	Code     string
	ExitCode int
	Outcome  freshness.Outcome
	Err      error
}

func (err *RunError) Error() string { return err.Err.Error() }

func (err *RunError) Unwrap() error { return err.Err }

type lockHandle interface {
	io.Closer
}

type acquireLock func(freshness.Store, string) (lockHandle, error)
type readState func(freshness.Store, string) (freshness.State, bool, error)
type writeState func(freshness.Store, string, time.Time, freshness.Policy, freshness.Outcome) error

// Runner serializes, throttles, executes, and records freshness attempts.
type Runner struct {
	store    freshness.Store
	clock    freshness.Clock
	outdated outdatedExecutor
	install  *installExecutor
	acquire  acquireLock
	read     readState
	write    writeState
}

// WithInstall configures non-interactive reconciliation followed by the
// transactional realization service.
func (runner *Runner) WithInstall(reconciler dependencyReconciler, realizer realizationService) *Runner {
	runner.install = &installExecutor{reconciler: reconciler, realizer: realizer}
	return runner
}

// NewRunner constructs the read-only outdated runner. Install execution is
// added through the same runner in the next layer.
func NewRunner(store freshness.Store, clock freshness.Clock, checker outdatedChecker) *Runner {
	return &Runner{
		store: store, clock: clock, outdated: outdatedExecutor{checker: checker},
		acquire: func(store freshness.Store, root string) (lockHandle, error) { return store.TryLock(root) },
		read:    func(store freshness.Store, root string) (freshness.State, bool, error) { return store.Read(root) },
		write: func(store freshness.Store, root string, checkedAt time.Time, policy freshness.Policy, outcome freshness.Outcome) error {
			return store.Write(root, checkedAt, policy, outcome)
		},
	}
}

// Run executes one policy after taking the canonical project lock and
// evaluating its matching-policy throttle window.
func (runner *Runner) Run(ctx context.Context, root string, policy freshness.Policy) (result Result, runErr error) {
	result.Policy = policy
	result.Outdated = []dependency.OutdatedDependency{}
	if policy == freshness.PolicyNone {
		return result, nil
	}
	lock, err := runner.acquire(runner.store, root)
	if errors.Is(err, freshness.ErrLockBusy) {
		result.Notices = []Notice{{Code: CodeBusy, Message: fmt.Sprintf("Another ACR operation is active for this project; retry 'acr freshness run --project %s --policy %s'.", root, policy)}}
		return result, nil
	}
	if err != nil {
		return stateFailure(result, fmt.Errorf("acquire project lock: %w", err), root, policy)
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil && runErr == nil {
			result, runErr = stateFailure(result, fmt.Errorf("release project lock: %w", closeErr), root, policy)
		}
	}()

	prior, usable, err := runner.read(runner.store, root)
	if err != nil {
		return stateFailure(result, err, root, policy)
	}
	now := runner.clock().UTC()
	if usable && freshness.Throttled(prior, policy, now) {
		result.Throttled = true
		return result, nil
	}

	var execution Result
	switch policy {
	case freshness.PolicyOutdated:
		execution, err = runner.outdated.execute(ctx, root)
	case freshness.PolicyInstall:
		if runner.install == nil {
			err = errors.New("install freshness execution is not configured")
		} else {
			execution, err = runner.install.execute(ctx, root)
		}
	default:
		err = fmt.Errorf("unsupported freshness policy %q", policy)
	}
	execution.Policy = policy
	if execution.Outdated == nil {
		execution.Outdated = []dependency.OutdatedDependency{}
	}
	result = execution
	outcome := freshness.OutcomeOK
	var classified *RunError
	if err != nil {
		classified = classifyFailure(err)
		outcome = classified.Outcome
		result.Notices = append(result.Notices, failureNotice(classified.Code, root, policy))
	}
	if writeErr := runner.write(runner.store, root, now, policy, outcome); writeErr != nil {
		stateResult, stateErr := stateFailure(result, writeErr, root, policy)
		return stateResult, stateErr
	}
	if classified != nil {
		return result, classified
	}
	return result, nil
}

func classifyFailure(err error) *RunError {
	var engineConflict *realize.ConflictError
	var preserveConflict *preserve.ConflictError
	if errors.As(err, &engineConflict) || errors.As(err, &preserveConflict) {
		return &RunError{Code: CodeConflict, ExitCode: 4, Outcome: freshness.OutcomeConflict, Err: err}
	}
	var remote *dependency.RemoteError
	if errors.As(err, &remote) {
		switch remote.StatusCode {
		case 0:
			return &RunError{Code: CodeOffline, ExitCode: 1, Outcome: freshness.OutcomeOffline, Err: err}
		case http.StatusUnauthorized, http.StatusForbidden:
			return &RunError{Code: CodeAuth, ExitCode: 1, Outcome: freshness.OutcomeAuth, Err: err}
		}
	}
	var network net.Error
	if errors.As(err, &network) {
		return &RunError{Code: CodeOffline, ExitCode: 1, Outcome: freshness.OutcomeOffline, Err: err}
	}
	return &RunError{Code: CodeUpdateFailed, ExitCode: 1, Outcome: freshness.OutcomeFailed, Err: err}
}

func stateFailure(result Result, err error, root string, policy freshness.Policy) (Result, error) {
	result.Notices = append(result.Notices, failureNotice(CodeStateUnwritable, root, policy))
	return result, &RunError{Code: CodeStateUnwritable, ExitCode: 1, Outcome: freshness.OutcomeFailed, Err: err}
}

func failureNotice(code, root string, policy freshness.Policy) Notice {
	var message string
	switch code {
	case CodeOffline:
		message = fmt.Sprintf("Freshness could not reach GitHub; check network access and run 'acr freshness run --project %s --policy %s'.", root, policy)
	case CodeAuth:
		message = fmt.Sprintf("Freshness could not authenticate to GitHub; run 'gh auth login', then 'acr freshness run --project %s --policy %s'.", root, policy)
	case CodeConflict:
		message = fmt.Sprintf("Freshness found a realization conflict; run 'acr realize --project %s' and resolve the reported ownership conflict.", root)
	case CodeStateUnwritable:
		message = fmt.Sprintf("Freshness state is not writable; fix ACR_STATE_HOME permissions, then run 'acr freshness run --project %s --policy %s'.", root, policy)
	default:
		message = fmt.Sprintf("Freshness could not apply updates; run 'acr install --project %s' to diagnose the failure.", root)
	}
	return Notice{Code: code, Message: message}
}
