package backup

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	xunix "golang.org/x/sys/unix"
)

// Evidence is supplied by trusted node code, not RPC input.
type FollowerResumeEvidence struct {
	TemporaryPort       int
	ReservationIdentity ReservationIdentity
}
type FollowerResumeResult struct{ ReservationRetained bool }
type FollowerResumeGuard interface {
	GuardFollowerResume(context.Context, MaterialisationPaths, int, func(FollowerResumeEvidence) error) error
}

// FollowerResumeHost is a trusted internal host provider, not RPC input.
type FollowerResumeHost interface {
	RegistrationDirectory(string) string
	ReservationStore() (string, uint32, error)
	Stop(context.Context) error
	AdoptRegistration(int) error
}

// ResumeMaterialisedWithHost is for trusted internal host integration.
func ResumeMaterialisedWithHost(dataRoot, name, source string, sourcePort int, sh engine.Shell, stepID string, report engine.Reporter, configRoot string, host FollowerResumeHost) *Materialised {
	m := ResumeMaterialised(dataRoot, name, source, sourcePort, sh, stepID, report)
	m.forkConfigRoot = configRoot
	m.FollowerResumeHost = host

	return m
}

func (m *Materialised) AdoptResumedFollower(ctx context.Context, port int, guard FollowerResumeGuard) (FollowerResumeResult, error) {
	var result FollowerResumeResult
	if m == nil || ctx == nil || guard == nil || m.Ownership != nil || m.reservation != nil || m.Port != 0 ||
		port < 1 || port > 65535 || config.CheckDatabaseName(m.Name) != nil ||
		config.CheckDatabaseName(m.Source) != nil || m.Name == m.Source ||
		!filepath.IsAbs(m.Data) || filepath.Clean(m.Data) != m.Data || filepath.Base(m.Data) != m.Name {
		return result, errors.New("invalid follower resume authority or identity")
	}

	var mu sync.Mutex

	active, called := true, false

	var effectErr, violation error

	err := guard.GuardFollowerResume(ctx, m.OwnershipPaths(), port, func(e FollowerResumeEvidence) error {
		mu.Lock()
		defer mu.Unlock()

		if !active || called {
			violation = errors.New("follower guard invoked effect outside its scope or more than once")
			return violation
		}

		called = true
		result, effectErr = m.adoptResumedFollower(ctx, port, e)

		return effectErr
	})

	mu.Lock()
	active = false
	didCall, effectResult, callbackViolation := called, effectErr, violation
	mu.Unlock()

	if err == nil && !didCall {
		return FollowerResumeResult{}, errors.New("follower guard did not authorize effect")
	}

	return result, errors.Join(err, effectResult, callbackViolation)
}

type followerRegistration struct {
	data, conf os.FileInfo
	port       int
}

// VerifyFollowerRegistration checks the exact deterministic registration.
// It supplies no lifecycle or deletion authority.
func (m *Materialised) VerifyFollowerRegistration(port int) error {
	if m == nil || port < 1 || port > 65535 {
		return errors.New("invalid follower registration identity")
	}

	registration, err := m.followerRegistration()
	if err != nil {
		return err
	}

	if registration.port != port {
		return errors.New("follower registration port mismatch")
	}

	return nil
}

// ManagedRegistrationPort reads the directly registered port after validating
// the deterministic data directory and cluster name. It grants no authority.
func (m *Materialised) ManagedRegistrationPort() (int, error) {
	r, err := m.followerRegistration()
	return r.port, err
}

func (m *Materialised) followerRegistration() (followerRegistration, error) {
	var result followerRegistration

	data, err := os.Lstat(m.Data)
	if err != nil || !data.IsDir() || data.Mode()&os.ModeSymlink != 0 {
		return result, errors.New("unverifiable follower data")
	}

	dir := m.ConfDir()
	if m.FollowerResumeHost != nil {
		dir = m.FollowerResumeHost.RegistrationDirectory(m.Name)
	}

	d, conf, err := openFollowerConfDirectory(dir)
	if err != nil {
		return result, err
	}

	defer func() { _ = d.Close() }()

	seen, err := readFollowerRegistration(d, dir, m.Data, m.Name)
	if err != nil {
		return result, err
	}

	n, err := strconv.Atoi(seen["port"])
	if err != nil || n < 1 || n > 65535 {
		return result, errors.New("invalid follower registration port")
	}

	return followerRegistration{data: data, conf: conf, port: n}, nil
}

// openFollowerConfDirectory validates and opens a follower's registration
// directory, whose handle pins the directory for the whole check.
func openFollowerConfDirectory(dir string) (*os.File, os.FileInfo, error) {
	conf, err := os.Lstat(dir)
	if err != nil || !conf.IsDir() || conf.Mode()&os.ModeSymlink != 0 {
		return nil, nil, errors.New("unverifiable follower configuration")
	}

	d, err := os.Open(dir)
	if err != nil {
		return nil, nil, err
	}

	opened, err := d.Stat()
	if err != nil || !os.SameFile(conf, opened) {
		_ = d.Close()
		return nil, nil, errors.New("follower configuration replaced")
	}

	return d, conf, nil
}

// readFollowerRegistration reads the registration file's identity keys under
// the opened directory handle, and requires the data directory and cluster
// name to be this materialisation's.
func readFollowerRegistration(d *os.File, dir, data, name string) (map[string]string, error) {
	fd, err := xunix.Openat(int(d.Fd()), "postgresql.conf", xunix.O_RDONLY|xunix.O_NOFOLLOW|xunix.O_CLOEXEC|xunix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}

	f := os.NewFile(uintptr(fd), "postgresql.conf")
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1024*1024 {
		return nil, errors.New("invalid follower registration")
	}

	seen, err := scanFollowerRegistration(f)
	if err != nil {
		return nil, err
	}

	named, err := os.Lstat(filepath.Join(dir, "postgresql.conf"))
	if err != nil || !os.SameFile(info, named) || seen["data_directory"] != data || seen["cluster_name"] != "18/"+name {
		return nil, errors.New("follower registration mismatch")
	}

	return seen, nil
}

// scanFollowerRegistration collects the registration file's identity keys,
// refusing a duplicate key that could hide a mismatch.
func scanFollowerRegistration(r io.Reader) (map[string]string, error) {
	seen := make(map[string]string)

	scanner := bufio.NewScanner(io.LimitReader(r, 1024*1024+1))
	for scanner.Scan() {
		key, value, ok := strings.Cut(strings.TrimSpace(scanner.Text()), "=")

		key = strings.TrimSpace(key)
		if !ok || key != "data_directory" && key != "cluster_name" && key != "port" {
			continue
		}

		if _, exists := seen[key]; exists {
			return nil, errors.New("duplicate follower registration identity")
		}

		seen[key] = strings.Trim(strings.TrimSpace(value), "'")
	}

	return seen, scanner.Err()
}

func (m *Materialised) adoptResumedFollower(ctx context.Context, final int, e FollowerResumeEvidence) (FollowerResumeResult, error) {
	var result FollowerResumeResult

	before, temporary, err := m.resumedFollowerState(final, e)
	if err != nil {
		return result, err
	}

	dir, uid, err := m.followerReservationStore()
	if err != nil {
		return result, err
	}

	lockCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	a, err := openForkAllocator(lockCtx, dir, uid)
	if err != nil {
		return result, err
	}
	defer a.close()

	expected := e.TemporaryPort
	if temporary {
		expected = before.port
	}

	present, retained, err := resumeFollowerReservation(a, m.Name, uid, expected, temporary, e.ReservationIdentity)
	if err != nil {
		return result, err
	}

	result.ReservationRetained = retained
	if err := DropRecoverySettings(m.Data); err != nil {
		return result, err
	}

	if err := m.InstallCopyPlan(); err != nil {
		return result, err
	}

	if err := m.stopAndAdoptFollower(ctx, final); err != nil {
		return result, err
	}

	after, err := m.followerRegistration()
	if err != nil || !os.SameFile(before.data, after.data) || !os.SameFile(before.conf, after.conf) || after.port != final {
		return result, errors.Join(err, errors.New("follower registration changed during adoption"))
	}

	if present && !result.ReservationRetained {
		if err := a.root.Remove(m.Name); err != nil {
			return result, err
		}
	}

	m.Port = final

	return result, nil
}

// resumedFollowerState reads the registration adoption starts from and decides
// whether its port is the temporary one recovery ran on.
func (m *Materialised) resumedFollowerState(final int, e FollowerResumeEvidence) (followerRegistration, bool, error) {
	var before followerRegistration
	if e.TemporaryPort != 0 && (e.TemporaryPort < config.TemporaryPortMin || e.TemporaryPort > config.TemporaryPortMax) {
		return before, false, errors.New("invalid evidenced temporary port")
	}

	before, err := m.followerRegistration()
	if err != nil {
		return before, false, err
	}

	temporary := before.port != final && before.port >= config.TemporaryPortMin && before.port <= config.TemporaryPortMax
	if before.port != final && !temporary || temporary && e.TemporaryPort != 0 && before.port != e.TemporaryPort {
		return before, false, errors.New("foreign follower registration port")
	}

	return before, temporary, nil
}

// followerReservationStore is the reservation store the host supplied, or the
// live one when adoption runs without a host.
func (m *Materialised) followerReservationStore() (string, uint32, error) {
	if m.FollowerResumeHost != nil {
		return m.FollowerResumeHost.ReservationStore()
	}

	return forkReservationStore()
}

// resumeFollowerReservation verifies the temporary port's reservation. It
// reports whether one is present and whether it is retained: a reservation the
// evidence does not bind by digest names no exact artifact, so adoption keeps
// it for the caller's separate cleanup authority.
func resumeFollowerReservation(a *forkAllocator, name string, uid uint32, expected int, temporary bool, id ReservationIdentity) (present, retained bool, err error) {
	mapped, info, err := a.read(name, uid)

	present = err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, false, err
	}

	if !present {
		if !temporary {
			return false, false, nil
		}

		return false, false, a.unique("", expected, uid)
	}

	if expected != 0 && mapped != expected {
		return false, false, errors.New("foreign follower reservation")
	}

	retained, err = verifyFollowerReservationIdentity(a, name, info, id)
	if err != nil {
		return false, false, err
	}

	if err := a.unique(name, mapped, uid); err != nil {
		return false, false, err
	}

	return true, retained || expected == 0, nil
}

// verifyFollowerReservationIdentity checks the present reservation's inode and
// content against the evidence, and reports whether the evidence binds it at
// all.
func verifyFollowerReservationIdentity(a *forkAllocator, name string, info os.FileInfo, id ReservationIdentity) (bool, error) {
	if id.Inode == 0 {
		return true, nil
	}

	if !reservationIdentityMatches(info, id) {
		return false, errors.New("follower reservation replaced")
	}

	if id.Digest == "" {
		return true, nil
	}

	digest, err := a.digest(name, info)
	if err != nil || digest != id.Digest {
		return false, errors.New("follower reservation content replaced")
	}

	return false, nil
}

// stopAndAdoptFollower stops the resumed cluster and rewrites its registration
// for the live port, through the host when one is supplied.
func (m *Materialised) stopAndAdoptFollower(ctx context.Context, final int) error {
	if m.FollowerResumeHost != nil {
		if err := m.FollowerResumeHost.Stop(ctx); err != nil {
			return err
		}

		return m.FollowerResumeHost.AdoptRegistration(final)
	}

	if err := m.stop(ctx); err != nil {
		return err
	}

	return m.adoptRegistration(final)
}
