package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/mbvlabs/vaire/internal/config"
)

// moveSwapReservation moves an exact reservation without replacing an
// existing destination. prepare must durably record a new identity before
// returning; on retry expected is that recorded identity, never a new guess.
// The caller must independently hold the managed cutover locks.
//
//nolint:unparam // aside is the caller's choice of reservation name; the only callers today are tests that use one name.
func moveSwapReservation(ctx context.Context, dir string, uid uint32, from, aside string, port int, expected ReservationIdentity, prepare func(ReservationIdentity) error) (ReservationIdentity, error) {
	if !validForkReservationName(from) || !validForkReservationName(aside) || from == aside ||
		port < config.TemporaryPortMin || port > config.TemporaryPortMax {
		return ReservationIdentity{}, errors.New("invalid swap reservation")
	}

	a, err := openForkAllocator(ctx, dir, uid)
	if err != nil {
		return ReservationIdentity{}, err
	}
	defer a.close()

	src, dst, err := swapReservationSides(a, from, aside)
	if err != nil {
		return ReservationIdentity{}, err
	}

	if err := swapReservationEvidence(src, dst, expected, prepare); err != nil {
		return ReservationIdentity{}, err
	}

	name, links := swapReservationSource(from, aside, src, dst)

	id, info, err := prepareSwapReservation(a, name, uid, port, links, expected, prepare)
	if err != nil {
		return ReservationIdentity{}, err
	}

	if err := verifySwapReservationDestination(a, aside, uid, port, info, src, dst); err != nil {
		return ReservationIdentity{}, err
	}

	if err := moveSwapReservationFiles(a, from, aside, uid, port, src, dst); err != nil {
		return ReservationIdentity{}, err
	}

	if err := verifyMovedSwapReservation(a, aside, uid, port, info, id.Digest); err != nil {
		return ReservationIdentity{}, err
	}

	if err := a.unique(aside, port, uid); err != nil {
		return ReservationIdentity{}, err
	}

	return id, nil
}

// swapReservationEvidence requires a first attempt to have somewhere to
// prepare and a retry to name the identity preparation recorded.
func swapReservationEvidence(src, dst bool, expected ReservationIdentity, prepare func(ReservationIdentity) error) error {
	if !src && !dst {
		return errors.New("swap reservation missing")
	}

	if expected.Inode == 0 && (!src || dst || prepare == nil) {
		return errors.New("swap reservation has no prepare evidence")
	}

	return nil
}

// swapReservationSource names the entry to read and its expected link count: a
// partially moved reservation has both links.
func swapReservationSource(from, aside string, src, dst bool) (string, uint64) {
	if !src {
		return aside, 1
	}

	if dst {
		return from, 2
	}

	return from, 1
}

// swapReservationSides reports whether the reservation names from, aside or
// both. It fails closed on an entry it cannot inspect.
func swapReservationSides(a *forkAllocator, from, aside string) (bool, bool, error) {
	_, srcErr := a.root.Lstat(from)

	_, dstErr := a.root.Lstat(aside)
	if srcErr != nil && !errors.Is(srcErr, os.ErrNotExist) || dstErr != nil && !errors.Is(dstErr, os.ErrNotExist) {
		return false, false, errors.Join(srcErr, dstErr)
	}

	return srcErr == nil, dstErr == nil, nil
}

// verifySwapReservationDestination requires an existing destination to be the
// same inode and port, so the move never treats a foreign reservation as its
// own.
func verifySwapReservationDestination(a *forkAllocator, aside string, uid uint32, port int, info os.FileInfo, src, dst bool) error {
	if !src || !dst {
		return nil
	}

	otherPort, other, err := a.readLinks(aside, uid, 2)
	if err != nil || otherPort != port || !os.SameFile(info, other) {
		return errors.New("swap reservation destination collision")
	}

	return nil
}

// moveSwapReservationFiles links the reservation under aside when it is still
// only under from, then drops from once aside names the same inode.
func moveSwapReservationFiles(a *forkAllocator, from, aside string, uid uint32, port int, src, dst bool) error {
	if src && !dst {
		if err := linkSwapReservation(a, from, aside, uid, port); err != nil {
			return err
		}
	}

	if !src {
		return nil
	}

	return removeSwapReservation(a, from)
}

// prepareSwapReservation verifies the exact live reservation and, on the first
// attempt, records its identity through prepare.
func prepareSwapReservation(a *forkAllocator, name string, uid uint32, port int, links uint64, expected ReservationIdentity, prepare func(ReservationIdentity) error) (ReservationIdentity, os.FileInfo, error) {
	got, info, err := a.readLinks(name, uid, links)
	if err != nil || got != port {
		return ReservationIdentity{}, nil, errors.Join(err, errors.New("swap reservation port or identity mismatch"))
	}

	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ReservationIdentity{}, nil, errors.New("unverifiable swap reservation")
	}

	digest, err := a.digest(name, info)
	if err != nil {
		return ReservationIdentity{}, nil, err
	}

	id := ReservationIdentity{Device: statDevice(st), Inode: st.Ino, UID: st.Uid, Digest: digest}
	if expected.Inode != 0 {
		if expected != id {
			return ReservationIdentity{}, nil, errors.New("swap reservation was replaced")
		}

		return id, info, nil
	}

	if err := a.unique(name, port, uid); err != nil {
		return ReservationIdentity{}, nil, err
	}

	if err := prepare(id); err != nil {
		return ReservationIdentity{}, nil, err
	}

	if err := verifyPreparedSwapReservation(a, name, uid, port, info, digest); err != nil {
		return ReservationIdentity{}, nil, err
	}

	return id, info, nil
}

// verifyPreparedSwapReservation re-reads the reservation after preparation and
// refuses a replacement or a content change that appeared while it ran, even
// while managed allocators hold the flock.
func verifyPreparedSwapReservation(a *forkAllocator, name string, uid uint32, port int, info os.FileInfo, digest string) error {
	got, current, err := a.read(name, uid)
	if err != nil || got != port || !os.SameFile(info, current) {
		return errors.New("swap reservation changed during preparation")
	}

	currentDigest, err := a.digest(name, current)
	if err != nil || currentDigest != digest {
		return errors.New("swap reservation content changed during preparation")
	}

	return nil
}

// linkSwapReservation leaves the reservation under both names. Link is
// exclusive, unlike rename. A crash after Link is resolved only by the
// persisted identity and the same two linked inodes.
func linkSwapReservation(a *forkAllocator, from, aside string, uid uint32, port int) error {
	if err := a.unique(from, port, uid); err != nil {
		return err
	}

	if err := a.root.Link(from, aside); err != nil {
		return fmt.Errorf("link swap reservation: %w", err)
	}

	return a.dir.Sync()
}

// removeSwapReservation drops the original name once aside names the same
// inode.
func removeSwapReservation(a *forkAllocator, from string) error {
	if err := a.root.Remove(from); err != nil {
		return err
	}

	return a.dir.Sync()
}

// verifyMovedSwapReservation requires the moved reservation to be the same
// inode with unchanged content.
func verifyMovedSwapReservation(a *forkAllocator, aside string, uid uint32, port int, info os.FileInfo, digest string) error {
	got, after, err := a.read(aside, uid)
	if err != nil || got != port || !os.SameFile(info, after) {
		return errors.New("moved swap reservation changed")
	}

	afterDigest, err := a.digest(aside, after)
	if err != nil || afterDigest != digest {
		return errors.New("moved swap reservation content changed")
	}

	return nil
}
