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

	_, srcErr := a.root.Lstat(from)
	_, dstErr := a.root.Lstat(aside)
	if srcErr != nil && !errors.Is(srcErr, os.ErrNotExist) || dstErr != nil && !errors.Is(dstErr, os.ErrNotExist) {
		return ReservationIdentity{}, errors.Join(srcErr, dstErr)
	}
	src, dst := srcErr == nil, dstErr == nil
	if !src && !dst {
		return ReservationIdentity{}, errors.New("swap reservation missing")
	}
	if expected.Inode == 0 && (!src || dst || prepare == nil) {
		return ReservationIdentity{}, errors.New("swap reservation has no prepare evidence")
	}
	name, links := from, uint64(1)
	if !src {
		name = aside
	} else if dst {
		links = 2
	}
	got, info, err := a.readLinks(name, uid, links)
	if err != nil || got != port {
		return ReservationIdentity{}, errors.Join(err, errors.New("swap reservation port or identity mismatch"))
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ReservationIdentity{}, errors.New("unverifiable swap reservation")
	}
	digest, err := a.digest(name, info)
	if err != nil {
		return ReservationIdentity{}, err
	}
	id := ReservationIdentity{Device: uint64(st.Dev), Inode: st.Ino, UID: st.Uid, Digest: digest}
	if expected.Inode == 0 {
		if err := a.unique(from, port, uid); err != nil {
			return ReservationIdentity{}, err
		}
		if err := prepare(id); err != nil {
			return ReservationIdentity{}, err
		}
		// Persistence is a fallible callback: never move a replacement that
		// appeared while it ran, even while managed allocators hold the flock.
		got, current, err := a.read(from, uid)
		if err != nil || got != port || !os.SameFile(info, current) {
			return ReservationIdentity{}, errors.New("swap reservation changed during preparation")
		}
		currentDigest, err := a.digest(from, current)
		if err != nil || currentDigest != id.Digest {
			return ReservationIdentity{}, errors.New("swap reservation content changed during preparation")
		}
	} else if expected != id {
		return ReservationIdentity{}, errors.New("swap reservation was replaced")
	}
	if src && dst {
		otherPort, other, err := a.readLinks(aside, uid, 2)
		if err != nil || otherPort != port || !os.SameFile(info, other) {
			return ReservationIdentity{}, errors.New("swap reservation destination collision")
		}
	}
	if src && !dst {
		if err := a.unique(from, port, uid); err != nil {
			return ReservationIdentity{}, err
		}
		// Link is exclusive, unlike rename. A crash after Link is resolved
		// only by the persisted identity and the same two linked inodes.
		if err := a.root.Link(from, aside); err != nil {
			return ReservationIdentity{}, fmt.Errorf("link swap reservation: %w", err)
		}
		if err := a.dir.Sync(); err != nil {
			return ReservationIdentity{}, err
		}
	}
	if src {
		if err := a.root.Remove(from); err != nil {
			return ReservationIdentity{}, err
		}
		if err := a.dir.Sync(); err != nil {
			return ReservationIdentity{}, err
		}
	}
	got, after, err := a.read(aside, uid)
	if err != nil || got != port || !os.SameFile(info, after) {
		return ReservationIdentity{}, errors.New("moved swap reservation changed")
	}
	digest, err = a.digest(aside, after)
	if err != nil || digest != id.Digest {
		return ReservationIdentity{}, errors.New("moved swap reservation content changed")
	}
	if err := a.unique(aside, port, uid); err != nil {
		return ReservationIdentity{}, err
	}
	return id, nil
}
