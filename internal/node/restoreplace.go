package node

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/backup"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/masterbackup"
	"github.com/mbvlabs/eru/internal/steps"
	"golang.org/x/crypto/ssh"
)

// PhaseRestore is cluster.bootstrap's placement of a master bundle on a new
// server (Phase 5, D7): before any step runs, the bundle's files go where
// they were, records.json is pruned and every database awaits recovery.
const PhaseRestore = "restore"

// RestorePlaced is the restore phase's result.
type RestorePlaced struct {
	Phase       string                `json:"phase"`
	Placed      []string              `json:"placed"`
	Pruned      []backup.PrunedRecord `json:"pruned,omitempty"`
	Awaiting    []string              `json:"awaiting_recovery"`
	SSHHostKeys []string              `json:"ssh_host_keys"`
}

// firstLoginID is where login users and groups start: a bundled file is
// owned by root or a system account, never a person's.
const firstLoginID = 1000

// placeDirs are the modes of the directories bundled files live in.
var placeDirs = map[string]os.FileMode{
	"/etc/eru": 0o755, "/etc/eru/tls": 0o755, "/etc/eru/acme": 0o700,
	"/etc/wireguard": 0o700, "/var/lib/eru": 0o700, "/etc/ssh": 0o755,
}

func bootstrapRestore(ctx context.Context, env *Env, p bootstrapParams) (Result, error) {
	cfg, err := config.ParseCluster([]byte(p.ClusterTOML))
	if err != nil {
		return Result{}, output.NewError(output.CodeConfig, "cluster_toml does not parse: "+err.Error(), output.ExitConfig, "")
	}
	if err := cfg.ValidateFor(config.ScopeHost); err != nil {
		return Result{}, config.ValidationError(err)
	}
	if _, err := bootstrapPreflight(cfg); err != nil {
		return Result{}, err
	}
	if err := placementFresh(p.InstanceID); err != nil {
		return Result{}, err
	}
	raw, err := base64.StdEncoding.DecodeString(env.Req.Secrets[steps.SecretMasterBundle])
	if err != nil || len(raw) == 0 {
		return Result{}, output.NewError(output.CodeInputRequired, "the request has no master bundle", output.ExitInput,
			"Run eru server init --restore from your machine; it sends the decrypted bundle.").WithDetails(map[string]any{"secret": steps.SecretMasterBundle})
	}
	b, err := masterbackup.ReadTar(raw)
	if err != nil {
		return Result{}, output.WrapError(output.CodeBundleCorrupt, err, output.ExitSecrets, "")
	}
	if b.Manifest.Server != cfg.Name() {
		return Result{}, output.NewError(output.CodePrecondition, "the bundle is server "+b.Manifest.Server+"'s, not "+cfg.Name()+"'s", output.ExitPrecondition, "").
			WithDetails(map[string]any{"reason": "bundle_server_mismatch"})
	}
	files, pruned, err := restoreFiles(b, cfg, p.ClusterTOML, time.Now())
	if err != nil {
		return Result{}, err
	}
	res := RestorePlaced{Phase: PhaseRestore, Pruned: pruned}
	for _, f := range files {
		if err := placeFile(f); err != nil {
			return Result{}, fmt.Errorf("place %s: %w", f.Path, err)
		}
		res.Placed = append(res.Placed, f.Path)
		if strings.HasPrefix(f.Path, "/etc/ssh/") && strings.HasSuffix(f.Path, ".pub") {
			res.SSHHostKeys = append(res.SSHHostKeys, strings.TrimSpace(string(f.Data)))
		}
	}
	if err := placeInstance(p.InstanceID, p.Epoch); err != nil {
		return Result{}, err
	}
	for _, c := range cfg.Clusters {
		res.Awaiting = append(res.Awaiting, c.Name)
	}
	// New SSH connections get the restored host keys; this one stays up.
	_ = exec.CommandContext(ctx, "systemctl", "try-reload-or-restart", "ssh.service").Run() //nolint:gosec // G204: fixed arguments.
	return Result{Data: res}, nil
}

// placementFresh refuses to place a bundle on a host that is already a node:
// placement overwrites cluster.toml, records.json and the host keys, and
// marks every database awaiting recovery. bootstrapPreflight lets a node of
// the same server through (init resumes); placement accepts it only when it
// is this rebuild's own earlier placement (the same instance id) and no
// PostgreSQL cluster exists yet.
func placementFresh(instance string) error {
	existing, err := config.LoadNode(config.NodePath)
	if config.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	placed, _ := masterbackup.ReadInstance("")
	if instance == "" || placed != instance {
		return output.NewError(output.CodePrecondition,
			"this machine is already a node of server "+existing.Server+"; a master bundle is placed on a fresh VPS only",
			output.ExitPrecondition,
			"Check --host: it must be the new VPS, never the running master.").
			WithDetails(map[string]any{"reason": "already_a_node", "server": existing.Server})
	}
	if entries, err := os.ReadDir("/etc/postgresql/18"); err == nil && len(entries) > 0 {
		return output.NewError(output.CodePrecondition,
			"this rebuild's node already has PostgreSQL clusters; the bundle was placed before",
			output.ExitPrecondition, "Continue with eru server init --restore --resume.").
			WithDetails(map[string]any{"reason": "already_placed"})
	}
	return nil
}

// restoreFiles is what placement writes: the bundle's files, with cluster.toml
// replaced by the operator's (the new public address), records.json pruned
// and awaiting recovery, protect.json's failures and lockout reset, and every
// SSH key checked.
func restoreFiles(b masterbackup.Bundle, cfg config.Cluster, clusterTOML string, now time.Time) ([]masterbackup.File, []backup.PrunedRecord, error) {
	var out []masterbackup.File
	var pruned []backup.PrunedRecord
	haveRecords := false
	for _, f := range b.Files {
		switch {
		case f.Path == masterbackup.ClusterPath:
			f.Data = []byte(clusterTOML)
		case f.Path == masterbackup.RecordsPath:
			haveRecords = true
			data, p, err := backup.PruneForRestore(f.Data, awaitingOf(cfg), now)
			if err != nil {
				return nil, nil, err
			}
			f.Data, pruned = data, p
		case f.Path == masterbackup.ProtectPath:
			data, err := resetProtect(f.Data)
			if err != nil {
				return nil, nil, err
			}
			f.Data = data
		case strings.HasPrefix(f.Path, "/etc/ssh/"):
			if err := checkHostKey(f); err != nil {
				return nil, nil, err
			}
		}
		out = append(out, f)
	}
	if !haveRecords && len(cfg.Clusters) > 0 {
		data, _, err := backup.PruneForRestore(nil, awaitingOf(cfg), now)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, masterbackup.NewFile(masterbackup.RecordsPath, "root", "root", 0o600, data))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, pruned, nil
}

func awaitingOf(cfg config.Cluster) []backup.AwaitingDatabase {
	out := make([]backup.AwaitingDatabase, 0, len(cfg.Clusters))
	for _, c := range cfg.Clusters {
		out = append(out, backup.AwaitingDatabase{Name: c.Name, Stanza: c.Stanza})
	}
	return out
}

// resetProtect clears the failure count and the lockout, keeping the
// secret and the last accepted step (a used code can't be replayed).
func resetProtect(raw []byte) ([]byte, error) {
	var st protectState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("protect.json in the bundle does not parse: %w", err)
	}
	st.Failures, st.LockedUntil = 0, time.Time{}
	out, err := json.MarshalIndent(st, "", "  ") //nolint:gosec // G117: the root-only file that holds the secret.
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// checkHostKey refuses an SSH host key that doesn't parse.
func checkHostKey(f masterbackup.File) error {
	if strings.HasSuffix(f.Path, ".pub") {
		if _, _, _, _, err := ssh.ParseAuthorizedKey(f.Data); err != nil {
			return fmt.Errorf("%s in the bundle is not a public key: %w", f.Path, err)
		}
		return nil
	}
	if _, err := ssh.ParseRawPrivateKey(f.Data); err != nil {
		return fmt.Errorf("%s in the bundle is not a private key: %w", f.Path, err)
	}
	return nil
}

// placeFile writes one file with its owner, group and mode. An owner or
// group this host doesn't have yet (ssl-cert before nginx is installed) is
// root; the step that owns the file sets it right later.
func placeFile(f masterbackup.File) error {
	dir := filepath.Dir(f.Path)
	mode, known := placeDirs[dir]
	if !known {
		mode = 0o755
	}
	if err := os.MkdirAll(dir, mode); err != nil {
		return err
	}
	owner, group := f.Owner, f.Group
	if uid, _, err := engine.ResolveIDs(owner, ""); err != nil || uid >= firstLoginID {
		owner = "root"
	}
	if _, gid, err := engine.ResolveIDs("", group); err != nil || gid >= firstLoginID {
		group = "root"
	}
	// Every bundled file is data (keys, configuration, records): never
	// executable, setuid or sticky, whatever the manifest says.
	return engine.WriteFileAtomicOwned(f.Path, f.Data, os.FileMode(f.Mode)&0o666, owner, group)
}

// placeInstance writes the instance id the operator generated and the epoch
// it wrote to the bucket, so this install's jobs pass the fence and the old
// master's don't.
func placeInstance(instance string, e *masterbackup.Epoch) error {
	if err := os.MkdirAll(masterbackup.Dir, 0o700); err != nil {
		return err
	}
	if instance != "" {
		if err := engine.WriteFileAtomic(masterbackup.InstancePath, []byte(instance+"\n"), 0o600); err != nil {
			return err
		}
	}
	if e != nil {
		return saveLocalEpoch(*e)
	}
	return nil
}
