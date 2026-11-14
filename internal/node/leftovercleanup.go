package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/steps"
)

var legacyLeftoverReservation = backup.CleanupCurrentLegacyReservation
var legacyLeftoverPort = func(root, name, source string) (int, error) {
	m := backup.ResumeMaterialisedWithHost(root, name, source, 0, engine.Shell{}, "", nil, materialisationConfigRoot, nil)
	return m.ManagedRegistrationPort()
}
var legacyLeftoverEffectiveSetting = func(name, setting string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "pg_conftool", "18", name, "show", setting) //nolint:gosec // validated managed cluster and fixed settings
	cmd.Env = []string{"LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func legacyLeftoverRegistration(name, source, root string, _ int, port int) error {
	direct, err := legacyLeftoverPort(root, name, source)
	if err != nil || direct != port {
		return errors.New("direct legacy registration is unverifiable")
	}
	for _, setting := range []struct{ name, value string }{
		{"data_directory", filepath.Join(root, name)},
		{"cluster_name", "18/" + name},
		{"port", strconv.Itoa(port)},
	} {
		got, err := legacyLeftoverEffectiveSetting(name, setting.name)
		if err != nil || got != setting.value {
			return fmt.Errorf("effective legacy %s is unverifiable", setting.name)
		}
	}
	return nil
}

var legacyLeftoverExists = stepsClusterExists

// stepsClusterExists is separated so tests never invoke host PostgreSQL tools.
func stepsClusterExists(ctx context.Context, name string) bool {
	return steps.ClusterExists(ctx, name)
}

// legacyLeftoverCleanup pins the current managed registration. These IDs are
// operation-local snapshots, not evidence of the artifacts' historical birth.
type legacyLeftoverCleanup struct {
	record                       backup.Leftover
	root                         string
	port                         int
	dataParent, confParent       *os.Root
	dataInfo, confInfo           os.FileInfo
	dataAncestors, confAncestors []materialisationAncestor
	data, conf                   *os.File
	dataID, confID               materialisationFileID
}

func (a *legacyLeftoverCleanup) close() {
	for _, f := range []*os.File{a.data, a.conf} {
		if f != nil {
			_ = f.Close()
		}
	}
	for _, r := range []*os.Root{a.dataParent, a.confParent} {
		if r != nil {
			_ = r.Close()
		}
	}
}

func validGeneratedLeftoverName(db, name string) bool {
	if config.CheckDatabaseName(db) != nil || name == "" {
		return false
	}
	suffix := strings.TrimPrefix(name, db+"__pre_restore_")
	if suffix == name {
		return false
	}
	suffix = strings.TrimSuffix(suffix, "b")
	t, err := time.Parse("20060102T150405", suffix)
	return err == nil && backup.PreRestoreName(db, t) == db+"__pre_restore_"+suffix
}

func validateLegacyLeftover(s *backup.Records, cfg config.Cluster, l backup.Leftover) error {
	if !validGeneratedLeftoverName(l.Database, l.Name) || l.RestoreID == "" {
		return errors.New("invalid leftover identity")
	}
	matches := 0
	for _, other := range s.Leftovers {
		if other.Name == l.Name {
			matches++
			if other != l {
				return errors.New("ambiguous leftover")
			}
		}
	}
	if matches != 1 {
		return errors.New("leftover missing or ambiguous")
	}
	for _, c := range cfg.Clusters {
		if c.Name == l.Name {
			return errors.New("leftover is configured")
		}
	}
	for db, r := range s.Restores {
		if db == l.Database && (r.Attempt != nil || r.MaterialisationPending || r.Phase != backup.PhaseReady) || r.Aside == l.Name ||
			r.Attempt != nil && r.Attempt.Target == l.Name || backup.RestoringName(db) == l.Name {
			return errors.New("leftover conflicts with restore")
		}
	}
	for db, u := range s.Undoing {
		if db == l.Database || u.Aside == l.Name || u.From.Name == l.Name {
			return errors.New("leftover participates in undo")
		}
	}
	for _, name := range s.Drilling {
		if name == l.Name {
			return errors.New("leftover participates in drill")
		}
	}
	for _, attempt := range s.DrillAttempts {
		if attempt.Target == l.Name {
			return errors.New("leftover conflicts with drill attempt")
		}
	}
	if _, ok := s.AwaitingRecovery[l.Name]; ok {
		return errors.New("leftover conflicts with awaiting recovery")
	}
	if _, ok := s.Unfollowing[l.Name]; ok {
		return errors.New("leftover conflicts with unfollow")
	}
	// An existing ownership marker, including a corrupt one, is never
	// downgraded to the legacy current-registration trust model.
	for _, dir := range []string{materialisationStateDir, filepath.Dir(backup.RecordsPath)} {
		if _, err := materialisationAncestry(dir, materialisationRecordUID, false); err != nil {
			return err
		}
	}
	for _, path := range []string{
		filepath.Join(materialisationStateDir, "materialise-"+l.Name+".json"),
		filepath.Join(filepath.Dir(backup.RecordsPath), "follow-"+l.Name+".json"),
	} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(err, errors.New("leftover conflicts with ownership marker"))
		}
	}
	return nil
}

func openLegacyLeftover(cfg config.Cluster, l backup.Leftover) (*legacyLeftoverCleanup, error) {
	s, err := backup.LoadRecords()
	if err != nil {
		return nil, err
	}
	if err := validateLegacyLeftover(&s, cfg, l); err != nil {
		return nil, err
	}
	uid, err := materialisationPostgresUID()
	if err != nil {
		return nil, err
	}
	a := &legacyLeftoverCleanup{record: l, root: cfg.Server.DataRoot}
	a.dataParent, a.dataInfo, a.dataAncestors, err = openMaterialisationParent(a.root, uid, true)
	if err != nil {
		return nil, err
	}
	a.confParent, a.confInfo, a.confAncestors, err = openMaterialisationParent(materialisationConfigRoot, materialisationRecordUID, false)
	if err != nil {
		a.close()
		return nil, err
	}
	fail := func(err error) (*legacyLeftoverCleanup, error) { a.close(); return nil, err }
	a.data, err = openMaterialisationChild(a.dataParent, l.Name)
	if err != nil {
		return fail(err)
	}
	a.conf, err = openMaterialisationChild(a.confParent, l.Name)
	if err != nil {
		return fail(err)
	}
	for _, item := range []struct {
		file *os.File
		id   *materialisationFileID
		mode os.FileMode
	}{{a.data, &a.dataID, 0o700}, {a.conf, &a.confID, 0o755}} {
		info, err := item.file.Stat()
		if err != nil {
			return fail(err)
		}
		*item.id, err = materialisationArtifactID(info)
		if err != nil || item.id.UID != uid || info.Mode().Perm() != item.mode {
			return fail(errors.New("untrusted legacy leftover directory"))
		}
	}
	if err := a.validate(cfg, false); err != nil {
		return fail(err)
	}
	a.port, err = legacyLeftoverPort(a.root, l.Name, l.Database)
	if err != nil || a.port < config.TemporaryPortMin || a.port > config.TemporaryPortMax || l.Port != 0 && a.port != l.Port {
		return fail(errors.New("legacy leftover port is unverifiable"))
	}
	if err := a.registration(cfg); err != nil {
		return fail(err)
	}
	return a, nil
}

func (a *legacyLeftoverCleanup) registration(cfg config.Cluster) error {
	spec, _ := cfg.ClusterByName(a.record.Database)
	if err := legacyLeftoverRegistration(a.record.Name, a.record.Database, a.root, spec.Port, a.port); err != nil {
		return err
	}
	return a.validate(cfg, false)
}

func (a *legacyLeftoverCleanup) validate(cfg config.Cluster, removed bool) error {
	s, err := backup.LoadRecords()
	if err != nil {
		return err
	}
	if err := validateLegacyLeftover(&s, cfg, a.record); err != nil {
		return err
	}
	if err := errors.Join(validateMaterialisationAncestry(a.dataAncestors), validateMaterialisationAncestry(a.confAncestors),
		validateMaterialisationParent(a.root, a.dataInfo), validateMaterialisationParent(materialisationConfigRoot, a.confInfo)); err != nil {
		return err
	}
	for _, item := range []struct {
		root *os.Root
		file *os.File
		id   materialisationFileID
		mode os.FileMode
	}{{a.dataParent, a.data, a.dataID, 0o700}, {a.confParent, a.conf, a.confID, 0o755}} {
		if !removed {
			if err := validateMaterialisationDir(item.root, a.record.Name, item.id, item.file, item.mode); err != nil {
				return err
			}
			continue
		}
		if _, err := item.root.Lstat(a.record.Name); !errors.Is(err, os.ErrNotExist) {
			return errors.New("leftover artifact remains or was replaced")
		}
		info, err := item.file.Stat()
		if err != nil {
			return err
		}
		id, err := materialisationArtifactID(info)
		st, ok := info.Sys().(*syscall.Stat_t)
		if err != nil || !ok || id != item.id || st.Nlink != 0 {
			return errors.New("original leftover artifact not removed")
		}
	}
	return nil
}

func (a *legacyLeftoverCleanup) remove(ctx context.Context, env *Env) error {
	return legacyLeftoverReservation(ctx, a.record.Name, a.port, func() error {
		if err := a.registration(env.Cluster); err != nil {
			return err
		}
		dropErr := restoreCleanupDrop(ctx, env, a.record.Name)
		if err := errors.Join(dropErr, a.validateAfterDrop(env.Cluster)); err != nil {
			return err
		}
		for _, item := range []struct {
			root *os.Root
			file *os.File
			id   materialisationFileID
			mode os.FileMode
		}{{a.dataParent, a.data, a.dataID, 0o700}, {a.confParent, a.conf, a.confID, 0o755}} {
			if _, err := item.root.Lstat(a.record.Name); errors.Is(err, os.ErrNotExist) {
				continue
			} else if err != nil {
				return err
			}
			if err := a.validateRecordAndParents(env.Cluster); err != nil {
				return err
			}
			if err := validateMaterialisationDir(item.root, a.record.Name, item.id, item.file, item.mode); err != nil {
				return err
			}
			if err := item.root.RemoveAll(a.record.Name); err != nil {
				return err
			}
		}
		if err := a.validate(env.Cluster, true); err != nil {
			return err
		}
		return errors.Join(materialisationSyncArtifactParent(a.dataParent), materialisationSyncArtifactParent(a.confParent))
	})
}

func (a *legacyLeftoverCleanup) validateAfterDrop(cfg config.Cluster) error {
	if err := a.validateRecordAndParents(cfg); err != nil {
		return err
	}
	for _, item := range []struct {
		root *os.Root
		file *os.File
		id   materialisationFileID
		mode os.FileMode
	}{{a.dataParent, a.data, a.dataID, 0o700}, {a.confParent, a.conf, a.confID, 0o755}} {
		if _, err := item.root.Lstat(a.record.Name); err == nil {
			if err := validateMaterialisationDir(item.root, a.record.Name, item.id, item.file, item.mode); err != nil {
				return err
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		info, err := item.file.Stat()
		if err != nil {
			return err
		}
		id, err := materialisationArtifactID(info)
		st, ok := info.Sys().(*syscall.Stat_t)
		if err != nil || !ok || id != item.id || st.Nlink != 0 {
			return errors.New("leftover artifact moved or replaced during drop")
		}
	}
	return nil
}

func (a *legacyLeftoverCleanup) validateRecordAndParents(cfg config.Cluster) error {
	s, err := backup.LoadRecords()
	if err != nil {
		return err
	}
	return errors.Join(validateLegacyLeftover(&s, cfg, a.record),
		validateMaterialisationAncestry(a.dataAncestors), validateMaterialisationAncestry(a.confAncestors),
		validateMaterialisationParent(a.root, a.dataInfo), validateMaterialisationParent(materialisationConfigRoot, a.confInfo))
}

func retireLegacyLeftover(cfg config.Cluster, l backup.Leftover) error {
	return backup.UpdateRecords(func(s *backup.Records) error {
		if err := validateLegacyLeftover(s, cfg, l); err != nil {
			return fmt.Errorf("cannot retire leftover: %w", err)
		}
		for i := range s.Leftovers {
			if s.Leftovers[i] == l {
				s.Leftovers = append(s.Leftovers[:i], s.Leftovers[i+1:]...)
				return nil
			}
		}
		return errors.New("leftover disappeared")
	})
}
