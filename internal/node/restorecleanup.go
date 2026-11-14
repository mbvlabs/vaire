package node

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
)

var restoreCleanupReservation = backup.CleanupRestoreReservation
var restoreCleanupAbsence = backup.LockForkReservation
var restoreCleanupRegistration = func(name, source, dataRoot string, sourcePort, port int) error {
	if err := followCleanupRegistration(name, source, dataRoot, sourcePort, port); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "pg_conftool", "18", name, "show", "port") //nolint:gosec // exact validated managed cluster name
	cmd.Env = []string{"LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	out, err := cmd.Output()
	effective, parseErr := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || parseErr != nil || effective != port {
		return errors.New("effective restore port is unverifiable")
	}
	return nil
}
var restoreCleanupDrop = followCleanupDrop
var restoreAbortReload = reloadPoolers

// restoreCleanup pins both born directories and checks the full restore
// record before every destructive transition.
type restoreCleanup struct {
	record                       backup.Restore
	dataRoot                     string
	dataParent, confParent       *os.Root
	dataInfo, confInfo           os.FileInfo
	dataAncestors, confAncestors []materialisationAncestor
	data, conf                   *os.File
}

func (a *restoreCleanup) close() {
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

func openRestoreCleanup(cfg config.Cluster, r backup.Restore) (*restoreCleanup, error) {
	t := r.Attempt
	if t == nil || r.ID == "" || r.InstanceID == "" || r.OperationID == "" ||
		r.Phase != backup.PhaseFailed || !r.MaterialisationPending || r.MaterialisationCleaned ||
		r.Aside != "" || r.Database != t.Source || r.ID != t.JobID || r.InstanceID != t.InstanceID ||
		r.OperationID != t.OperationID || t.Target != backup.RestoringName(r.Database) ||
		(t.Kind != "restore" && t.Kind != "recover") || (t.Kind == "recover") != r.Recover ||
		t.RecordDigest == "" || t.TemporaryPort < config.TemporaryPortMin || t.TemporaryPort > config.TemporaryPortMax ||
		t.ArtifactStage != materialisationRegistered || t.DataIdentity.Inode == 0 ||
		t.ConfigIdentity.Inode == 0 || t.ReservationIdentity.Inode == 0 || t.ReservationIdentity.Digest == "" ||
		(t.Reason != "recovery_requirements_unmet" && t.Reason != "recovery_parameter_mismatch") ||
		t.DataPath != filepath.Join(cfg.Server.DataRoot, t.Target) ||
		t.ConfigPath != filepath.Join(materialisationConfigRoot, t.Target) {
		return nil, incompleteMaterialisation(backup.RestoringName(r.Database))
	}
	instance, err := currentFollowInstance()
	if err != nil || instance != r.InstanceID {
		return nil, incompleteMaterialisation(t.Target)
	}
	spec, ok := cfg.ClusterByName(r.Database)
	if !ok || spec.Port != t.SourcePort || spec.Stanza != t.Stanza {
		return nil, incompleteMaterialisation(t.Target)
	}
	state, err := restoreAbsenceState()
	if err != nil {
		return nil, err
	}
	job, err := materialisationJob(state, r.ID)
	if err != nil || job.Job != t.Kind || job.DB != r.Database || !job.Final() {
		return nil, incompleteMaterialisation(t.Target)
	}
	uid, err := materialisationPostgresUID()
	if err != nil || t.DataIdentity.UID != uid || t.ConfigIdentity.UID != uid {
		return nil, incompleteMaterialisation(t.Target)
	}
	a := &restoreCleanup{record: r, dataRoot: cfg.Server.DataRoot}
	a.dataParent, a.dataInfo, a.dataAncestors, err = openMaterialisationParent(a.dataRoot, uid, true)
	if err != nil {
		return nil, err
	}
	a.confParent, a.confInfo, a.confAncestors, err = openMaterialisationParent(materialisationConfigRoot, materialisationRecordUID, false)
	if err != nil {
		a.close()
		return nil, err
	}
	a.data, err = openMaterialisationChild(a.dataParent, t.Target)
	if err != nil {
		a.close()
		return nil, err
	}
	a.conf, err = openMaterialisationChild(a.confParent, t.Target)
	if err != nil {
		a.close()
		return nil, err
	}
	if err := a.validate(false); err != nil {
		a.close()
		return nil, err
	}
	if err := restoreCleanupRegistration(t.Target, t.Source, a.dataRoot, t.SourcePort, t.TemporaryPort); err != nil {
		a.close()
		return nil, err
	}
	return a, nil
}

func (a *restoreCleanup) validate(removed bool) error {
	t := a.record.Attempt
	records, err := backup.LoadRecords()
	if err != nil {
		return err
	}
	current, ok := records.Restores[t.Source]
	if !ok || !reflect.DeepEqual(current, a.record) {
		return incompleteMaterialisation(t.Target)
	}
	if err := errors.Join(validateMaterialisationAncestry(a.dataAncestors), validateMaterialisationAncestry(a.confAncestors),
		validateMaterialisationParent(a.dataRoot, a.dataInfo), validateMaterialisationParent(materialisationConfigRoot, a.confInfo)); err != nil {
		return err
	}
	for _, item := range []struct {
		root *os.Root
		file *os.File
		id   backup.ArtifactIdentity
		mode os.FileMode
	}{{a.dataParent, a.data, t.DataIdentity, 0o700}, {a.confParent, a.conf, t.ConfigIdentity, 0o755}} {
		if !removed {
			if err := validateMaterialisationDir(item.root, t.Target, restoreNodeID(item.id), item.file, item.mode); err != nil {
				return err
			}
			continue
		}
		if _, err := item.root.Lstat(t.Target); !errors.Is(err, os.ErrNotExist) {
			return errors.New("restore cleanup path remains or was replaced")
		}
		info, err := item.file.Stat()
		if err != nil {
			return err
		}
		id, err := materialisationArtifactID(info)
		st, ok := info.Sys().(*syscall.Stat_t)
		if err != nil || !ok || restoreFileID(id) != item.id || st.Nlink != 0 {
			return errors.New("original restore artifact not removed")
		}
	}
	return nil
}

func (a *restoreCleanup) remove(ctx context.Context, env *Env) error {
	t := a.record.Attempt
	return restoreCleanupReservation(ctx, t.Target, t.TemporaryPort, t.ReservationIdentity, func() error {
		if err := a.validate(false); err != nil {
			return err
		}
		if err := restoreCleanupRegistration(t.Target, t.Source, a.dataRoot, t.SourcePort, t.TemporaryPort); err != nil {
			return err
		}
		if err := a.validate(false); err != nil {
			return err
		}
		if err := restoreCleanupDrop(ctx, env, t.Target); err != nil {
			return err
		}
		// pg_dropcluster normally removes both directories itself. If it
		// leaves them, only the still-identical pinned artifacts may be removed.
		if err := a.removeRemaining(); err != nil {
			return err
		}
		if err := a.validate(true); err != nil {
			return err
		}
		return errors.Join(materialisationSyncArtifactParent(a.dataParent), materialisationSyncArtifactParent(a.confParent))
	})
}

func (a *restoreCleanup) removeRemaining() error {
	t := a.record.Attempt
	records, err := backup.LoadRecords()
	if err != nil {
		return err
	}
	if current, ok := records.Restores[t.Source]; !ok || !reflect.DeepEqual(current, a.record) {
		return incompleteMaterialisation(t.Target)
	}
	if err := errors.Join(validateMaterialisationAncestry(a.dataAncestors), validateMaterialisationAncestry(a.confAncestors),
		validateMaterialisationParent(a.dataRoot, a.dataInfo), validateMaterialisationParent(materialisationConfigRoot, a.confInfo)); err != nil {
		return err
	}
	for _, item := range []struct {
		root *os.Root
		file *os.File
		id   backup.ArtifactIdentity
		mode os.FileMode
	}{{a.dataParent, a.data, t.DataIdentity, 0o700}, {a.confParent, a.conf, t.ConfigIdentity, 0o755}} {
		_, err := item.root.Lstat(t.Target)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := validateMaterialisationDir(item.root, t.Target, restoreNodeID(item.id), item.file, item.mode); err != nil {
			return err
		}
		if err := item.root.RemoveAll(t.Target); err != nil {
			return err
		}
	}
	return nil
}

func (a *restoreCleanup) retire(ctx context.Context) error {
	t := a.record.Attempt
	unlock, err := restoreCleanupAbsence(ctx, t.Target, t.TemporaryPort, false)
	if err != nil {
		return err
	}
	defer unlock()
	if err := a.validate(true); err != nil {
		return err
	}
	err = backup.UpdateRecords(func(s *backup.Records) error {
		current, ok := s.Restores[t.Source]
		if !ok || !reflect.DeepEqual(current, a.record) {
			return incompleteMaterialisation(t.Target)
		}
		delete(s.Restores, t.Source)
		return nil
	})
	if err == nil {
		releaseMaterialisationJob(t.JobID)
	}
	return err
}
