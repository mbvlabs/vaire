package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"

	"github.com/mbvlabs/vaire/internal/backup"
)

type followResumeGuard struct {
	env                   *jobEnv
	owner                 followOwner
	dataRoot              string
	sourcePort, finalPort int
	sourceStanza          string
}

func (g *followResumeGuard) GuardFollowerResume(_ context.Context, p backup.MaterialisationPaths, final int, effect func(backup.FollowerResumeEvidence) error) error {
	return withLockedJobTarget(g.env, g.owner.Name, func() error {
		if effect == nil || final != g.finalPort || final < 1 || final > 65535 ||
			p.Name != g.owner.Name || p.Source != g.owner.Source ||
			p.DataDir != filepath.Join(g.dataRoot, g.owner.Name) ||
			p.ConfigDir != filepath.Join(materialisationConfigRoot, g.owner.Name) ||
			p.SourcePort != g.sourcePort || p.Port != 0 {
			return errors.New("follower resume target mismatch")
		}
		spec, ok := g.env.Cluster.ClusterByName(g.owner.Name)
		if !ok || spec.Port != final || spec.Follows != g.owner.Source {
			return errors.New("follower final configuration changed")
		}
		b := followBirth{env: g.env, owner: g.owner, key: followAttemptKey{SourceStanza: g.sourceStanza}, sourcePort: g.sourcePort}
		if err := b.validateLeaseIdentity(); err != nil {
			return err
		}
		instance, err := currentFollowInstance()
		if err != nil || instance != g.env.installID {
			return errors.New("follower resume install changed")
		}
		record, err := trustedFollowRecord(g.owner.Name)
		if err != nil {
			return err
		}
		before, owned, err := readFollowOwnership(g.owner)
		if err != nil || !owned {
			return errors.New("follower resume ownership unavailable")
		}
		if err := refuseIncompleteFollowAttempt(before); err != nil {
			return err
		}
		if before.Phase != followPhaseCopied && before.Phase != followPhaseStarted {
			return errors.New("follower copy not complete")
		}
		// Legacy copied records can authorize continuation under the live
		// lease, but their snapshots never become birth or cleanup evidence.
		legacy := before.Attempt == nil
		var evidence backup.FollowerResumeEvidence
		if before.Attempt != nil {
			a := before.Attempt
			if a.State != followAttemptCompleted || a.SourceStanza != "" && a.SourceStanza != g.sourceStanza {
				return errors.New("follower attempt identity mismatch")
			}
			legacy = a.ArtifactStage == "" && a.DataIdentity == (materialisationFileID{}) &&
				a.ConfigIdentity == (materialisationFileID{}) && a.ReservationIdentity == (backup.ReservationIdentity{})
			if !legacy && a.ArtifactStage != materialisationRegistered {
				return errors.New("follower birth identity unavailable")
			}
			if !legacy && a.SourceStanza != g.sourceStanza {
				return errors.New("bound follower source stanza changed")
			}
			evidence = backup.FollowerResumeEvidence{TemporaryPort: a.TemporaryPort, ReservationIdentity: a.ReservationIdentity}
		}
		afterRead, err := trustedFollowRecord(g.owner.Name)
		if err != nil || !os.SameFile(record, afterRead) {
			return errors.New("follower resume record changed")
		}
		uid, err := materialisationPostgresUID()
		if err != nil {
			return err
		}
		dataAncestors, err := materialisationAncestry(g.dataRoot, uid, true)
		if err != nil {
			return err
		}
		confAncestors, err := materialisationAncestry(materialisationConfigRoot, materialisationRecordUID, false)
		if err != nil {
			return err
		}
		inspect := func() (os.FileInfo, os.FileInfo, error) {
			if err := errors.Join(validateMaterialisationAncestry(dataAncestors), validateMaterialisationAncestry(confAncestors)); err != nil {
				return nil, nil, err
			}
			data, err := os.Lstat(p.DataDir)
			if err != nil || !data.IsDir() || data.Mode()&os.ModeSymlink != 0 {
				return nil, nil, errors.New("follower data unavailable")
			}
			conf, err := os.Lstat(p.ConfigDir)
			if err != nil || !conf.IsDir() || conf.Mode()&os.ModeSymlink != 0 {
				return nil, nil, errors.New("follower registration unavailable")
			}
			registered, err := followRegisteredData(g.owner.Name)
			if err != nil || !followConfRegistered(g.owner.Name) || registered != p.DataDir {
				return nil, nil, errors.New("follower registration path mismatch")
			}
			dataID, err1 := materialisationArtifactID(data)
			confID, err2 := materialisationArtifactID(conf)
			if err1 != nil || err2 != nil || dataID.UID != uid || confID.UID != uid ||
				data.Mode().Perm() != 0o700 || conf.Mode().Perm() != 0o755 {
				return nil, nil, errors.New("follower artifacts replaced")
			}
			if !legacy && (dataID != before.Attempt.DataIdentity || confID != before.Attempt.ConfigIdentity) {
				return nil, nil, errors.New("bound follower artifacts replaced")
			}
			return data, conf, nil
		}
		data, conf, err := inspect()
		if err != nil {
			return err
		}
		dataHandle, err := os.Open(p.DataDir) //nolint:gosec // deterministic path checked above
		if err != nil {
			return err
		}
		defer func() { _ = dataHandle.Close() }()
		confHandle, err := os.Open(p.ConfigDir) //nolint:gosec // deterministic path checked above
		if err != nil {
			return err
		}
		defer func() { _ = confHandle.Close() }()
		openedData, dataErr := dataHandle.Stat()
		openedConf, confErr := confHandle.Stat()
		if dataErr != nil || confErr != nil || !os.SameFile(data, openedData) || !os.SameFile(conf, openedConf) {
			return errors.New("follower artifacts changed while pinning")
		}
		effectErr := effect(evidence)
		after, afterOwned, afterErr := readFollowOwnership(g.owner)
		afterRecord, recordErr := trustedFollowRecord(g.owner.Name)
		dataAfter, confAfter, inspectErr := inspect()
		if afterErr != nil || !afterOwned || !reflect.DeepEqual(before, after) || recordErr != nil ||
			!os.SameFile(record, afterRecord) || inspectErr != nil || !os.SameFile(data, dataAfter) || !os.SameFile(conf, confAfter) {
			return errors.Join(effectErr, errors.New("follower resume ownership changed"))
		}
		if err := b.validateLeaseIdentity(); err != nil {
			return errors.Join(effectErr, err)
		}
		postSpec, ok := g.env.Cluster.ClusterByName(g.owner.Name)
		if !ok || postSpec.Port != final || postSpec.Follows != g.owner.Source {
			return errors.Join(effectErr, errors.New("follower final configuration changed"))
		}
		instance, err = currentFollowInstance()
		if err != nil || instance != g.env.installID {
			return errors.Join(effectErr, errors.New("follower resume install changed"))
		}
		return effectErr
	})
}
