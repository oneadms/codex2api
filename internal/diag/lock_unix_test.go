//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package diag

import (
	"os"
	"os/exec"
	"testing"
)

func TestWorkerLockSurvivesUncleanExit(t *testing.T) {
	if dir := os.Getenv("DIAG_TEST_LOCK_DIRECTORY"); dir != "" {
		if _, err := acquireWorkerLock(dir); err != nil {
			os.Exit(2)
		}
		// Exit without running the release function, as during a container kill.
		os.Exit(0)
	}
	dir := t.TempDir()
	unlock, err := acquireWorkerLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if release, err := acquireWorkerLock(dir); err == nil {
		release()
		t.Fatal("concurrent worker acquired lock")
	}
	unlock()
	child := exec.Command(os.Args[0], "-test.run=^TestWorkerLockSurvivesUncleanExit$")
	child.Env = append(os.Environ(), "DIAG_TEST_LOCK_DIRECTORY="+dir)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("child: %s %v", output, err)
	}
	unlock, err = acquireWorkerLock(dir)
	if err != nil {
		t.Fatalf("unclean exit left a stale lock: %v", err)
	}
	unlock()
}
