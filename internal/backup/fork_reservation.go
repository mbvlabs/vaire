package backup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	xunix "golang.org/x/sys/unix"
)

// ownedReservation is an in-process birth capability, not recoverable from
// persisted metadata. Holding the file open prevents inode reuse.
type ownedReservation struct {
	mu     sync.Mutex
	owner  *Materialised
	dir    string
	uid    uint32
	name   string
	port   int
	file   *os.File
	info   os.FileInfo
	digest string
	done   bool
}

// ReservationIdentity identifies the file exclusively created by this
// invocation. It is evidence only when persisted under the allocator lock.
type ReservationIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	UID    uint32 `json:"uid"`
	Digest string `json:"digest,omitempty"`
}

func (m *Materialised) ForkReservationIdentity() (ReservationIdentity, error) {
	if m == nil || m.reservation == nil || m.reservation.owner != m {
		return ReservationIdentity{}, errors.New("fork reservation capability required")
	}
	st, ok := m.reservation.info.Sys().(*syscall.Stat_t)
	if !ok || st.Ino == 0 {
		return ReservationIdentity{}, errors.New("unverifiable reservation identity")
	}
	return ReservationIdentity{Device: uint64(st.Dev), Inode: st.Ino, UID: st.Uid, Digest: m.reservation.digest}, nil
}

var forkReservationStore = func() (string, uint32, error) {
	parent, err := os.Lstat(filepath.Dir(TemporaryPortDir))
	if err != nil {
		return "", 0, err
	}
	st, ok := parent.Sys().(*syscall.Stat_t)
	if !ok || !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 || st.Uid != 0 || parent.Mode().Perm()&0o022 != 0 {
		return "", 0, errors.New("unsafe reservation parent")
	}
	if err := os.Mkdir(TemporaryPortDir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", 0, err
	}
	return TemporaryPortDir, 0, nil
}

var forkClusterPorts = clusterPorts
var forkSocketTaken = func(port int) bool {
	_, err := os.Lstat(fmt.Sprintf("%s/.s.PGSQL.%d.lock", pgSocketDir, port))
	return !errors.Is(err, os.ErrNotExist)
}

// LockForkReservation checks the exact owner/port mapping (or its absence)
// under the allocator lock. The caller must hold independent live job and
// artifact authority; a matching mapping is not birth evidence.
func LockForkReservation(ctx context.Context, name string, port int, present bool) (func(), error) {
	dir, uid, err := forkReservationStore()
	if err != nil {
		return nil, err
	}
	return LockForkReservationWithHost(ctx, dir, uid, name, port, present)
}

// LockForkReservationWithHost is for trusted host providers, not RPC input.
func LockForkReservationWithHost(ctx context.Context, dir string, uid uint32, name string, port int, present bool) (func(), error) {
	if !validForkReservationName(name) || port < config.TemporaryPortMin || port > config.TemporaryPortMax {
		return nil, errors.New("invalid fork reservation")
	}
	a, err := openForkAllocator(ctx, dir, uid)
	if err != nil {
		return nil, err
	}
	got, _, err := a.read(name, uid)
	if present {
		if err == nil && got == port {
			err = a.unique(name, port, uid)
		} else {
			err = errors.Join(err, errors.New("fork reservation mismatch"))
		}
	} else if errors.Is(err, os.ErrNotExist) {
		err = nil
	} else {
		err = errors.Join(err, errors.New("fork reservation is not absent"))
	}
	if err != nil {
		a.close()
		return nil, err
	}
	return a.close, nil
}

// LockTemporaryReservationAbsence holds the allocator lock while proving that
// an exact owner has no reservation. Absence alone grants no artifact rights.
func LockTemporaryReservationAbsence(ctx context.Context, name string) (func(), error) {
	dir, uid, err := forkReservationStore()
	if err != nil {
		return nil, err
	}
	return LockTemporaryReservationAbsenceWithHost(ctx, dir, uid, name)
}

// LockTemporaryReservationAbsenceWithHost is for trusted internal test hosts.
func LockTemporaryReservationAbsenceWithHost(ctx context.Context, dir string, uid uint32, name string) (func(), error) {
	if !validForkReservationName(name) {
		return nil, errors.New("invalid reservation owner")
	}
	a, err := openForkAllocator(ctx, dir, uid)
	if err != nil {
		return nil, err
	}
	if _, err := a.root.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		a.close()
		return nil, errors.Join(err, errors.New("reservation exists or is unverifiable"))
	}
	return a.close, nil
}

// CleanupFollowerReservation runs a trusted node cleanup under the allocator
// lock and removes only the captured reservation. The caller must establish
// the active destructive lock, exact target and artifact birth evidence. The
// callback must not call another allocator operation.
func CleanupFollowerReservation(ctx context.Context, name string, port int, id ReservationIdentity, effect func() error) error {
	dir, uid, err := forkReservationStore()
	if err != nil {
		return err
	}
	return CleanupFollowerReservationWithHost(ctx, dir, uid, name, port, id, effect)
}

// CleanupFollowerReservationWithHost is for trusted internal host providers.
func CleanupFollowerReservationWithHost(ctx context.Context, dir string, uid uint32, name string, port int, id ReservationIdentity, effect func() error) error {
	return cleanupBoundReservation(ctx, dir, uid, name, port, id, false, effect)
}

// CleanupRestoreReservation requires the original reservation to remain
// present throughout cleanup. Its callback must not acquire the allocator.
func CleanupRestoreReservation(ctx context.Context, name string, port int, id ReservationIdentity, effect func() error) error {
	dir, uid, err := forkReservationStore()
	if err != nil {
		return err
	}
	return CleanupRestoreReservationWithHost(ctx, dir, uid, name, port, id, effect)
}

// CleanupRestoreReservationWithHost is for trusted internal host providers.
func CleanupRestoreReservationWithHost(ctx context.Context, dir string, uid uint32, name string, port int, id ReservationIdentity, effect func() error) error {
	return cleanupBoundReservation(ctx, dir, uid, name, port, id, true, effect)
}

// CleanupCurrentLegacyReservation holds the allocator lock while a separately
// authorized legacy registration is removed. The snapshot identifies only
// the current mapping, not its historical creator. Absence after reboot is
// valid; a present mapping is removed only after the effect succeeds.
func CleanupCurrentLegacyReservation(ctx context.Context, name string, port int, effect func() error) error {
	dir, uid, err := forkReservationStore()
	if err != nil {
		return err
	}
	return CleanupCurrentLegacyReservationWithHost(ctx, dir, uid, name, port, effect)
}

// CleanupCurrentLegacyReservationWithHost accepts a trusted internal host.
func CleanupCurrentLegacyReservationWithHost(ctx context.Context, dir string, uid uint32, name string, port int, effect func() error) error {
	if !validForkReservationName(name) || port < config.TemporaryPortMin || port > config.TemporaryPortMax || effect == nil {
		return errors.New("invalid legacy reservation cleanup")
	}
	a, err := openForkAllocator(ctx, dir, uid)
	if err != nil {
		return err
	}
	defer a.close()
	got, info, err := a.read(name, uid)
	present := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	digest := ""
	if present {
		if got != port {
			return errors.New("legacy reservation port mismatch")
		}
		fd, err := xunix.Openat(int(a.dir.Fd()), name, xunix.O_RDONLY|xunix.O_NOFOLLOW|xunix.O_NONBLOCK|xunix.O_CLOEXEC, 0) //nolint:gosec // trusted allocator directory
		if err != nil {
			return err
		}
		pinned := os.NewFile(uintptr(fd), name)
		defer func() { _ = pinned.Close() }()
		opened, err := pinned.Stat()
		if err != nil || !os.SameFile(info, opened) {
			return errors.New("legacy reservation replaced while pinning")
		}
		digest, err = a.digest(name, info)
		if err != nil {
			return err
		}
	}
	if err := a.unique(name, port, uid); err != nil {
		return err
	}
	if err := effect(); err != nil {
		return err
	}
	got, after, err := a.read(name, uid)
	if !present {
		if !errors.Is(err, os.ErrNotExist) {
			return errors.New("legacy reservation appeared during cleanup")
		}
		return a.unique(name, port, uid)
	}
	if err != nil || got != port || !os.SameFile(info, after) {
		return errors.New("legacy reservation changed during cleanup")
	}
	afterDigest, err := a.digest(name, after)
	if err != nil || digest != afterDigest {
		return errors.New("legacy reservation content changed during cleanup")
	}
	if err := a.unique(name, port, uid); err != nil {
		return err
	}
	if err := a.root.Remove(name); err != nil {
		return err
	}
	return a.dir.Sync()
}

func cleanupBoundReservation(ctx context.Context, dir string, uid uint32, name string, port int, id ReservationIdentity, requirePresent bool, effect func() error) error {
	if !validForkReservationName(name) || port < config.TemporaryPortMin || port > config.TemporaryPortMax ||
		id.Inode == 0 || id.UID != uid || len(id.Digest) != 64 || effect == nil {
		return errors.New("missing follower reservation birth evidence")
	}
	if _, err := hex.DecodeString(id.Digest); err != nil {
		return errors.New("invalid follower reservation birth evidence")
	}
	a, err := openForkAllocator(ctx, dir, uid)
	if err != nil {
		return err
	}
	defer a.close()
	check := func() (bool, error) {
		got, info, err := a.read(name, uid)
		if errors.Is(err, os.ErrNotExist) {
			return false, a.unique("", port, uid)
		}
		if err != nil || got != port {
			return false, errors.Join(err, errors.New("follower reservation mismatch"))
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || uint64(st.Dev) != id.Device || st.Ino != id.Inode || st.Uid != id.UID {
			return false, errors.New("follower reservation replaced")
		}
		digest, err := a.digest(name, info)
		if err != nil || digest != id.Digest {
			return false, errors.New("follower reservation content replaced")
		}
		return true, a.unique(name, port, uid)
	}
	present, err := check()
	if err != nil {
		return err
	}
	if requirePresent && !present {
		return errors.New("restore reservation is absent")
	}
	if err := effect(); err != nil {
		return err
	}
	after, err := check()
	if err != nil || after != present {
		return errors.Join(err, errors.New("follower reservation changed during cleanup"))
	}
	if present {
		if err := a.root.Remove(name); err != nil {
			return err
		}
		return a.dir.Sync()
	}
	return nil
}

// NewForkMaterialised allocates a reservation with an in-memory birth
// capability. The caller must still supply an active, exact node ownership
// guard before any fork lifecycle action; this capability is not a job lease.
func NewForkMaterialised(dataRoot, name, source string, sourcePort int, sh engine.Shell, stepID string, report engine.Reporter) (*Materialised, error) {
	dir, uid, err := forkReservationStore()
	if err != nil {
		return nil, err
	}
	return NewForkMaterialisedWithHost(dataRoot, name, source, sourcePort, sh, stepID, report, ForkMaterialisationHost{
		ReservationDir: dir, UID: uid, ClusterPorts: forkClusterPorts, SocketTaken: forkSocketTaken,
	})
}

// ForkMaterialisationHost is a trusted internal host provider, never RPC
// input. Its alternative paths permit isolated filesystem testing.
type ForkMaterialisationHost struct {
	ReservationDir string
	ConfigRoot     string
	UID            uint32
	ClusterPorts   func() map[int]bool
	SocketTaken    func(int) bool
	Running        func(context.Context, string) bool
	WriteFile      func(string, []byte, os.FileMode, string, string) error
}

func NewForkMaterialisedWithHost(dataRoot, name, source string, sourcePort int, sh engine.Shell, stepID string, report engine.Reporter, host ForkMaterialisationHost) (*Materialised, error) {
	if host.ClusterPorts == nil || host.SocketTaken == nil {
		return nil, errors.New("fork host probes required")
	}
	m := &Materialised{Name: name, Source: source, SourcePort: sourcePort, Data: filepath.Join(dataRoot, name), Shell: sh, StepID: stepID, Report: report}
	m.forkConfigRoot = host.ConfigRoot
	m.forkRunning = host.Running
	m.forkWriteFile = host.WriteFile
	for _, path := range []string{m.ConfDir(), m.Data} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return nil, errors.Join(err, errors.New("materialisation path exists or is unverifiable"))
		}
	}
	r, err := reserveForkAt(context.Background(), host.ReservationDir, host.UID, m, host.ClusterPorts, host.SocketTaken)
	if err != nil {
		return nil, err
	}
	m.Port, m.reservation = r.port, r
	return m, nil
}

func validForkReservationName(name string) bool {
	if name == "" || len(name) > 128 || name == "ports.lock" {
		return false
	}
	for _, c := range name {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

type forkAllocator struct {
	root      *os.Root
	dir, lock *os.File
}

func openForkAllocator(ctx context.Context, path string, uid uint32) (*forkAllocator, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, errors.New("invalid allocator context")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || info.Mode().Perm() != 0o700 || st.Uid != uid {
		return nil, errors.New("unsafe reservation directory")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	a := &forkAllocator{root: root}
	fail := func(err error) (*forkAllocator, error) { a.close(); return nil, err }
	a.dir, err = root.Open(".")
	if err != nil {
		return fail(err)
	}
	di, err := a.dir.Stat()
	if err != nil || !os.SameFile(info, di) {
		return fail(errors.New("reservation directory replaced"))
	}
	fd, err := xunix.Openat(int(a.dir.Fd()), "ports.lock", xunix.O_RDWR|xunix.O_CREAT|xunix.O_NOFOLLOW|xunix.O_CLOEXEC, 0o600) //nolint:gosec // trusted directory descriptor
	if err != nil {
		return fail(err)
	}
	a.lock = os.NewFile(uintptr(fd), "ports.lock")
	li, err := a.lock.Stat()
	if err != nil {
		return fail(err)
	}
	ls, ok := li.Sys().(*syscall.Stat_t)
	if !ok || !li.Mode().IsRegular() || li.Mode().Perm() != 0o600 || li.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || ls.Uid != uid || ls.Nlink != 1 {
		return fail(errors.New("unsafe allocator lock"))
	}
	for {
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return fail(err)
		}
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	named, err := os.Lstat(path)
	ln, le := root.Lstat("ports.lock")
	if err != nil || le != nil || !os.SameFile(info, named) || !os.SameFile(li, ln) {
		return fail(errors.New("allocator replaced"))
	}
	return a, nil
}

func (a *forkAllocator) close() {
	if a.lock != nil {
		_ = syscall.Flock(int(a.lock.Fd()), syscall.LOCK_UN) //nolint:gosec // descriptor
		_ = a.lock.Close()
	}
	if a.dir != nil {
		_ = a.dir.Close()
	}
	if a.root != nil {
		_ = a.root.Close()
	}
}

func (a *forkAllocator) read(name string, uid uint32) (int, os.FileInfo, error) {
	return a.readLinks(name, uid, 1)
}

// readLinks permits the temporary two-link state of a crash-interrupted
// no-replace reservation move. Ordinary allocator operations require one link.
func (a *forkAllocator) readLinks(name string, uid uint32, links uint64) (int, os.FileInfo, error) {
	if !validForkReservationName(name) {
		return 0, nil, errors.New("invalid reservation owner")
	}
	fd, err := xunix.Openat(int(a.dir.Fd()), name, xunix.O_RDONLY|xunix.O_NOFOLLOW|xunix.O_NONBLOCK|xunix.O_CLOEXEC, 0) //nolint:gosec // trusted directory descriptor
	if err != nil {
		return 0, nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return 0, nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || st.Uid != uid || uint64(st.Nlink) != links || info.Size() > 256 {
		return 0, nil, errors.New("unsafe reservation")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 257))
	named, ne := a.root.Lstat(name)
	if err != nil || ne != nil || !os.SameFile(info, named) {
		return 0, nil, errors.New("reservation replaced")
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || port < config.TemporaryPortMin || port > config.TemporaryPortMax {
		return 0, nil, errors.New("invalid reservation port")
	}
	return port, info, nil
}

func (a *forkAllocator) unique(name string, port int, uid uint32) error {
	d, err := a.root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	entries, err := d.ReadDir(257)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(entries) > 256 {
		return errors.New("too many reservations")
	}
	for _, entry := range entries {
		if entry.Name() == "ports.lock" || entry.Name() == name {
			continue
		}
		other, _, err := a.read(entry.Name(), uid)
		if err != nil || other == port {
			return errors.Join(err, errors.New("conflicting or unverifiable reservation"))
		}
	}
	return nil
}

func (a *forkAllocator) digest(name string, expected os.FileInfo) (string, error) {
	fd, err := xunix.Openat(int(a.dir.Fd()), name, xunix.O_RDONLY|xunix.O_NOFOLLOW|xunix.O_CLOEXEC|xunix.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	f := os.NewFile(uintptr(fd), name)
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !os.SameFile(expected, info) {
		return "", errors.New("reservation replaced")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 257))
	if err != nil || len(raw) > 256 {
		return "", errors.New("invalid reservation content")
	}
	named, err := a.root.Lstat(name)
	if err != nil || !os.SameFile(info, named) {
		return "", errors.New("reservation replaced")
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func reserveForkAt(ctx context.Context, dir string, uid uint32, m *Materialised, cluster func() map[int]bool, socket func(int) bool) (*ownedReservation, error) {
	if m == nil || !validForkReservationName(m.Name) {
		return nil, errors.New("invalid reservation owner")
	}
	a, err := openForkAllocator(ctx, dir, uid)
	if err != nil {
		return nil, err
	}
	defer a.close()
	if _, err := a.root.Lstat(m.Name); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.Join(err, errors.New("reservation owner exists or is unverifiable"))
	}
	taken := cluster()
	entries, err := a.dir.ReadDir(257)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > 256 {
		return nil, errors.New("too many reservations")
	}
	for _, entry := range entries {
		if entry.Name() == "ports.lock" {
			continue
		}
		port, _, err := a.read(entry.Name(), uid)
		if err != nil {
			return nil, err
		}
		taken[port] = true
	}
	for port := config.TemporaryPortMin; port <= config.TemporaryPortMax; port++ {
		if taken[port] || socket(port) {
			continue
		}
		fd, err := xunix.Openat(int(a.dir.Fd()), m.Name, xunix.O_RDWR|xunix.O_CREAT|xunix.O_EXCL|xunix.O_NOFOLLOW|xunix.O_CLOEXEC, 0o600) //nolint:gosec // trusted directory descriptor
		if err != nil {
			return nil, err
		}
		f := os.NewFile(uintptr(fd), m.Name)
		var nonce [16]byte
		if _, err = rand.Read(nonce[:]); err != nil {
			_ = f.Close()
			return nil, err
		}
		var whitespace strings.Builder
		for _, b := range nonce {
			for bit := 0; bit < 8; bit++ {
				if b&(1<<bit) != 0 {
					whitespace.WriteByte('\t')
				} else {
					whitespace.WriteByte(' ')
				}
			}
		}
		_, err = fmt.Fprintf(f, "%d%s\n", port, whitespace.String())
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		if err = f.Sync(); err != nil {
			_ = f.Close()
			return nil, err
		}
		if err = a.dir.Sync(); err != nil {
			_ = f.Close()
			return nil, err
		}
		info, err := f.Stat()
		got, named, readErr := a.read(m.Name, uid)
		if err != nil || readErr != nil || got != port || !os.SameFile(info, named) {
			_ = f.Close()
			return nil, errors.New("created reservation unverifiable")
		}
		digest, err := a.digest(m.Name, info)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		return &ownedReservation{owner: m, dir: dir, uid: uid, name: m.Name, port: port, file: f, info: info, digest: digest}, nil
	}
	return nil, errors.New("no temporary port available")
}

func (r *ownedReservation) matches(a *forkAllocator, m *Materialised) error {
	if r.done || r.owner != m || m.Name != r.name || m.Port != r.port || r.file == nil {
		return errors.New("stale or mismatched reservation capability")
	}
	port, info, err := a.read(r.name, r.uid)
	if err != nil || port != r.port || !os.SameFile(r.info, info) {
		return errors.New("reservation identity or port mismatch")
	}
	digest, err := a.digest(r.name, info)
	if err != nil || digest != r.digest {
		return errors.New("reservation content changed")
	}
	return a.unique(r.name, r.port, r.uid)
}

func (r *ownedReservation) validate(ctx context.Context, m *Materialised) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, err := openForkAllocator(ctx, r.dir, r.uid)
	if err != nil {
		return err
	}
	defer a.close()
	return r.matches(a, m)
}

// WithForkReservation holds the allocator lock while effect runs, after
// verifying this object's original reservation. It is not a node job lease.
// Effect must not call another allocator operation.
func (m *Materialised) WithForkReservation(ctx context.Context, effect func() error) error {
	r := m.reservation
	if r == nil || effect == nil {
		return errors.New("fork reservation capability required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	a, err := openForkAllocator(ctx, r.dir, r.uid)
	if err != nil {
		return err
	}
	defer a.close()
	if err := r.matches(a, m); err != nil {
		return err
	}
	err = effect()
	return errors.Join(err, r.matches(a, m))
}

// perform holds the allocator flock through both the effect and exact removal.
// Call it inside, never around, the node guard: that guard may check the same lock.
func (r *ownedReservation) perform(ctx context.Context, m *Materialised, effect func() error, complete func()) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if effect == nil {
		return errors.New("stale or mismatched reservation capability")
	}
	a, err := openForkAllocator(ctx, r.dir, r.uid)
	if err != nil {
		return err
	}
	defer a.close()
	check := func() error { return r.matches(a, m) }
	if err := check(); err != nil {
		return err
	}
	if err := effect(); err != nil {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	if err := a.root.Remove(r.name); err != nil {
		return err
	}
	if err := a.dir.Sync(); err != nil {
		return err
	}
	r.done = true
	_ = r.file.Close()
	r.file = nil
	if complete != nil {
		complete()
	}
	return nil
}

// DiscardOwnedReservation runs a node-authorized exact-artifact cleanup and
// retires this invocation's reservation. The callback must not acquire the
// allocator lock and must independently enforce the live job lease.
func (m *Materialised) DiscardOwnedReservation(ctx context.Context, effect func() error) error {
	if m == nil || m.Ownership == nil || m.reservation == nil || m.reservation.owner != m {
		return errors.New("owned reservation and materialisation guard required")
	}
	return m.reservation.perform(ctx, m, effect, nil)
}

// ReleaseForkReservationAuthority relinquishes the in-memory capability
// without deleting the reservation or the target. It cannot be reacquired
// from metadata; cleanup of a retained target requires separate authority.
func (m *Materialised) ReleaseForkReservationAuthority() {
	r := m.reservation
	if r == nil || r.owner != m {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.done = true
	if r.file != nil {
		_ = r.file.Close()
		r.file = nil
	}
}
