package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/masterbackup"
)

// A db recover of a follower materialises the copy and adopts it in several
// separately written files. Its ownership of the deterministic data and
// configuration paths is recorded before the first local mutation, so a job
// that dies during the copy or the adoption leaves work the public recover
// command can continue instead of a cluster it refuses as foreign.
//
// The record is node-local, beside records.json, and is never part of the
// master bundle: a rebuilt host has no such copy, and its new instance id makes
// a surviving record stale. Ownership is the matching record plus the exact
// paths derived from cluster.toml, never a path read from the record.
const (
	followPhaseCopying = "copying"
	followPhaseCopied  = "copied"
)

// followOwnership is follow-<database>.json.
type followOwnership struct {
	Database string `json:"database"`
	Source   string `json:"source"`
	Instance string `json:"instance"`
	Stanza   string `json:"stanza"`
	Since    string `json:"since"`
	Phase    string `json:"phase"`
}

// followHostInstance is this install's instance id. It is a variable so a test
// can present the node as rebuilt without writing the host's real identity.
var followHostInstance = func() (string, error) { return masterbackup.ReadInstance("") }

// followRegisteredData is cluster name's registered data directory.
var followRegisteredData = backup.ClusterDataDirectory

// followConfRegistered reports whether cluster name has a postgresql-common
// registration file.
var followConfRegistered = backup.ClusterRegistered

// followConfDirectory reports whether cluster name has a postgresql-common
// configuration directory, even a partial registration without
// postgresql.conf. It is tracked apart from followConfRegistered because the
// materialised constructor refuses the directory itself.
var followConfDirectory = backup.ClusterConfigDirectory

// followerArtefacts are the deterministic paths a follower copy uses: its
// postgresql-common configuration directory (registered or partial), its
// registration and its data directory. Match is true only when the
// registration points at that database's configured data directory.
type followerArtefacts struct {
	ConfDir bool
	Conf    bool
	Data    bool
	Match   bool
}

func followerArtefactsOf(cfg config.Cluster, name string) (followerArtefacts, error) {
	data := cfg.Server.DataDirectory(name)
	_, err := os.Stat(data)
	dataExists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return followerArtefacts{}, err
	}
	confDir, err := followConfDirectory(name)
	if err != nil {
		return followerArtefacts{}, err
	}
	conf := followConfRegistered(name)
	registered, err := followRegisteredData(name)
	if err != nil {
		return followerArtefacts{}, err
	}
	return followerArtefacts{
		ConfDir: confDir,
		Conf:    conf,
		Data:    dataExists,
		Match:   conf && registered != "" && filepath.Clean(registered) == filepath.Clean(data),
	}, nil
}

// followOwnershipPath is the record's fixed path. name is a validated database
// name from cluster.toml; a name that could escape the directory is refused,
// not joined.
func followOwnershipPath(name string) (string, error) {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("database name %q cannot own an interrupted follow", name)
	}
	return filepath.Join(filepath.Dir(backup.RecordsPath), "follow-"+name+".json"), nil
}

// currentFollowInstance fails closed when the node has no instance identity: an
// empty id must never match another empty one and establish ownership.
func currentFollowInstance() (string, error) {
	id, err := followHostInstance()
	if err != nil {
		return "", err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return "", errors.New("the node instance identity is missing, so an interrupted follow cannot prove its copy is its own")
	}
	return id, nil
}

// writeFollowOwnership records phase before or after a local mutation. It is
// root-owned 0600 and atomic, so the next process sees either the old phase or
// the new one.
func writeFollowOwnership(name, source string, awaiting backup.Awaiting, phase string) error {
	if phase != followPhaseCopying && phase != followPhaseCopied {
		return fmt.Errorf("follow ownership of %s has invalid phase %q", name, phase)
	}
	path, err := followOwnershipPath(name)
	if err != nil {
		return err
	}
	if source == "" {
		return fmt.Errorf("follow ownership of %s names no source", name)
	}
	if awaiting.Since == "" {
		return fmt.Errorf("follow ownership of %s has no awaiting recovery record", name)
	}
	instance, err := currentFollowInstance()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(followOwnership{Database: name, Source: source, Instance: instance,
		Stanza: awaiting.Stanza, Since: awaiting.Since, Phase: phase})
	if err != nil {
		return err
	}
	return engine.WriteFileAtomic(path, append(raw, '\n'), 0o600)
}

// readFollowOwnership returns the record of name and source. A missing record
// is not owned; an unreadable or malformed record is an error, never ownership
// and never its absence.
func readFollowOwnership(name, source string, awaiting backup.Awaiting) (followOwnership, bool, error) {
	path, err := followOwnershipPath(name)
	if err != nil {
		return followOwnership{}, false, err
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: a validated database name under the node state directory.
	if errors.Is(err, os.ErrNotExist) {
		return followOwnership{}, false, nil
	}
	if err != nil {
		return followOwnership{}, false, fmt.Errorf("read the interrupted follow of %s: %w", name, err)
	}
	var own followOwnership
	if err := json.Unmarshal(raw, &own); err != nil {
		return followOwnership{}, false, fmt.Errorf("the interrupted follow record of %s is corrupt: %w", name, err)
	}
	instance, err := currentFollowInstance()
	if err != nil {
		return followOwnership{}, false, err
	}
	if own.Database != name || own.Source != source || own.Instance != instance ||
		own.Stanza != awaiting.Stanza || own.Since != awaiting.Since ||
		(own.Phase != followPhaseCopying && own.Phase != followPhaseCopied) {
		return own, false, nil
	}
	return own, true, nil
}

// clearFollowOwnership removes the record after the operation no longer owns
// the paths. A missing record is already clear.
func clearFollowOwnership(name string) error {
	path, err := followOwnershipPath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove the interrupted follow record of %s: %w", name, err)
	}
	return nil
}
