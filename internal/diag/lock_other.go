//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package diag

import (
	"fmt"
	"os"
	"path/filepath"
)

func acquireWorkerLock(dir string) (func(), error) {
	lock := filepath.Join(dir, ".lock")
	if err := os.Mkdir(lock, 0700); err != nil {
		return nil, fmt.Errorf("cannot acquire diagnostic worker lock: %w (verify the previous worker has stopped before removing a stale .lock)", err)
	}
	return func() { _ = os.Remove(lock) }, nil
}
