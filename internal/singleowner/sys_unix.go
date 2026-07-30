//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package singleowner

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// tryLockFile takes a non-blocking exclusive flock on f. It reports
// whether the lock was acquired; a lock held by another open file
// description is (false, nil), not an error.
//
// flock locks are attached to the open file description and released by
// the kernel when the last descriptor referring to it closes — including
// when the process is killed — which is what makes a free lock proof that
// the previous owner is gone.
func tryLockFile(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.EWOULDBLOCK):
		return false, nil
	default:
		return false, fmt.Errorf("flock: %w", err)
	}
}

func unlockFile(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		return fmt.Errorf("flock unlock: %w", err)
	}
	return nil
}

func processLiveness(pid int) (Liveness, error) {
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil:
		return LivenessLive, nil
	case errors.Is(err, syscall.ESRCH):
		return LivenessDead, nil
	case errors.Is(err, syscall.EPERM):
		// The process exists; this one is not allowed to signal it.
		return LivenessLive, nil
	default:
		return LivenessUnknown, fmt.Errorf("kill(%d, 0): %w", pid, err)
	}
}
