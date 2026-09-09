package producerconvert

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
)

// transactionHooks are deterministic fault/race boundaries, never CLI options.
type transactionHooks struct{ Before func(string, string) error }

func (h transactionHooks) call(phase, name string) error {
	if h.Before != nil {
		return h.Before(phase, name)
	}
	return nil
}

// Apply commits exactly the prepared delta after rechecking all source bytes
// and modes. The private transaction directory is an exclusive claim and holds
// before-images until commit. Recovery faults retain it and name the paths.
func (p Plan) Apply() (Report, error) { return p.apply(transactionHooks{}) }

func (p Plan) apply(hooks transactionHooks) (report Report, err error) {
	report = p.Report
	report.DryRun = false
	if p.Report.Current {
		return report, nil
	}
	if p.root == "" || p.receipt == nil || len(p.Report.Blockers) != 0 {
		return report, refuse("invalid_plan", "plan", "prepare a successful complete conversion before applying")
	}
	root, err := os.OpenRoot(p.root)
	if err != nil {
		return report, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	if err = hooks.call("validate", ""); err != nil {
		return report, err
	}
	if err = verifyBefore(root, p.options.PackageRoot, p.before); err != nil {
		return report, err
	}
	if _, err := root.Lstat(ReceiptPath); err == nil {
		return report, refuse("receipt_conflict", ReceiptPath, "receipt appeared after planning; rerun the command")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return report, err
	}
	if err = root.Mkdir(transactionPath, 0o700); err != nil {
		return report, fmt.Errorf("claim exclusive %s: %w", transactionPath, err)
	}
	retain := false
	defer func() {
		if !retain {
			if e := root.RemoveAll(transactionPath); e != nil {
				err = errors.Join(err, fmt.Errorf("remove transaction staging %s: %w", transactionPath, e))
			}
		}
	}()
	changes := append([]Change(nil), p.changes...)
	changes = append(changes, makeChange(ReceiptPath, fileState{}, p.receipt, 0o600, false))
	// A readable journal plus synced before-images survive an interruption. An
	// interrupted claim refuses on the next invocation; it is never overwritten.
	for i, c := range changes {
		if c.Operation != "create" {
			if err = writeExclusive(root, stageName("before", i), []byte(c.Before), fs.FileMode(c.BeforeMode)); err != nil {
				return report, err
			}
		}
		if c.Operation != "remove" {
			if err = writeExclusive(root, stageName("after", i), []byte(c.After), fs.FileMode(c.AfterMode)); err != nil {
				return report, err
			}
		}
	}
	if err = writeExclusive(root, path.Join(transactionPath, "receipt.json"), p.receipt, 0o600); err != nil {
		return report, err
	}
	if err = verifyBefore(root, p.options.PackageRoot, p.before); err != nil {
		return report, err
	}
	type progress struct {
		index          int
		moved, created bool
	}
	applied := []progress{}
	rollback := func(cause error) error {
		var failures []error
		for i := len(applied) - 1; i >= 0; i-- {
			done := applied[i]
			if !done.created && !done.moved {
				continue
			}
			c := changes[done.index]
			if e := hooks.call("rollback", c.Path); e != nil {
				failures = append(failures, fmt.Errorf("rollback %s: %w", c.Path, e))
				continue
			}
			if e := checkParents(root, c.Path); e != nil {
				failures = append(failures, e)
				continue
			}
			if done.created {
				current, e := readState(root, c.Path)
				if e != nil || current.Digest != digest([]byte(c.After)) || current.Mode != c.AfterMode {
					failures = append(failures, fmt.Errorf("rollback %s: output changed; preserve it and restore its before-image manually (read error: %v)", c.Path, e))
					continue
				}
				if e := root.Remove(c.Path); e != nil {
					failures = append(failures, fmt.Errorf("rollback remove %s: %w", c.Path, e))
					continue
				}
			}
			if done.moved {
				// Link is an exclusive restore: a racing/user-created destination is never
				// overwritten. The moved original retains its exact executable mode.
				if e := root.Link(stageName("original", done.index), c.Path); e != nil {
					failures = append(failures, fmt.Errorf("rollback restore %s: %w", c.Path, e))
				}
			}
		}
		if len(failures) > 0 {
			retain = true
			failures = append([]error{cause, fmt.Errorf("rollback incomplete; inspect retained backups in %s", path.Join(p.root, transactionPath))}, failures...)
			return errors.Join(failures...)
		}
		return cause
	}
	for i, c := range changes {
		if err = hooks.call("edit", c.Path); err != nil {
			return report, rollback(err)
		}
		if err = checkParents(root, c.Path); err != nil {
			return report, rollback(err)
		}
		current, readErr := readState(root, c.Path)
		if c.Operation == "create" {
			if !errors.Is(readErr, fs.ErrNotExist) {
				return report, rollback(refuse("source_changed", c.Path, "planned new path is occupied; rerun the dry-run"))
			}
		} else if readErr != nil || current.Digest != digest([]byte(c.Before)) || current.Mode != c.BeforeMode {
			return report, rollback(refuse("source_changed", c.Path, "source bytes or mode changed after planning; rerun the dry-run"))
		}
		done := progress{index: i}
		if c.Operation != "create" {
			if err = root.Rename(c.Path, stageName("original", i)); err != nil {
				return report, rollback(err)
			}
			done.moved = true
		}
		applied = append(applied, done)
		if done.moved {
			saved, e := readState(root, stageName("original", i))
			if e != nil || saved.Digest != digest([]byte(c.Before)) || saved.Mode != c.BeforeMode {
				return report, rollback(refuse("source_changed", c.Path, "source changed at the write boundary; rerun the dry-run"))
			}
		}
		if c.Operation != "remove" {
			if err = hooks.call("write", c.Path); err != nil {
				return report, rollback(err)
			}
			if err = checkParents(root, c.Path); err != nil {
				return report, rollback(err)
			}
			if err = root.Link(stageName("after", i), c.Path); err != nil {
				return report, rollback(fmt.Errorf("exclusive create %s: %w", c.Path, err))
			}
			applied[len(applied)-1].created = true
		}
	}
	if err = hooks.call("commit", ""); err != nil {
		return report, rollback(err)
	}
	current, e := snapshot(root, p.options.PackageRoot)
	if e != nil {
		return report, rollback(e)
	}
	if !matches(p.after, current) {
		return report, rollback(refuse("source_changed", p.root, "source changed during application; retry from a stable checkout"))
	}
	report.Wrote = true
	return report, nil
}

func verifyBefore(root *os.Root, selected string, want tree) error {
	current, err := snapshot(root, selected)
	if err != nil {
		return err
	}
	if !matches(want, current) {
		return refuse("source_changed", root.Name(), "source bytes, modes or paths changed since planning; rerun the dry-run")
	}
	return nil
}
func stageName(kind string, index int) string {
	return path.Join(transactionPath, fmt.Sprintf("%s-%06d", kind, index))
}
func writeExclusive(root *os.Root, name string, data []byte, mode fs.FileMode) (err error) {
	if err = checkParents(root, name); err != nil {
		return err
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	if err = file.Chmod(mode); err != nil {
		return err
	}
	if _, err = file.Write(data); err != nil {
		return err
	}
	return file.Sync()
}
