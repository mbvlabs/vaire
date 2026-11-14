package node

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

// followCleanup is used only by confirmed db.drop while dispatch holds the
// exclusive node lock. It pins the exact record, parents and born directories.
type followCleanup struct {
	owner                          followOwner
	record                         followOwnership
	recordInfo                     os.FileInfo
	dataRoot                       string
	dataParent, confParent         *os.Root
	dataAncestors, confAncestors   []materialisationAncestor
	dataParentInfo, confParentInfo os.FileInfo
	data, conf                     *os.File
	registeredPort                 int
}

var followCleanupReservation = backup.CleanupFollowerReservation
var followCleanupAbsent = backup.LockForkReservation
var followCleanupRegistration = func(name, source, dataRoot string, sourcePort, port int) error {
	m := backup.ResumeMaterialised(dataRoot, name, source, sourcePort, engine.Shell{}, "", nil)
	if err := m.VerifyFollowerRegistration(port); err != nil {
		return err
	}
	// postgresql-common may resolve included settings differently from the
	// main file. Refuse cleanup unless its effective data directory agrees.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "pg_conftool", "18", name, "show", "data_directory") //nolint:gosec // validated cluster name
	cmd.Env = []string{"LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != filepath.Join(dataRoot, name) {
		return errors.New("effective follower data directory is unverifiable")
	}
	return nil
}
var followCleanupDrop = func(ctx context.Context, env *Env, name string) error {
	return runtimeFor(env, false).Shell.Exec(ctx, "db.drop", engine.Command{Name: "pg_dropcluster", Args: []string{"18", name, "--stop"}}, env.Report)
}

func (a *followCleanup) close() {
	if a.data != nil {
		_ = a.data.Close()
	}
	if a.conf != nil {
		_ = a.conf.Close()
	}
	if a.dataParent != nil {
		_ = a.dataParent.Close()
	}
	if a.confParent != nil {
		_ = a.confParent.Close()
	}
}

func openFollowCleanup(cfg config.Cluster, spec config.ClusterSpec) (*followCleanup, error) {
	if !spec.IsFollower() {
		return nil, errors.New("follower configuration required")
	}
	records, err := backup.LoadRecords()
	if err != nil {
		return nil, err
	}
	owner := followOwner{Name: spec.Name, Source: spec.Follows, Generation: spec.FollowGeneration, Spec: spec}
	if aw, ok := records.AwaitingRecovery[spec.Name]; ok {
		owner.Awaiting = aw
	}
	info, err := trustedFollowRecord(spec.Name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	o, owned, err := readFollowOwnership(owner)
	if err != nil {
		return nil, err
	}
	if o.Attempt == nil {
		if !owned || o.Phase != followPhaseStarted {
			return nil, errors.New("follower cleanup has no bound birth evidence")
		}
		return nil, nil
	}
	if !owned || (o.Attempt.State != followAttemptRetained && o.Attempt.State != followAttemptCompleted) || o.Attempt.ArtifactStage != materialisationRegistered ||
		o.Attempt.ReservationIdentity.Inode == 0 || o.Attempt.ReservationIdentity.Digest == "" {
		return nil, errors.New("follower cleanup birth or ownership is unverifiable")
	}
	source, ok := cfg.ClusterByName(spec.Follows)
	if !ok || source.Stanza != o.Attempt.SourceStanza {
		return nil, errors.New("follower source identity changed")
	}
	uid, err := materialisationPostgresUID()
	if err != nil {
		return nil, err
	}
	a := &followCleanup{owner: owner, record: o, recordInfo: info, dataRoot: cfg.Server.DataRoot}
	a.dataParent, a.dataParentInfo, a.dataAncestors, err = openMaterialisationParent(a.dataRoot, uid, true)
	if err != nil {
		return nil, err
	}
	a.confParent, a.confParentInfo, a.confAncestors, err = openMaterialisationParent(materialisationConfigRoot, materialisationRecordUID, false)
	if err != nil {
		a.close()
		return nil, err
	}
	a.data, err = openMaterialisationChild(a.dataParent, spec.Name)
	if err != nil {
		a.close()
		return nil, err
	}
	a.conf, err = openMaterialisationChild(a.confParent, spec.Name)
	if err != nil {
		a.close()
		return nil, err
	}
	if err := a.validate(false); err != nil {
		a.close()
		return nil, err
	}
	a.registeredPort = o.Attempt.TemporaryPort
	if err := followCleanupRegistration(spec.Name, spec.Follows, a.dataRoot, source.Port, a.registeredPort); err != nil {
		if o.Attempt.State != followAttemptCompleted || followCleanupRegistration(spec.Name, spec.Follows, a.dataRoot, source.Port, spec.Port) != nil {
			a.close()
			return nil, err
		}
		a.registeredPort = spec.Port
	}
	return a, nil
}

func (a *followCleanup) validate(removed bool) error {
	info, err := trustedFollowRecord(a.owner.Name)
	if err != nil || !os.SameFile(a.recordInfo, info) {
		return errors.New("follower record replaced")
	}
	current, owned, err := readFollowOwnership(a.owner)
	if err != nil || !owned || !reflect.DeepEqual(current, a.record) {
		return errors.New("follower record changed")
	}
	if err := errors.Join(validateMaterialisationAncestry(a.dataAncestors), validateMaterialisationAncestry(a.confAncestors),
		validateMaterialisationParent(a.dataRoot, a.dataParentInfo), validateMaterialisationParent(materialisationConfigRoot, a.confParentInfo)); err != nil {
		return err
	}
	for _, item := range []struct {
		root *os.Root
		file *os.File
		id   materialisationFileID
		mode os.FileMode
	}{
		{a.dataParent, a.data, a.record.Attempt.DataIdentity, 0o700},
		{a.confParent, a.conf, a.record.Attempt.ConfigIdentity, 0o755},
	} {
		if !removed {
			if err := validateMaterialisationDir(item.root, a.owner.Name, item.id, item.file, item.mode); err != nil {
				return err
			}
			continue
		}
		if _, err := item.root.Lstat(a.owner.Name); !errors.Is(err, os.ErrNotExist) {
			return errors.New("follower cleanup path remains or was replaced")
		}
		info, err := item.file.Stat()
		if err != nil {
			return err
		}
		id, err := materialisationArtifactID(info)
		st, ok := info.Sys().(*syscall.Stat_t)
		if err != nil || !ok || id != item.id || st.Nlink != 0 {
			return errors.New("original follower artifact not removed")
		}
	}
	return nil
}

func (a *followCleanup) remove(ctx context.Context, env *Env, cfg config.Cluster) error {
	o := a.record
	source, ok := cfg.ClusterByName(o.Source)
	if !ok {
		return errors.New("follower source missing")
	}
	return followCleanupReservation(ctx, o.Database, o.Attempt.TemporaryPort, o.Attempt.ReservationIdentity, func() error {
		if err := a.validate(false); err != nil {
			return err
		}
		if err := followCleanupRegistration(o.Database, o.Source, a.dataRoot, source.Port, a.registeredPort); err != nil {
			return err
		}
		if err := followCleanupDrop(ctx, env, o.Database); err != nil {
			return err
		}
		if err := a.dataParent.RemoveAll(o.Database); err != nil {
			return err
		}
		if err := a.confParent.RemoveAll(o.Database); err != nil {
			return err
		}
		if err := a.validate(true); err != nil {
			return err
		}
		return errors.Join(materialisationSyncArtifactParent(a.dataParent), materialisationSyncArtifactParent(a.confParent))
	})
}

func (a *followCleanup) retire(ctx context.Context) error {
	o := a.record
	unlock, err := followCleanupAbsent(ctx, o.Database, o.Attempt.TemporaryPort, false)
	if err != nil {
		return err
	}
	defer unlock()
	if err := a.validate(true); err != nil {
		return err
	}
	path, err := followOwnershipPath(o.Database)
	if err != nil {
		return err
	}
	return os.Remove(path)
}
