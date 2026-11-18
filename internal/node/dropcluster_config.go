package node

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	dropConfigQuarantinePrefix = ".vaire-drop-quarantine-"
	dropConfigQuarantineEntry  = "candidate"
)

// A registered cluster has /etc/postgresql/18/<name>/postgresql.conf. Debian's
// pg_dropcluster removes that registration but can leave the directory and
// conf.d behind. This pins the exact registered directory before the external
// drop; only that same inode may be removed afterwards.
type registeredDropConfig struct {
	name       string
	parent     *os.Root
	parentInfo os.FileInfo
	ancestors  []materialisationAncestor
	dir        *os.File
	dirID      materialisationFileID
}

// errDropConfigNotRegistered means there is no configuration directory to
// clean up. The drop itself still has to run and report its own outcome.
var errDropConfigNotRegistered = errors.New("no registered configuration directory")

// Tests interleave a concurrent postgres-owned actor around the atomic
// quarantine move. Production leaves both hooks as no-ops.
var dropConfigBeforeQuarantine = func() {}
var dropConfigAfterQuarantine = func() {}

func validDropConfigName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.ContainsAny(name, `/\`) && strings.IndexByte(name, 0) < 0
}

// openRegisteredDropConfig pins /etc/postgresql/18 and the registered
// directory name. An existing directory without postgresql.conf is an
// unregistered remnant, never cleanup authority, and is refused.
func openRegisteredDropConfig(name string) (*registeredDropConfig, error) {
	if !validDropConfigName(name) {
		return nil, errors.New("invalid cluster name for configuration cleanup")
	}

	uid, err := materialisationPostgresUID()
	if err != nil {
		return nil, err
	}

	parent, parentInfo, ancestors, err := openMaterialisationConfigParent(uid)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errDropConfigNotRegistered
		}

		return nil, err
	}

	named, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		_ = parent.Close()
		return nil, errDropConfigNotRegistered
	}

	if err != nil {
		_ = parent.Close()
		return nil, err
	}

	id, err := materialisationArtifactID(named)
	if err != nil || id.UID != uid || named.Mode().Perm() != 0o755 {
		_ = parent.Close()
		return nil, errors.New("registered configuration directory is unsafe")
	}

	dir, err := openMaterialisationChild(parent, name)
	if err != nil {
		_ = parent.Close()
		return nil, err
	}

	c := &registeredDropConfig{
		name: name, parent: parent, parentInfo: parentInfo,
		ancestors: ancestors, dir: dir, dirID: id,
	}
	if err := c.validatePinned(); err != nil {
		c.close()
		return nil, err
	}

	present, err := c.registrationPresent()
	if err != nil {
		c.close()
		return nil, err
	}

	if !present {
		c.close()
		return nil, errors.New("configuration directory is not registered")
	}

	return c, nil
}

func (c *registeredDropConfig) close() {
	if c.dir != nil {
		_ = c.dir.Close()
	}

	if c.parent != nil {
		_ = c.parent.Close()
	}
}

func (c *registeredDropConfig) validatePinned() error {
	if err := validateMaterialisationAncestry(c.ancestors); err != nil {
		return err
	}

	if err := validateMaterialisationParent(materialisationConfigRoot, c.parentInfo); err != nil {
		return err
	}

	return validateMaterialisationDir(c.parent, c.name, c.dirID, c.dir, 0o755)
}

// registrationPresentIn answers whether the pinned directory still carries
// the standard registration file. The directory fd is used so the answer
// cannot be redirected by a replaced path.
func registrationPresentIn(dir *os.File) (bool, error) {
	fd, err := unix.Openat(int(dir.Fd()), "postgresql.conf",
		unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}

	if err != nil {
		return false, err
	}

	f := os.NewFile(uintptr(fd), "postgresql.conf")
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return false, err
	}

	if !info.Mode().IsRegular() {
		return false, errors.New("cluster registration is not a regular file")
	}

	return true, nil
}

func (c *registeredDropConfig) registrationPresent() (bool, error) {
	return registrationPresentIn(c.dir)
}

func (c *registeredDropConfig) validateRegistered() error {
	if err := c.validatePinned(); err != nil {
		return err
	}

	present, err := c.registrationPresent()
	if err != nil {
		return err
	}

	if !present {
		return errDropConfigNotRegistered
	}

	return nil
}

// drop runs the external effect while the registered directory and its parent
// remain pinned, then removes only the verified remnant the effect left.
func (c *registeredDropConfig) drop(effect func() error) error {
	if effect == nil {
		return errors.New("missing cluster drop effect")
	}

	if err := c.validateRegistered(); err != nil {
		return err
	}

	if err := effect(); err != nil {
		return err
	}

	return c.finish()
}

type dropConfigQuarantine struct {
	name string
	root *os.Root
}

func (q *dropConfigQuarantine) candidatePath() string {
	return q.root.Name() + string(os.PathSeparator) + dropConfigQuarantineEntry
}

// createQuarantine creates a fresh root-owned 0700 directory inside the pinned
// config parent. The directory is held by fd, so later renames of its public
// name cannot redirect the candidate move or removal.
func (c *registeredDropConfig) createQuarantine() (*dropConfigQuarantine, error) {
	for range 8 {
		var raw [12]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return nil, err
		}

		name := dropConfigQuarantinePrefix + hex.EncodeToString(raw[:])
		if err := c.parent.Mkdir(name, 0o700); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}

			return nil, err
		}

		named, err := c.parent.Lstat(name)
		if err != nil {
			return nil, err
		}

		qroot, err := c.parent.OpenRoot(name)
		if err != nil {
			return nil, err
		}

		opened, statErr := qroot.Stat(".")

		id, idErr := materialisationArtifactID(named)
		if statErr != nil || idErr != nil || id.UID != materialisationRecordUID ||
			named.Mode().Perm() != 0o700 || !os.SameFile(named, opened) || opened.Mode().Perm() != 0o700 {
			_ = qroot.Close()
			return nil, errors.New("configuration quarantine is unsafe")
		}

		return &dropConfigQuarantine{name: name, root: qroot}, nil
	}

	return nil, errors.New("cannot create configuration quarantine")
}

func (c *registeredDropConfig) removeQuarantine(q *dropConfigQuarantine) error {
	named, err := c.parent.Lstat(q.name)
	if err != nil {
		return err
	}

	opened, err := q.root.Stat(".")
	if err != nil {
		return err
	}

	id, err := materialisationArtifactID(opened)
	if err != nil || id.UID != materialisationRecordUID || !os.SameFile(named, opened) ||
		named.Mode().Perm() != 0o700 || opened.Mode().Perm() != 0o700 {
		return errors.New("configuration quarantine changed")
	}

	return c.parent.Remove(q.name)
}

// moveIntoQuarantine atomically moves whatever currently sits at the public
// name into the private jail. It never removes or follows the candidate.
func (c *registeredDropConfig) moveIntoQuarantine(q *dropConfigQuarantine) error {
	parent, err := c.parent.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()

	jail, err := q.root.Open(".")
	if err != nil {
		return err
	}

	defer func() { _ = jail.Close() }()

	return renameNoReplaceAt(int(parent.Fd()), c.name, int(jail.Fd()), dropConfigQuarantineEntry)
}

// restoreFromQuarantine returns a non-authoritative candidate to an absent
// public name without replacing anything that appeared there meanwhile.
func (c *registeredDropConfig) restoreFromQuarantine(q *dropConfigQuarantine) (bool, error) {
	if _, err := c.parent.Lstat(c.name); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}

	parent, err := c.parent.Open(".")
	if err != nil {
		return false, err
	}
	defer func() { _ = parent.Close() }()

	jail, err := q.root.Open(".")
	if err != nil {
		return false, err
	}

	defer func() { _ = jail.Close() }()

	err = renameNoReplaceAt(int(jail.Fd()), dropConfigQuarantineEntry, int(parent.Fd()), c.name)
	if err == nil {
		return true, nil
	}

	if errors.Is(err, unix.EEXIST) {
		return false, nil
	}

	return false, err
}

// rejectQuarantine never recursively removes a candidate that failed private
// validation. It restores it only into an absent public name, or keeps it in
// the held jail and reports where it is.
func (c *registeredDropConfig) rejectQuarantine(q *dropConfigQuarantine, reason error) error {
	restored, err := c.restoreFromQuarantine(q)
	if err != nil {
		return errors.Join(reason, fmt.Errorf("unknown configuration retained at %s: %w", q.candidatePath(), err))
	}

	if !restored {
		return fmt.Errorf("%w; unknown configuration retained at %s", reason, q.candidatePath())
	}

	if err := c.removeQuarantine(q); err != nil {
		return errors.Join(reason, fmt.Errorf("rejected configuration was restored, quarantine remains at %s: %w", q.root.Name(), err))
	}

	return reason
}

func (c *registeredDropConfig) requirePinnedRemoved() error {
	info, err := c.dir.Stat()
	if err != nil {
		return err
	}

	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Nlink != 0 {
		return errors.New("original configuration directory was not removed")
	}

	return nil
}

// finishAbsent accepts complete absence only when the pinned original inode is
// actually unlinked. A rename elsewhere leaves Nlink non-zero and is refused.
func (c *registeredDropConfig) finishAbsent() error {
	if err := c.requirePinnedRemoved(); err != nil {
		return err
	}

	if err := validateMaterialisationAncestry(c.ancestors); err != nil {
		return err
	}

	if err := validateMaterialisationParent(materialisationConfigRoot, c.parentInfo); err != nil {
		return err
	}

	if _, err := c.parent.Lstat(c.name); err == nil {
		return errors.New("configuration name was recreated during drop")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return materialisationSyncArtifactParent(c.parent)
}

// finish atomically quarantines the public name, validates the moved inode and
// its registration inside the private jail, and only then removes it through
// the jail root. The public name is never passed to RemoveAll.
//
// Quarantine confines deletion to the validated original inode and prevents
// public-name substitution. It does not revoke existing handles or freeze
// contents. Safety relies on trusted root/postgres writers not concurrently
// repurposing the retired tree; operation locks serialize cooperating vaire
// operations, not the postgres service itself.
func (c *registeredDropConfig) finish() error {
	if err := c.validateParents(); err != nil {
		return err
	}

	if _, err := c.parent.Lstat(c.name); errors.Is(err, os.ErrNotExist) {
		return c.finishAbsent()
	} else if err != nil {
		return err
	}

	return c.finishQuarantined()
}

func (c *registeredDropConfig) validateParents() error {
	if err := validateMaterialisationAncestry(c.ancestors); err != nil {
		return err
	}

	return validateMaterialisationParent(materialisationConfigRoot, c.parentInfo)
}

func (c *registeredDropConfig) finishQuarantined() error {
	q, err := c.createQuarantine()
	if err != nil {
		return err
	}
	defer func() { _ = q.root.Close() }()

	dropConfigBeforeQuarantine()

	err = c.moveIntoQuarantine(q)
	if err != nil {
		cleanupErr := c.removeQuarantine(q)
		if errors.Is(err, unix.ENOENT) {
			if cleanupErr != nil {
				return cleanupErr
			}

			return c.finishAbsent()
		}

		return errors.Join(err, cleanupErr)
	}

	dropConfigAfterQuarantine()

	if err := c.validateAndRemoveQuarantined(q); err != nil {
		return err
	}

	if err := c.removeQuarantine(q); err != nil {
		return err
	}
	// Success is not complete until the post-jail state matches a real
	// removal: original inode unlinked, ancestors and parent pinned, public
	// name absent, and the parent synced. A replacement that appeared under
	// the public name is preserved and refuses success.
	return c.finishAbsent()
}

func (c *registeredDropConfig) validateAndRemoveQuarantined(q *dropConfigQuarantine) error {
	if err := validateMaterialisationDir(q.root, dropConfigQuarantineEntry, c.dirID, c.dir, 0o755); err != nil {
		return c.rejectQuarantine(q, err)
	}

	candidate, err := openMaterialisationChild(q.root, dropConfigQuarantineEntry)
	if err != nil {
		return c.rejectQuarantine(q, err)
	}

	defer func() { _ = candidate.Close() }()

	if err := c.requireQuarantinedUnregistered(candidate); err != nil {
		return c.rejectQuarantine(q, err)
	}

	if err := c.validateParents(); err != nil {
		return c.rejectQuarantine(q, err)
	}
	// Recheck inside the private jail immediately before destruction.
	if err := c.requireQuarantinedUnregistered(candidate); err != nil {
		return c.rejectQuarantine(q, err)
	}

	if err := q.root.RemoveAll(dropConfigQuarantineEntry); err != nil {
		return err
	}

	if _, err := q.root.Lstat(dropConfigQuarantineEntry); !errors.Is(err, os.ErrNotExist) {
		return errors.New("quarantined configuration directory remains")
	}

	return c.requirePinnedRemoved()
}

func (c *registeredDropConfig) requireQuarantinedUnregistered(candidate *os.File) error {
	present, err := registrationPresentIn(candidate)
	if err != nil {
		return err
	}

	if present {
		return errors.New("cluster registration was recreated before removal")
	}

	return nil
}

// dropRegisteredCluster wraps the external pg_dropcluster effect. It pins a
// registered configuration directory first and removes only that same
// directory when a successful drop leaves it behind. Unregistered remnants
// and replaced, re-owned or symlinked targets are refused without removal. If
// no configuration directory exists, the effect still runs and nothing is
// cleaned. The data directory is never touched here.
func dropRegisteredCluster(name string, effect func() error) error {
	if effect == nil {
		return errors.New("missing cluster drop effect")
	}

	c, err := openRegisteredDropConfig(name)
	if errors.Is(err, errDropConfigNotRegistered) {
		return effect()
	}

	if err != nil {
		return err
	}

	defer c.close()

	return c.drop(effect)
}
