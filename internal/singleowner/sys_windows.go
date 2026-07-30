//go:build windows

package singleowner

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const (
	lockfileFailImmediately = 0x00000001
	lockfileExclusiveLock   = 0x00000002

	// Windows byte-range locks are mandatory: a process that does not hold
	// the lock cannot read the locked bytes. The lock is therefore placed
	// far past any plausible end of file (offset 1<<62) so that it never
	// covers the ownership record, and a second process can still read the
	// owner's pid out of a file it may not lock.
	lockRegionOffsetHigh = 0x40000000
	lockRegionLength     = 1

	// PROCESS_QUERY_LIMITED_INFORMATION. Unlike
	// PROCESS_QUERY_INFORMATION it is granted for processes running under
	// other accounts, which keeps a cross-user pid check out of the
	// "unknown" bucket more often.
	processQueryLimitedInformation = 0x1000

	stillActive = 259

	errorInvalidParameter syscall.Errno = 87
	errorLockViolation    syscall.Errno = 33
)

// The syscall package exposes OpenProcess and GetExitCodeProcess but not
// LockFileEx/UnlockFileEx, so those two are resolved from kernel32 here.
// golang.org/x/sys/windows has them, but memdolt is not taking a new
// dependency for two calls.
var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

func lockRegion() *syscall.Overlapped {
	return &syscall.Overlapped{OffsetHigh: lockRegionOffsetHigh}
}

// tryLockFile takes a non-blocking exclusive byte-range lock on f. It
// reports whether the lock was acquired; a lock held elsewhere is
// (false, nil), not an error.
//
// Windows releases every lock a process holds when the process terminates,
// however it terminates, which is what makes a free lock proof that the
// previous owner is gone.
func tryLockFile(f *os.File) (bool, error) {
	if err := procLockFileEx.Find(); err != nil {
		return false, fmt.Errorf("resolve kernel32!LockFileEx: %w", err)
	}
	overlapped := lockRegion()
	r1, _, errno := syscall.SyscallN(
		procLockFileEx.Addr(),
		uintptr(syscall.Handle(f.Fd())),
		uintptr(lockfileExclusiveLock|lockfileFailImmediately),
		0,
		uintptr(lockRegionLength),
		0,
		uintptr(unsafe.Pointer(overlapped)),
	)
	if r1 != 0 {
		return true, nil
	}
	err := errnoErr(errno)
	if errors.Is(err, errorLockViolation) || errors.Is(err, syscall.ERROR_IO_PENDING) {
		return false, nil
	}
	return false, fmt.Errorf("LockFileEx: %w", err)
}

func unlockFile(f *os.File) error {
	if err := procUnlockFileEx.Find(); err != nil {
		return fmt.Errorf("resolve kernel32!UnlockFileEx: %w", err)
	}
	overlapped := lockRegion()
	r1, _, errno := syscall.SyscallN(
		procUnlockFileEx.Addr(),
		uintptr(syscall.Handle(f.Fd())),
		0,
		uintptr(lockRegionLength),
		0,
		uintptr(unsafe.Pointer(overlapped)),
	)
	if r1 != 0 {
		return nil
	}
	return fmt.Errorf("UnlockFileEx: %w", errnoErr(errno))
}

// errnoErr turns the raw errno from a SyscallN into an error, standing in
// a generic failure when the call reported no error code at all.
func errnoErr(errno syscall.Errno) error {
	if errno == 0 {
		return errors.New("call failed without an error code")
	}
	return errno
}

func processLiveness(pid int) (Liveness, error) {
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		if errors.Is(err, errorInvalidParameter) {
			// Windows reports a pid that belongs to no process this way.
			return LivenessDead, nil
		}
		// ERROR_ACCESS_DENIED lands here too: the process may well exist,
		// so the honest answer is that the check did not conclude.
		return LivenessUnknown, fmt.Errorf("OpenProcess(%d): %w", pid, err)
	}
	// The handle is only needed for the exit-code query below.
	defer func() { _ = syscall.CloseHandle(handle) }()

	var code uint32
	if err := syscall.GetExitCodeProcess(handle, &code); err != nil {
		return LivenessUnknown, fmt.Errorf("GetExitCodeProcess(%d): %w", pid, err)
	}
	if code == stillActive {
		return LivenessLive, nil
	}
	// OpenProcess can still resolve a pid whose process has exited while
	// another handle to it remains open; the exit code is the truth.
	return LivenessDead, nil
}
