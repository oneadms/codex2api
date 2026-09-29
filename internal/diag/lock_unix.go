//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package diag

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// The kernel releases this lock on process exit, including a killed container.
// Never unlink the file: waiters must continue to lock the same inode.
func acquireWorkerLock(dir string) (func(), error) {
	if _, err := os.Stat(filepath.Join(dir, ".lock")); !os.IsNotExist(err) {
		return nil, fmt.Errorf("legacy diagnostic worker lock present; stop the old worker before removing .lock")
	}
	file, err := os.OpenFile(filepath.Join(dir, ".worker.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("cannot acquire diagnostic worker lock: %w", err)
	}
	return func() { _ = file.Close() }, nil
}
