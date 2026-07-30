package singleowner

import "fmt"

// Liveness is what a pid check could establish about a process.
type Liveness int

const (
	// LivenessUnknown means the check could not reach a verdict. Callers
	// must treat it as "may be alive" and fail closed.
	LivenessUnknown Liveness = iota

	// LivenessLive means a process with that pid exists.
	LivenessLive

	// LivenessDead means no process with that pid exists.
	LivenessDead
)

func (l Liveness) String() string {
	switch l {
	case LivenessLive:
		return "live"
	case LivenessDead:
		return "dead"
	case LivenessUnknown:
		return "unknown"
	default:
		return fmt.Sprintf("Liveness(%d)", int(l))
	}
}

// ProcessLiveness reports whether a process with the given pid exists.
//
// # What it establishes
//
// LivenessDead is trustworthy: the operating system has no process with
// that pid, so the process that recorded it is certainly gone.
//
// LivenessLive is weaker than it looks. It means "some process holds this
// pid", not "the process that wrote this pid is still running". Pids are
// recycled — quickly on Windows, where they come from a reusable table,
// and eventually on Unix, where the counter wraps at pid_max. memdolt has
// no portable way to read another process's start time or image path, so
// it cannot rule recycling out.
//
// LivenessUnknown means the check itself could not complete. On Windows
// that includes a process owned by another user that this one may not
// query even for limited information.
//
// This is why staleness is decided by the advisory lock rather than by
// this function: the kernel releases the lock when the owner exits, which
// no pid can be recycled into. See the package documentation.
//
// # Per-platform mechanism
//
//   - Unix: kill(pid, 0). ESRCH is dead; success is live; EPERM is live
//     (the process exists, this one just may not signal it).
//   - Windows: OpenProcess with PROCESS_QUERY_LIMITED_INFORMATION, then
//     GetExitCodeProcess. ERROR_INVALID_PARAMETER is dead;
//     STILL_ACTIVE is live; any other exit code is dead (a handle held
//     elsewhere can keep an exited process's pid resolvable);
//     ERROR_ACCESS_DENIED is unknown.
//   - Anywhere else: unknown, with ErrUnsupportedPlatform.
//
// A non-nil error always accompanies LivenessUnknown.
func ProcessLiveness(pid int) (Liveness, error) {
	if pid <= 0 {
		return LivenessUnknown, fmt.Errorf("liveness check for invalid pid %d", pid)
	}
	return processLiveness(pid)
}
