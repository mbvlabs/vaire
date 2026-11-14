package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"

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
		if o.ConfigIdentity.Inode != 0 {
			_, err := a.configParent.Lstat(filepath.Join(o.Target, "postgresql.conf"))
			if err == nil {
				// Registration validation uses the target path, name and port;
				// the former source port is not part of that registration.
				if err := restoreCleanupRegistration(o.Target, o.Source, a.dataRoot, 0, o.TemporaryPort); err != nil {
					return err
				}
				if err := a.validate(); err != nil {
					return err
				}
				if err := restoreCleanupDrop(ctx, env, o.Target); err != nil {
					return err
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		for _, item := range []struct {
			root *os.Root
			file *os.File
			id   materialisationFileID
			mode os.FileMode
		}{{a.dataParent, a.data, o.DataIdentity, 0o700}, {a.configParent, a.conf, o.ConfigIdentity, 0o755}} {
			if err := a.validateRecord(); err != nil {
				return err
			}
			if err := errors.Join(validateMaterialisationAncestry(a.dataAncestors), validateMaterialisationAncestry(a.configAncestors),
				validateMaterialisationParent(a.dataRoot, a.dataParentInfo), validateMaterialisationParent(materialisationConfigRoot, a.configParentInfo)); err != nil {
				return err
			}
			if _, err := item.root.Lstat(o.Target); errors.Is(err, os.ErrNotExist) {
				continue
			} else if err != nil {
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
		if err := a.validateCleanupCompleted(); err != nil {
			return err
		}
		return errors.Join(materialisationSyncArtifactParent(a.dataParent), materialisationSyncArtifactParent(a.configParent))
	})
}
