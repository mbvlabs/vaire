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
	"syscall"
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
	conf, err := os.Lstat(dir)
	if err != nil || !conf.IsDir() || conf.Mode()&os.ModeSymlink != 0 {
		return result, errors.New("unverifiable follower configuration")
	}
	d, err := os.Open(dir) //nolint:gosec // trusted fixed host path
	if err != nil {
		return result, err
	}
	defer func() { _ = d.Close() }()
	opened, err := d.Stat()
	if err != nil || !os.SameFile(conf, opened) {
		return result, errors.New("follower configuration replaced")
	}
	fd, err := xunix.Openat(int(d.Fd()), "postgresql.conf", xunix.O_RDONLY|xunix.O_NOFOLLOW|xunix.O_CLOEXEC|xunix.O_NONBLOCK, 0)
	if err != nil {
		return result, err
	}
	f := os.NewFile(uintptr(fd), "postgresql.conf")
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1024*1024 {
		return result, errors.New("invalid follower registration")
	}
	seen := make(map[string]string)
	scanner := bufio.NewScanner(io.LimitReader(f, 1024*1024+1))
	for scanner.Scan() {
		key, value, ok := strings.Cut(strings.TrimSpace(scanner.Text()), "=")
		key = strings.TrimSpace(key)
		if !ok || key != "data_directory" && key != "cluster_name" && key != "port" {
			continue
		}
		if _, exists := seen[key]; exists {
			return result, errors.New("duplicate follower registration identity")
		}
		seen[key] = strings.Trim(strings.TrimSpace(value), "'")
	}
	if err := scanner.Err(); err != nil {
		return result, err
	}
	named, err := os.Lstat(filepath.Join(dir, "postgresql.conf"))
	if err != nil || !os.SameFile(info, named) || seen["data_directory"] != m.Data || seen["cluster_name"] != "18/"+m.Name {
		return result, errors.New("follower registration mismatch")
	}
	n, err := strconv.Atoi(seen["port"])
	if err != nil || n < 1 || n > 65535 {
		return result, errors.New("invalid follower registration port")
	}
	return followerRegistration{data: data, conf: conf, port: n}, nil
}

func (m *Materialised) adoptResumedFollower(ctx context.Context, final int, e FollowerResumeEvidence) (FollowerResumeResult, error) {
	var result FollowerResumeResult
	if e.TemporaryPort != 0 && (e.TemporaryPort < config.TemporaryPortMin || e.TemporaryPort > config.TemporaryPortMax) {
		return result, errors.New("invalid evidenced temporary port")
	}
	before, err := m.followerRegistration()
	if err != nil {
		return result, err
	}
	temporary := before.port != final && before.port >= config.TemporaryPortMin && before.port <= config.TemporaryPortMax
	if before.port != final && !temporary || temporary && e.TemporaryPort != 0 && before.port != e.TemporaryPort {
		return result, errors.New("foreign follower registration port")
	}
	var dir string
	var uid uint32
	if m.FollowerResumeHost != nil {
		dir, uid, err = m.FollowerResumeHost.ReservationStore()
	} else {
		dir, uid, err = forkReservationStore()
	}
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
	mapped, mappedInfo, err := a.read(m.Name, uid)
	present := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	expected := e.TemporaryPort
	if temporary {
		expected = before.port
	}
	if present {
		if expected != 0 && mapped != expected {
			return result, errors.New("foreign follower reservation")
		}
		if e.ReservationIdentity.Inode != 0 {
			st, ok := mappedInfo.Sys().(*syscall.Stat_t)
			if !ok || uint64(st.Dev) != e.ReservationIdentity.Device || st.Ino != e.ReservationIdentity.Inode || st.Uid != e.ReservationIdentity.UID {
				return result, errors.New("follower reservation replaced")
			}
			if e.ReservationIdentity.Digest != "" {
				digest, err := a.digest(m.Name, mappedInfo)
				if err != nil || digest != e.ReservationIdentity.Digest {
					return result, errors.New("follower reservation content replaced")
				}
			} else {
				result.ReservationRetained = true
			}
		} else {
			result.ReservationRetained = true
		}
		if err := a.unique(m.Name, mapped, uid); err != nil {
			return result, err
		}
		result.ReservationRetained = result.ReservationRetained || expected == 0
	} else if temporary {
		if err := a.unique("", before.port, uid); err != nil {
			return result, err
		}
	}
	if err := DropRecoverySettings(m.Data); err != nil {
		return result, err
	}
	if m.FollowerResumeHost != nil {
		err = m.FollowerResumeHost.Stop(ctx)
	} else {
		err = m.stop(ctx)
	}
	if err != nil {
		return result, err
	}
	if m.FollowerResumeHost != nil {
		err = m.FollowerResumeHost.AdoptRegistration(final)
	} else {
		err = m.adoptRegistration(final)
	}
	if err != nil {
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
