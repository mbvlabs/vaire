package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
)

// openFailedForkCleanup accepts only a typed failed fork with a recorded
// allocator birth. Older markers without that proof remain blocked.
func openFailedForkCleanup(cfg config.Cluster, name string) (*materialisationArtifacts, error) {
	uid, err := materialisationPostgresUID()
	if err != nil {
		return nil, err
	}

	a, err := openMaterialisationArtifacts(name, cfg.Server.DataRoot, uid)
	if err != nil {
		return nil, err
	}

	o := a.owner

	target, targetOK := cfg.ClusterByName(name)
	if o.Kind != materialisationFork || o.Phase != materialisationFailed ||
		(o.Reason != "recovery_requirements_unmet" && o.Reason != "recovery_parameter_mismatch") ||
		!targetOK || target.Port != o.IntendedPort ||
		o.ReservationIdentity.Inode == 0 || o.ReservationIdentity.Digest == "" {
		a.close()
		return nil, materialisationIncomplete(name, &o)
	}

	return a, nil
}

func removeFailedFork(ctx context.Context, env *Env, a *materialisationArtifacts) error {
	o := a.owner

	return restoreCleanupReservation(ctx, o.Target, o.TemporaryPort, o.ReservationIdentity, func() error {
		if err := a.validate(); err != nil {
			return err
		}

		if err := dropFailedForkRegistration(ctx, env, a); err != nil {
			return err
		}

		if err := removeFailedForkArtifacts(a); err != nil {
			return err
		}

		if err := a.validateCleanupCompleted(); err != nil {
			return err
		}

		return errors.Join(materialisationSyncArtifactParent(a.dataParent), materialisationSyncArtifactParent(a.configParent))
	})
}

func dropFailedForkRegistration(ctx context.Context, env *Env, a *materialisationArtifacts) error {
	o := a.owner
	if o.ConfigIdentity.Inode == 0 {
		return nil
	}

	_, err := a.configParent.Lstat(filepath.Join(o.Target, "postgresql.conf"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return err
	}
	// Registration validation uses the target path, name and port; the former
	// source port is not part of that registration.
	if err := restoreCleanupRegistration(o.Target, o.Source, a.dataRoot, 0, o.TemporaryPort); err != nil {
		return err
	}

	if err := a.validate(); err != nil {
		return err
	}

	return restoreCleanupDrop(ctx, env, o.Target)
}

type forkCleanupArtifact struct {
	root *os.Root
	file *os.File
	id   materialisationFileID
	mode os.FileMode
}

// preparedForkFenceForCleanup captures the exact prepared fork fence before
// any configuration is removed. A fence that is not this operation's prepared
// fork is conflicting evidence, not something cleanup may retire.
func preparedForkFenceForCleanup(cfg config.Cluster, target, opID string) (backup.ActivationFence, bool, error) {
	recs, err := backup.LoadRecords()
	if err != nil {
		return backup.ActivationFence{}, false, err
	}

	f, ok := recs.ActivationFenceFor(target)
	if !ok {
		return backup.ActivationFence{}, false, nil
	}

	if f.Kind != "fork" || f.Phase != fencePhasePrepared || f.OperationID != opID {
		return backup.ActivationFence{}, false, materialisationIncomplete(target, nil)
	}

	verified, spec, err := verifyActivationFence(cfg, target, opID, "fork", fencePhasePrepared)
	if err != nil {
		return backup.ActivationFence{}, false, err
	}

	if spec.Follows != "" || spec.FollowGeneration != "" {
		return backup.ActivationFence{}, false, materialisationIncomplete(target, nil)
	}

	if err := CheckNotProtected(cfg, target, "db fork cleanup"); err != nil {
		return backup.ActivationFence{}, false, err
	}

	if err := refuseSourceDrop(cfg, target); err != nil {
		return backup.ActivationFence{}, false, err
	}

	return verified, true, nil
}

// retireAbandonedForkFence removes exactly the captured fence in one records
// compare-and-set. check, when present, re-proves the surrounding absence and
// conflicts under the same lock.
func retireAbandonedForkFence(want backup.ActivationFence, check func(*backup.Records) error) error {
	return forkFenceUpdateRecords(func(s *backup.Records) error {
		current, ok := s.ActivationFenceFor(want.Database)
		if !ok || !reflect.DeepEqual(current, want) {
			return materialisationIncomplete(want.Database, nil)
		}

		if check != nil {
			if err := check(s); err != nil {
				return err
			}
		}

		delete(s.Activation, want.Database)

		return nil
	})
}

// preparedForkConfig is the exact target configuration and server data root a
// prepared fork was created with. Cleanup may remove only this still-identical
// entry, never a replacement that reused the name.
type preparedForkConfig struct {
	spec     config.ClusterSpec
	dataRoot string
}

func capturePreparedForkConfig(cfg config.Cluster, target string, o materialisationOwnership, dataRoot string) (preparedForkConfig, error) {
	spec, ok := cfg.ClusterByName(target)
	if !ok || dataRoot == "" || cfg.Server.DataRoot != dataRoot ||
		spec.Name != target || spec.Port != o.IntendedPort ||
		spec.Follows != "" || spec.FollowGeneration != "" ||
		o.DataPath != filepath.Join(dataRoot, target) ||
		o.RegisteredPath != filepath.Join(materialisationConfigRoot, target) {
		return preparedForkConfig{}, materialisationIncomplete(target, &o)
	}

	return preparedForkConfig{spec: spec, dataRoot: dataRoot}, nil
}

func matchPreparedForkConfig(fresh config.Cluster, expected preparedForkConfig) error {
	spec, ok := fresh.ClusterByName(expected.spec.Name)
	if !ok || !reflect.DeepEqual(spec, expected.spec) || fresh.Server.DataRoot != expected.dataRoot {
		return materialisationIncomplete(expected.spec.Name, nil)
	}

	return nil
}

func freshPreparedForkConfig(expected preparedForkConfig) (config.Cluster, error) {
	fresh, _, err := followerState().Load()
	if err != nil {
		return config.Cluster{}, err
	}

	if err := matchPreparedForkConfig(fresh, expected); err != nil {
		return config.Cluster{}, err
	}

	return fresh, nil
}

// removePreparedForkConfig removes only the exact still-identical target entry
// from a freshly loaded cluster.toml. Any changed stanza, setting, port, data
// root or follower ownership retains the evidence.
func removePreparedForkConfig(expected preparedForkConfig) error {
	store := followerState()

	fresh, _, err := store.Load()
	if err != nil {
		return err
	}

	if err := matchPreparedForkConfig(fresh, expected); err != nil {
		return err
	}

	if err := CheckNotProtected(fresh, expected.spec.Name, "db fork cleanup"); err != nil {
		return err
	}

	if err := refuseSourceDrop(fresh, expected.spec.Name); err != nil {
		return err
	}

	_, err = saveDroppedConfig(store, fresh, expected.spec.Name)

	return err
}

// cleanupPreparedForkMetadata compensates a fork that failed before a
// materialised cluster existed. It runs under the live runner lease and removes
// only metadata: the exact prepared materialisation record, the target's
// cluster.toml entry and the still-prepared activation fence, and only after
// both deterministic target names and the allocator reservation are proven
// absent. A foreign path, reservation, record or changed configuration retains
// every piece of evidence.
func cleanupPreparedForkMetadata(ctx context.Context, a *forkMaterialisationAuthority, target, opID string) error {
	if a == nil || a.artifacts == nil || a.env == nil {
		return errors.New("fork authority unavailable")
	}

	return a.run(ctx, func() error {
		return cleanupPreparedForkMetadataOwned(ctx, a, target, opID)
	})
}

func cleanupPreparedForkMetadataOwned(ctx context.Context, a *forkMaterialisationAuthority, target, opID string) error {
	o := a.artifacts.owner
	if o.Kind != materialisationFork || o.Phase != materialisationPrepared || o.Target != target ||
		o.ArtifactStage != "" || o.TemporaryPort != 0 ||
		o.DataIdentity != (materialisationFileID{}) || o.ConfigIdentity != (materialisationFileID{}) ||
		o.ReservationIdentity != (backup.ReservationIdentity{}) {
		return materialisationIncomplete(target, &o)
	}

	targetCfg, err := capturePreparedForkConfig(a.env.Cluster, target, o, a.artifacts.dataRoot)
	if err != nil {
		return err
	}

	fresh, err := freshPreparedForkConfig(targetCfg)
	if err != nil {
		return err
	}

	expected, hasFence, err := preparedForkFenceForCleanup(fresh, target, opID)
	if err != nil {
		return err
	}

	if err := a.artifacts.validate(); err != nil {
		return err
	}

	if err := a.artifacts.validateCleanupCompleted(); err != nil {
		return err
	}

	unlock, err := forkAbsenceReservation(ctx, target)
	if err != nil || unlock == nil {
		return materialisationIncomplete(target, &o)
	}
	defer unlock()

	if err := removePreparedForkConfig(targetCfg); err != nil {
		return err
	}

	if err := a.artifacts.validate(); err != nil {
		return err
	}

	if err := a.artifacts.validateCleanupCompleted(); err != nil {
		return err
	}

	if err := a.artifacts.removeMaterialisationRecord(); err != nil {
		return err
	}

	if !hasFence {
		return nil
	}

	return retireAbandonedForkFence(expected, nil)
}

func removeFailedForkArtifacts(a *materialisationArtifacts) error {
	o := a.owner
	for _, item := range []forkCleanupArtifact{
		{a.dataParent, a.data, o.DataIdentity, 0o700},
		{a.configParent, a.conf, o.ConfigIdentity, 0o755},
	} {
		if err := a.validateRecord(); err != nil {
			return err
		}

		if err := errors.Join(validateMaterialisationAncestry(a.dataAncestors), validateMaterialisationAncestry(a.configAncestors),
			validateMaterialisationParent(a.dataRoot, a.dataParentInfo), validateMaterialisationParent(materialisationConfigRoot, a.configParentInfo)); err != nil {
			return err
		}

		_, err := item.root.Lstat(o.Target)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err != nil {
			return err
		}

		if item.id.Inode == 0 {
			return materialisationIncomplete(o.Target, &o)
		}

		if err := validateMaterialisationDir(item.root, o.Target, item.id, item.file, item.mode); err != nil {
			return err
		}

		if err := item.root.RemoveAll(o.Target); err != nil {
			return err
		}
	}

	return nil
}
