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
		if err := g.validateResumeTarget(p, final, effect); err != nil {
			return err
		}
		state, err := g.prepareResume()
		if err != nil {
			return err
		}
		data, conf, err := state.inspect(p)
		if err != nil {
			return err
		}
		dataHandle, confHandle, err := openResumeArtifacts(p)
		if err != nil {
			return err
		}
		defer func() { _ = dataHandle.Close() }()
		defer func() { _ = confHandle.Close() }()
		if err := state.pinned(dataHandle, confHandle, data, conf); err != nil {
			return err
		}
		return state.finish(p, final, data, conf, effect(state.evidence))
	})
}

func (g *followResumeGuard) validateResumeTarget(p backup.MaterialisationPaths, final int, effect func(backup.FollowerResumeEvidence) error) error {
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
	return b.validateLeaseIdentity()
}

type followResumeState struct {
	guard         *followResumeGuard
	before        followOwnership
	record        os.FileInfo
	legacy        bool
	evidence      backup.FollowerResumeEvidence
	uid           uint32
	dataAncestors []materialisationAncestor
	confAncestors []materialisationAncestor
}

func (g *followResumeGuard) prepareResume() (*followResumeState, error) {
	instance, err := currentFollowInstance()
	if err != nil || instance != g.env.installID {
		return nil, errors.New("follower resume install changed")
	}
	record, err := trustedFollowRecord(g.owner.Name)
	if err != nil {
		return nil, err
	}
	before, owned, err := readFollowOwnership(g.owner)
	if err != nil || !owned {
		return nil, errors.New("follower resume ownership unavailable")
	}
	if err := refuseIncompleteFollowAttempt(before); err != nil {
		return nil, err
	}
	if before.Phase != followPhaseCopied && before.Phase != followPhaseStarted {
		return nil, errors.New("follower copy not complete")
	}
	legacy, evidence, err := resumeAttemptEvidence(before, g.sourceStanza)
	if err != nil {
		return nil, err
	}
	afterRead, err := trustedFollowRecord(g.owner.Name)
	if err != nil || !os.SameFile(record, afterRead) {
		return nil, errors.New("follower resume record changed")
	}
	uid, err := materialisationPostgresUID()
	if err != nil {
		return nil, err
	}
	dataAncestors, err := materialisationAncestry(g.dataRoot, uid, true)
	if err != nil {
		return nil, err
	}
	confAncestors, err := materialisationConfigAncestry(materialisationConfigRoot, uid)
	if err != nil {
		return nil, err
	}
	return &followResumeState{guard: g, before: before, record: record, legacy: legacy, evidence: evidence,
		uid: uid, dataAncestors: dataAncestors, confAncestors: confAncestors}, nil
}

// resumeAttemptEvidence validates the retained attempt and returns whether it
// is legacy (no artifact snapshot) and the evidence the effect may use. Legacy
// copied records can authorize continuation under the live lease, but their
// snapshots never become birth or cleanup evidence.
func resumeAttemptEvidence(before followOwnership, sourceStanza string) (bool, backup.FollowerResumeEvidence, error) {
	var evidence backup.FollowerResumeEvidence
	if before.Attempt == nil {
		return true, evidence, nil
	}
	a := before.Attempt
	if a.State != followAttemptCompleted || a.SourceStanza != "" && a.SourceStanza != sourceStanza {
		return false, evidence, errors.New("follower attempt identity mismatch")
	}
	legacy := a.ArtifactStage == "" && a.DataIdentity == (materialisationFileID{}) &&
		a.ConfigIdentity == (materialisationFileID{}) && a.ReservationIdentity == (backup.ReservationIdentity{})
	if !legacy && a.ArtifactStage != materialisationRegistered {
		return false, evidence, errors.New("follower birth identity unavailable")
	}
	if !legacy && a.SourceStanza != sourceStanza {
		return false, evidence, errors.New("bound follower source stanza changed")
	}
	return legacy, backup.FollowerResumeEvidence{TemporaryPort: a.TemporaryPort, ReservationIdentity: a.ReservationIdentity}, nil
}

func (s *followResumeState) inspect(p backup.MaterialisationPaths) (os.FileInfo, os.FileInfo, error) {
	if err := errors.Join(validateMaterialisationAncestry(s.dataAncestors), validateMaterialisationAncestry(s.confAncestors)); err != nil {
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
	registered, err := followRegisteredData(s.guard.owner.Name)
	if err != nil || !followConfRegistered(s.guard.owner.Name) || registered != p.DataDir {
		return nil, nil, errors.New("follower registration path mismatch")
	}
	dataID, err1 := materialisationArtifactID(data)
	confID, err2 := materialisationArtifactID(conf)
	if err1 != nil || err2 != nil || dataID.UID != s.uid || confID.UID != s.uid ||
		data.Mode().Perm() != 0o700 || conf.Mode().Perm() != 0o755 {
		return nil, nil, errors.New("follower artifacts replaced")
	}
	if !s.legacy && (dataID != s.before.Attempt.DataIdentity || confID != s.before.Attempt.ConfigIdentity) {
		return nil, nil, errors.New("bound follower artifacts replaced")
	}
	return data, conf, nil
}

func openResumeArtifacts(p backup.MaterialisationPaths) (*os.File, *os.File, error) {
	dataHandle, err := os.Open(p.DataDir)
	if err != nil {
		return nil, nil, err
	}
	confHandle, err := os.Open(p.ConfigDir)
	if err != nil {
		_ = dataHandle.Close()
		return nil, nil, err
	}
	return dataHandle, confHandle, nil
}

func (s *followResumeState) pinned(dataHandle, confHandle *os.File, data, conf os.FileInfo) error {
	openedData, dataErr := dataHandle.Stat()
	openedConf, confErr := confHandle.Stat()
	if dataErr != nil || confErr != nil || !os.SameFile(data, openedData) || !os.SameFile(conf, openedConf) {
		return errors.New("follower artifacts changed while pinning")
	}
	return nil
}

func (s *followResumeState) finish(p backup.MaterialisationPaths, final int, data, conf os.FileInfo, effectErr error) error {
	g := s.guard
	after, afterOwned, afterErr := readFollowOwnership(g.owner)
	afterRecord, recordErr := trustedFollowRecord(g.owner.Name)
	dataAfter, confAfter, inspectErr := s.inspect(p)
	if afterErr != nil || !afterOwned || !reflect.DeepEqual(s.before, after) || recordErr != nil ||
		!os.SameFile(s.record, afterRecord) || inspectErr != nil || !os.SameFile(data, dataAfter) || !os.SameFile(conf, confAfter) {
		return errors.Join(effectErr, errors.New("follower resume ownership changed"))
	}
	b := followBirth{env: g.env, owner: g.owner, key: followAttemptKey{SourceStanza: g.sourceStanza}, sourcePort: g.sourcePort}
	if err := b.validateLeaseIdentity(); err != nil {
		return errors.Join(effectErr, err)
	}
	postSpec, ok := g.env.Cluster.ClusterByName(g.owner.Name)
	if !ok || postSpec.Port != final || postSpec.Follows != g.owner.Source {
		return errors.Join(effectErr, errors.New("follower final configuration changed"))
	}
	instance, err := currentFollowInstance()
	if err != nil || instance != g.env.installID {
		return errors.Join(effectErr, errors.New("follower resume install changed"))
	}
	return effectErr
}
