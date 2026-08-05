package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kninetimmy/memdolt/internal/layout"
	"github.com/kninetimmy/memdolt/internal/singleowner"
)

// probeTimeout bounds the health request Probe makes. The owner is on
// loopback, so a slow answer means something is wrong, not far away.
const probeTimeout = 3 * time.Second

// ErrOwnerUnverified reports that Probe found a pidfile whose owner
// process is alive but could not be confirmed to be the memdolt endpoint
// it claims to be. Probe fails closed rather than guessing: a caller that
// assumed "no owner" here could open the store behind a live owner's back.
var ErrOwnerUnverified = errors.New("pidfile owner is running but its ipc endpoint did not answer")

// Status is one of the three outcomes of a liveness probe.
type Status int

const (
	// StatusNoOwner means no process owns this repository's store: there
	// is no pidfile, or the last owner shut down cleanly. A CLI may open
	// the store directly.
	StatusNoOwner Status = iota

	// StatusOwnerDead means a pidfile survives an owner that died. The
	// record is orphaned; the store may be opened directly once it is
	// cleaned up, which happens on the next Listen.
	StatusOwnerDead

	// StatusOwnerLive means an owner is running and answered on its
	// loopback endpoint. A CLI routes through it (PRD §5.2.1).
	StatusOwnerLive
)

func (s Status) String() string {
	switch s {
	case StatusNoOwner:
		return "no-owner"
	case StatusOwnerDead:
		return "owner-dead"
	case StatusOwnerLive:
		return "owner-live"
	default:
		return fmt.Sprintf("Status(%d)", int(s))
	}
}

// OwnerInfo describes the process named in a pidfile. It never carries the
// token.
type OwnerInfo struct {
	PID        int
	Port       int
	Host       string
	OwnerID    string
	Executable string
	StartedAt  time.Time
}

// Probe reports whether a live owner holds this repository's store.
//
// It answers from two independent signals. The pidfile doubles as the
// owner's advisory lock, so a free lock proves the writing process is gone
// no matter what its recorded pid now refers to; that alone separates "no
// owner" and "dead owner" from "held". A held lock is then confirmed by
// asking the endpoint itself, which proves the holder really is the
// memdolt owner named in the file rather than an unrelated process.
//
// Probe fails closed. If the pidfile cannot be read, if its record is
// unusable, or if a live holder's endpoint does not answer with a matching
// identity, Probe returns an error rather than a verdict.
func Probe(ctx context.Context, baseDir string) (Status, OwnerInfo, error) {
	status, info, _, err := probeOwner(ctx, baseDir)
	return status, info, err
}

// probeOwner is Probe, plus the endpoint detail it had to read anyway. That
// detail carries the token, which is why it goes no further than this
// package: OwnerInfo never carries one. A client reconnecting to a
// replacement owner needs the new port and the new token together with the
// verdict that the owner is live, and reading the pidfile a second time to
// get them would leave a window in which the two disagree.
func probeOwner(ctx context.Context, baseDir string) (Status, OwnerInfo, ownerDetail, error) {
	paths, err := layout.New(baseDir)
	if err != nil {
		return StatusNoOwner, OwnerInfo{}, ownerDetail{}, err
	}

	owner, state, err := singleowner.Inspect(paths.PidFile())
	if err != nil {
		return StatusNoOwner, OwnerInfo{}, ownerDetail{}, fmt.Errorf("ipc: inspect pidfile: %w", err)
	}

	switch state {
	case singleowner.StateNone:
		return StatusNoOwner, OwnerInfo{}, ownerDetail{}, nil

	case singleowner.StateStale:
		info, _ := ownerInfo(owner) // a dead owner's detail is diagnostic only
		return StatusOwnerDead, info, ownerDetail{}, nil

	case singleowner.StateHeld:
		info, detail, err := ownerInfoWithDetail(owner)
		if err != nil {
			return StatusNoOwner, OwnerInfo{}, ownerDetail{}, fmt.Errorf("ipc: %w", err)
		}
		health, err := newClient(baseDir, detail).Health(ctx)
		if err != nil {
			return StatusNoOwner, info, ownerDetail{}, fmt.Errorf("ipc: %w: pid %d, %w", ErrOwnerUnverified, info.PID, err)
		}
		if health.OwnerID != owner.ID {
			return StatusNoOwner, info, ownerDetail{}, fmt.Errorf(
				"ipc: %w: pidfile names owner %s but the endpoint on port %d reports %s",
				ErrOwnerUnverified, owner.ID, info.Port, health.OwnerID)
		}
		return StatusOwnerLive, info, detail, nil

	default:
		return StatusNoOwner, OwnerInfo{}, ownerDetail{}, fmt.Errorf("ipc: unexpected pidfile state %v", state)
	}
}

func ownerInfo(owner singleowner.Owner) (OwnerInfo, ownerDetail) {
	info, detail, _ := ownerInfoWithDetail(owner)
	return info, detail
}

func ownerInfoWithDetail(owner singleowner.Owner) (OwnerInfo, ownerDetail, error) {
	info := OwnerInfo{
		PID:        owner.PID,
		Host:       owner.Host,
		OwnerID:    owner.ID,
		Executable: owner.Executable,
		StartedAt:  owner.AcquiredAt,
	}
	if len(owner.Detail) == 0 {
		return info, ownerDetail{}, errors.New("pidfile record carries no endpoint detail")
	}
	var detail ownerDetail
	if err := json.Unmarshal(owner.Detail, &detail); err != nil {
		// The error is not wrapped: a decoder error can quote the input,
		// and the input contains the token.
		return info, ownerDetail{}, errors.New("pidfile record's endpoint detail is not readable")
	}
	if detail.Port <= 0 {
		return info, ownerDetail{}, fmt.Errorf("pidfile record has an invalid port %d", detail.Port)
	}
	info.Port = detail.Port
	return info, detail, nil
}
