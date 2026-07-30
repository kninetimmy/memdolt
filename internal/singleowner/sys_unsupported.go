//go:build !windows && !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package singleowner

import "os"

// This build has no advisory file locking implementation. memdolt fails
// closed rather than running the single-owner rule (PRD §5.2) on an
// unenforced lock.

func tryLockFile(_ *os.File) (bool, error) { return false, ErrUnsupportedPlatform }

func unlockFile(_ *os.File) error { return ErrUnsupportedPlatform }

func processLiveness(_ int) (Liveness, error) { return LivenessUnknown, ErrUnsupportedPlatform }
