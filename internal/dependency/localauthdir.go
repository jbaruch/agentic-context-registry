package dependency

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Keep every ancestor open, not only the leaf: an ancestor can be replaced
// while the leaf and its advisory lock are still reachable through a handle.
// All mutations are relative to the verified leaf, never an absolute pathname.
type localAuthorizationDirectory struct {
	chain  []authorizationAncestor
	record *authorizationRecord
}

// Mutations compare both bytes and file identity. A replacement carrying the
// same pending JSON belongs to the concurrent writer, not this transaction.
type authorizationRecord struct {
	info os.FileInfo
	data []byte
}

func (directory *localAuthorizationDirectory) checkRecord(name string) error {
	if err := directory.verify(); err != nil {
		return err
	}
	info, err := directory.root().Lstat(name)
	if directory.record == nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		return errors.New("authorization record appeared concurrently; retained current record")
	}
	if err != nil {
		return fmt.Errorf("authorization record changed concurrently: %w", err)
	}
	data, err := directory.read(name)
	if err != nil {
		return err
	}
	after, err := directory.root().Lstat(name)
	if err != nil {
		return err
	}
	if !os.SameFile(info, after) || !os.SameFile(info, directory.record.info) || !bytes.Equal(data, directory.record.data) {
		return errors.New("authorization record changed concurrently; retained current record")
	}
	return nil
}

type authorizationAncestor struct {
	root    *os.Root
	name    string
	info    os.FileInfo
	private bool
}

func openAuthorizationDirectory(filename string, create bool) (directory *localAuthorizationDirectory, err error) {
	directory = &localAuthorizationDirectory{}
	defer func() {
		if err != nil {
			err = errors.Join(err, directory.Close())
		}
	}()
	root, err := os.OpenRoot(string(filepath.Separator))
	if err != nil {
		return directory, err
	}
	directory.chain = append(directory.chain, authorizationAncestor{root: root})
	parts := strings.Split(strings.TrimPrefix(filepath.Dir(filename), string(filepath.Separator)), string(filepath.Separator))
	for index, name := range parts {
		if err := directory.verify(); err != nil {
			return directory, err
		}
		parent := directory.root()
		info, inspectErr := parent.Lstat(name)
		created := false
		if errors.Is(inspectErr, os.ErrNotExist) && create {
			if err := parent.Mkdir(name, 0o700); err != nil {
				return directory, err
			}
			created = true
			info, inspectErr = parent.Lstat(name)
		}
		if inspectErr != nil {
			return directory, inspectErr
		}
		private := created || index >= len(parts)-2
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || private && info.Mode().Perm() != 0o700 {
			requirement := ""
			if private {
				requirement = " with permissions 0700"
			}
			return directory, fmt.Errorf("authorization directory %s must be a real directory%s", name, requirement)
		}
		child, err := parent.OpenRoot(name)
		if err != nil {
			return directory, err
		}
		directory.chain = append(directory.chain, authorizationAncestor{root: child, name: name, info: info, private: private})
		opened, err := child.Stat(".")
		if err != nil {
			return directory, err
		}
		if !os.SameFile(info, opened) {
			return directory, errors.New("authorization directory changed concurrently while opening")
		}
	}
	return directory, directory.verify()
}

func (directory *localAuthorizationDirectory) root() *os.Root {
	return directory.chain[len(directory.chain)-1].root
}

func (directory *localAuthorizationDirectory) verify() error {
	for index := 1; index < len(directory.chain); index++ {
		child := directory.chain[index]
		current, err := directory.chain[index-1].root.Lstat(child.name)
		if err != nil {
			return fmt.Errorf("authorization directory changed concurrently: %w", err)
		}
		if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(child.info, current) || child.private && current.Mode().Perm() != 0o700 {
			return errors.New("authorization directory changed concurrently; retained current record; repair ACR_STATE_HOME")
		}
	}
	return nil
}

func (directory *localAuthorizationDirectory) Close() (err error) {
	for index := len(directory.chain) - 1; index >= 0; index-- {
		err = errors.Join(err, directory.chain[index].root.Close())
	}
	return err
}

func (directory *localAuthorizationDirectory) read(name string) ([]byte, error) {
	if err := directory.verify(); err != nil {
		return nil, err
	}
	data, mode, err := readRegularFile(directory.root(), name)
	if err != nil {
		return nil, err
	}
	if mode != 0o600 {
		return nil, errors.New("local authorization must have permissions 0600")
	}
	if err := directory.verify(); err != nil {
		return nil, err
	}
	return data, nil
}

func (directory *localAuthorizationDirectory) remove(name string) error {
	if err := directory.checkRecord(name); err != nil {
		return err
	}
	return directory.root().Remove(name)
}

var errAuthorizationStageChanged = errors.New("authorization staging file changed concurrently; retained current file")

func writeAuthorization(directory *localAuthorizationDirectory, name string, data []byte) error {
	return writeAuthorizationWithStageHook(directory, name, data, nil)
}

// The optional hook models a concurrent replacement after close, immediately
// before the production promotion checks. Ordinary writes have no hook.
func writeAuthorizationWithStageHook(directory *localAuthorizationDirectory, name string, data []byte, afterClose func(string) error) (err error) {
	if err := directory.checkRecord(name); err != nil {
		return err
	}
	temporary, err := temporaryStateName(".")
	if err != nil {
		return err
	}
	file, err := directory.root().OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	stagedInfo, statErr := file.Stat()
	if statErr != nil {
		return errors.Join(statErr, file.Close())
	}
	// Cleanup uses the original handle even when an ancestor was replaced. It
	// removes only this operation's exclusive staging file, never a redirected one.
	defer func() {
		current, inspectErr := directory.root().Lstat(temporary)
		if errors.Is(inspectErr, os.ErrNotExist) {
			return
		}
		if inspectErr != nil {
			err = errors.Join(err, inspectErr)
			return
		}
		if !os.SameFile(stagedInfo, current) {
			err = errors.Join(err, errAuthorizationStageChanged)
			return
		}
		err = errors.Join(err, directory.root().Remove(temporary))
	}()
	if err = file.Chmod(0o600); err != nil {
		return errors.Join(err, file.Close())
	}
	if _, err = file.Write(data); err != nil {
		return errors.Join(err, file.Close())
	}
	if err = file.Sync(); err != nil {
		return errors.Join(err, file.Close())
	}
	if err = file.Close(); err != nil {
		return err
	}
	if afterClose != nil {
		if err := afterClose(temporary); err != nil {
			return err
		}
	}
	if err := directory.checkRecord(name); err != nil {
		return err
	}
	current, err := directory.root().Lstat(temporary)
	if err != nil {
		return fmt.Errorf("inspect authorization staging file before promotion: %w", err)
	}
	if !current.Mode().IsRegular() || !os.SameFile(stagedInfo, current) {
		return errAuthorizationStageChanged
	}
	if err := directory.root().Rename(temporary, name); err != nil {
		return err
	}
	directory.record = &authorizationRecord{info: stagedInfo, data: bytes.Clone(data)}
	return nil
}
