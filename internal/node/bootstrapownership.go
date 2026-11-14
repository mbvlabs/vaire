package node

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/masterbackup"
	"github.com/mbvlabs/vaire/internal/steps"
)

var (
	bootstrapStateDir    = "/var/lib/vaire"
	bootstrapNodePath    = config.NodePath
	bootstrapClusterPath = config.ClusterPath
	bootstrapExpectedUID uint32
	bootstrapConfDir     = steps.PGConfDir
	bootstrapInstance    = currentFollowInstance
)

const bootstrapManifestName = "bootstrap-ownership.json"

var errUnownedBootstrap = errors.New("bootstrap has no fresh-node ownership")

type bootstrapOwnership struct {
	Version    int             `json:"version"`
	Server     string          `json:"server"`
	Node       string          `json:"node"`
	Operation  string          `json:"operation"`
	Instance   string          `json:"instance"`
	ConfigHash string          `json:"config_hash"`
	Targets    map[string]bool `json:"targets"` // true only after durable completion
}

func bootstrapHash(cfg config.Cluster) (string, error) {
	raw, err := cfg.Encode()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func bootstrapDir(create bool) error {
	// The state directory is private, root-owned and never chmod-repaired.
	parent, err := os.Lstat(filepath.Dir(bootstrapStateDir))
	if err != nil {
		return err
	}
	parentStat, ok := parent.Sys().(*syscall.Stat_t)
	if !ok || parentStat.Uid != bootstrapExpectedUID || !parent.IsDir() || parent.Mode().Perm()&0o022 != 0 {
		return errors.New("unsafe bootstrap state parent")
	}
	info, err := os.Lstat(bootstrapStateDir)
	if create && errors.Is(err, os.ErrNotExist) {
		if err = os.Mkdir(bootstrapStateDir, 0o700); err != nil {
			return err
		}
		info, err = os.Lstat(bootstrapStateDir)
	}
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != bootstrapExpectedUID || !info.IsDir() || info.Mode().Perm() != 0o700 || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("unsafe bootstrap state directory")
	}
	return nil
}

func bootstrapReadOwnership() (bootstrapOwnership, error) {
	var owner bootstrapOwnership
	path := filepath.Join(bootstrapStateDir, bootstrapManifestName)
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0) //nolint:gosec // fixed file within private state directory
	if err != nil {
		return owner, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return owner, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != bootstrapExpectedUID || stat.Nlink != 1 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 ||
		info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || info.Size() > 1<<20 {
		return owner, errors.New("unsafe bootstrap ownership manifest")
	}
	dec := json.NewDecoder(io.LimitReader(f, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&owner); err != nil {
		return owner, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return owner, errors.New("invalid bootstrap ownership manifest")
	}
	if owner.Version != 1 || owner.Server == "" || owner.Node == "" || owner.Operation == "" || owner.Instance == "" || owner.ConfigHash == "" || owner.Targets == nil {
		return owner, errors.New("incomplete bootstrap ownership manifest")
	}
	return owner, nil
}

func bootstrapValidateOwnership(owner bootstrapOwnership, cfg config.Cluster, node, operation string) error {
	hash, err := bootstrapHash(cfg)
	if err != nil {
		return err
	}
	if operation == "" || owner.Server != cfg.Name() || owner.Node != node || owner.Operation != operation || owner.ConfigHash != hash {
		return errors.New("bootstrap ownership does not match this setup")
	}
	info, err := os.Lstat(filepath.Join(bootstrapStateDir, "instance"))
	if err != nil {
		return errors.New("bootstrap instance is unavailable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != bootstrapExpectedUID || stat.Nlink != 1 || !info.Mode().IsRegular() ||
		info.Mode().Perm() != 0o600 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return errors.New("unsafe bootstrap instance")
	}
	instance, err := bootstrapInstance()
	if err != nil || instance != owner.Instance {
		return errors.New("bootstrap ownership instance does not match")
	}
	expected := 0
	for _, spec := range cfg.Clusters {
		if !spec.IsFollower() {
			expected++
			if _, ok := owner.Targets[spec.Name]; !ok {
				return errors.New("bootstrap target is not owned")
			}
		}
	}
	if len(owner.Targets) != expected {
		return errors.New("bootstrap target set does not match")
	}
	return nil
}

func bootstrapWriteNew(path string, raw []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600) //nolint:gosec // fixed path under validated private directory
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(raw); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	dir, err := os.Open(bootstrapStateDir) //nolint:gosec // validated fixed state directory
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func bootstrapExistingConfigMatches(cfg config.Cluster, node string) error {
	existing, err := config.LoadNode(bootstrapNodePath)
	if err == nil {
		if existing.Server != cfg.Name() || existing.NodeName != node {
			return errors.New("existing node identity does not match bootstrap ownership")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, err = os.Lstat(bootstrapClusterPath)
	if err == nil {
		existingCfg, err := config.LoadCluster(bootstrapClusterPath)
		if err != nil {
			return err
		}
		want, wantErr := bootstrapHash(cfg)
		got, gotErr := bootstrapHash(existingCfg)
		if wantErr != nil || gotErr != nil || want != got {
			return errors.New("existing configuration does not match bootstrap ownership")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// prepareBootstrapOwnership never creates a manifest on an initialized node.
// A previously created manifest is the only source of resume authority.
func prepareBootstrapOwnership(cfg config.Cluster, node, operation string) error {
	if _, err := os.Lstat(bootstrapStateDir); errors.Is(err, os.ErrNotExist) {
		for _, path := range []string{bootstrapNodePath, bootstrapClusterPath} {
			if _, err := os.Lstat(path); err == nil {
				return errUnownedBootstrap
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	} else if err != nil {
		return err
	}
	if err := bootstrapDir(true); err != nil {
		return err
	}
	owner, err := bootstrapReadOwnership()
	if err == nil {
		if err := bootstrapExistingConfigMatches(cfg, node); err != nil {
			return err
		}
		// The first host attempt may have durably created its manifest but
		// crashed before writing the instance file. Only that original,
		// unexposed manifest may finish creating its own recorded instance.
		instancePath := filepath.Join(bootstrapStateDir, "instance")
		if _, statErr := os.Lstat(instancePath); errors.Is(statErr, os.ErrNotExist) {
			hash, hashErr := bootstrapHash(cfg)
			_, nodeErr := os.Lstat(bootstrapNodePath)
			_, clusterErr := os.Lstat(bootstrapClusterPath)
			if hashErr != nil || operation == "" || owner.Server != cfg.Name() || owner.Node != node ||
				owner.Operation != operation || owner.ConfigHash != hash ||
				!errors.Is(nodeErr, os.ErrNotExist) || !errors.Is(clusterErr, os.ErrNotExist) {
				return errors.New("incomplete bootstrap ownership cannot be resumed")
			}
			for _, spec := range cfg.Clusters {
				if owner.Targets[spec.Name] {
					return errors.New("completed bootstrap ownership cannot recreate its instance")
				}
				for _, path := range []string{bootstrapConfDir(spec.Name), cfg.Server.DataDirectory(spec.Name)} {
					if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
						return errors.New("bootstrap instance is missing after a target appeared")
					}
				}
			}
			if err := bootstrapWriteNew(instancePath, []byte(owner.Instance+"\n")); err != nil {
				return err
			}
		} else if statErr != nil {
			return statErr
		}
		return bootstrapValidateOwnership(owner, cfg, node, operation)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, path := range []string{bootstrapNodePath, bootstrapClusterPath} {
		if _, err := os.Lstat(path); err == nil {
			return errUnownedBootstrap
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	paths := []string{bootstrapNodePath, bootstrapClusterPath, filepath.Join(bootstrapStateDir, "instance")}
	for _, spec := range cfg.Clusters {
		paths = append(paths, bootstrapConfDir(spec.Name), cfg.Server.DataDirectory(spec.Name))
	}
	for _, path := range paths {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				return fmt.Errorf("bootstrap ownership cannot claim existing artifact %s", path)
			}
			return err
		}
	}
	if operation == "" {
		return errors.New("bootstrap setup operation is missing")
	}
	hash, err := bootstrapHash(cfg)
	if err != nil {
		return err
	}
	instance, err := masterbackup.NewInstanceID()
	if err != nil {
		return err
	}
	owner = bootstrapOwnership{Version: 1, Server: cfg.Name(), Node: node, Operation: operation, Instance: instance, ConfigHash: hash, Targets: map[string]bool{}}
	for _, spec := range cfg.Clusters {
		if !spec.IsFollower() {
			owner.Targets[spec.Name] = false
		}
	}
	raw, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	if err := bootstrapWriteNew(filepath.Join(bootstrapStateDir, bootstrapManifestName), append(raw, '\n')); err != nil {
		return err
	}
	return bootstrapWriteNew(filepath.Join(bootstrapStateDir, "instance"), []byte(instance+"\n"))
}

func bootstrapCompleteTarget(owner *bootstrapOwnership, cfg config.Cluster, node, operation, target string) error {
	if err := bootstrapDir(false); err != nil {
		return err
	}
	current, err := bootstrapReadOwnership()
	if err != nil {
		return err
	}
	if err := bootstrapValidateOwnership(current, cfg, node, operation); err != nil {
		return err
	}
	done, ok := current.Targets[target]
	if !ok || done {
		return errors.New("bootstrap target is not an unfinished owned target")
	}
	current.Targets[target] = true
	raw, err := json.Marshal(current)
	if err != nil {
		return err
	}
	if err := engine.WriteFileAtomic(filepath.Join(bootstrapStateDir, bootstrapManifestName), append(raw, '\n'), 0o600); err != nil {
		return err
	}
	*owner = current
	return nil
}
