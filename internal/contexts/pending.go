package contexts

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/mbvlabs/vaire/internal/transport"
	"github.com/pelletier/go-toml/v2"
)

// Pending init files inside pending/<name>/ (§6.3).
const (
	AnswersFile  = "answers.toml"
	SecretsFile  = "secrets.json"
	ProgressFile = "progress.json"
)

// Bootstrap is the answers file's [bootstrap] table: how to reach the server
// on first contact. Credentials stay out of it; they come from flags again on
// --resume.
type Bootstrap struct {
	Host               string `toml:"host" json:"host"`
	User               string `toml:"user" json:"user"`
	Port               int    `toml:"port,omitempty" json:"port,omitempty"`
	Identity           string `toml:"identity,omitempty" json:"identity,omitempty"`
	HostKeyFingerprint string `toml:"host_key_fingerprint,omitempty" json:"host_key_fingerprint,omitempty"`
}

// PendingSecrets are generated on the operator before first contact. The
// database and service passwords are added once the answers are known (D7).
type PendingSecrets struct {
	OperatorName  string         `json:"operator_name"`
	WGPrivateKey  string         `json:"wg_private_key"`
	SSHPrivateKey string         `json:"ssh_private_key"`
	SudoPassword  string         `json:"sudo_password"`
	Database      *DatabaseCreds `json:"database,omitempty"`
	// Backup is the recovery kit when init configures backups.
	Backup *BackupCreds `json:"backup,omitempty"`
	// Metrics is the /metrics endpoint credential (Phase 4).
	Metrics *MetricsCreds `json:"metrics,omitempty"`
}

// Progress records the completed init phases and what was learned on the way.
type Progress struct {
	Local       bool              `json:"local"`
	Phases      map[string]string `json:"phases"`
	OS          string            `json:"os,omitempty"`
	Arch        string            `json:"arch,omitempty"`
	WGPublicKey string            `json:"wg_public_key,omitempty"`
	SSHHostKeys []string          `json:"ssh_host_keys,omitempty"`
	SecretsOut  string            `json:"secrets_out,omitempty"`
	PublicIPv6  string            `json:"public_ipv6,omitempty"`
	// Restore is set for server init --restore (Phase 5).
	Restore *RestoreProgress `json:"restore,omitempty"`
}

// RestoreProgress is what a rebuild has decided so far, so --resume picks
// up the same bundle, instance id and epoch.
type RestoreProgress struct {
	Bundle         string `json:"bundle,omitempty"`
	From           string `json:"from,omitempty"`
	OldAddress     string `json:"old_address,omitempty"`
	NewAddress     string `json:"new_address,omitempty"`
	InstanceID     string `json:"instance_id,omitempty"`
	Epoch          int64  `json:"epoch,omitempty"`
	EpochWrittenAt string `json:"epoch_written_at,omitempty"`
	// Timings are each phase's seconds, for the RTO.
	Timings map[string]float64 `json:"timings,omitempty"`
}

// BundleFile holds a rebuild's master bundle, still encrypted.
const BundleFile = "bundle.tar.age"

// SavePendingBundle keeps the encrypted bundle for --resume.
func SavePendingBundle(name string, cipher []byte) error {
	return writePending(name, BundleFile, cipher)
}

// LoadPendingBundle reads it back.
func LoadPendingBundle(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(PendingDir(name), BundleFile))
}

// Done reports whether phase completed.
func (p *Progress) Done(phase string) bool {
	_, ok := p.Phases[phase]
	return ok
}

// Mark records phase as completed now.
func (p *Progress) Mark(phase string) {
	if p.Phases == nil {
		p.Phases = map[string]string{}
	}
	p.Phases[phase] = time.Now().UTC().Format(time.RFC3339)
}

// PendingDir is pending/<name>.
func PendingDir(name string) string { return filepath.Join(Root(), "pending", name) }

// PendingExists reports whether an unfinished init exists for name.
func PendingExists(name string) bool {
	_, err := os.Stat(filepath.Join(PendingDir(name), ProgressFile))
	return err == nil
}

// PendingNames lists unfinished inits.
func PendingNames() []string {
	entries, err := os.ReadDir(filepath.Join(Root(), "pending"))
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && PendingExists(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

func writePending(name, file string, data []byte) error {
	if err := ensureRoot(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(Root(), "pending"), 0o700); err != nil {
		return err
	}
	return transport.WriteSecretFile(filepath.Join(PendingDir(name), file), data)
}

// SaveAnswers writes answers.toml: the cluster.toml bytes plus [bootstrap].
func SaveAnswers(name string, clusterTOML []byte, b Bootstrap) error {
	raw, err := toml.Marshal(struct {
		Bootstrap Bootstrap `toml:"bootstrap"`
	}{b})
	if err != nil {
		return err
	}
	return writePending(name, AnswersFile, append(append(append([]byte(nil), clusterTOML...), '\n'), raw...))
}

// LoadAnswers returns answers.toml and its [bootstrap] table.
func LoadAnswers(name string) ([]byte, Bootstrap, error) {
	raw, err := os.ReadFile(filepath.Join(PendingDir(name), AnswersFile))
	if err != nil {
		return nil, Bootstrap{}, err
	}
	var wrap struct {
		Bootstrap Bootstrap `toml:"bootstrap"`
	}
	if err := toml.NewDecoder(bytes.NewReader(raw)).Decode(&wrap); err != nil {
		return nil, Bootstrap{}, err
	}
	return raw, wrap.Bootstrap, nil
}

// SaveSecrets writes secrets.json (0600).
func SaveSecrets(name string, s PendingSecrets) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writePending(name, SecretsFile, append(raw, '\n'))
}

// LoadSecrets reads secrets.json.
func LoadSecrets(name string) (PendingSecrets, error) {
	var s PendingSecrets
	raw, err := os.ReadFile(filepath.Join(PendingDir(name), SecretsFile))
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(raw, &s)
}

// SaveProgress writes progress.json.
func SaveProgress(name string, p *Progress) error {
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return writePending(name, ProgressFile, append(raw, '\n'))
}

// LoadProgress reads progress.json. A missing file is a fresh progress.
func LoadProgress(name string) (*Progress, error) {
	p := &Progress{Phases: map[string]string{}}
	raw, err := os.ReadFile(filepath.Join(PendingDir(name), ProgressFile))
	if err != nil {
		if os.IsNotExist(err) {
			return p, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, p); err != nil {
		return nil, err
	}
	if p.Phases == nil {
		p.Phases = map[string]string{}
	}
	return p, nil
}

// PendingKnownHosts is pending/<name>/known_hosts.
func PendingKnownHosts(name string) string {
	return filepath.Join(PendingDir(name), KnownHostsFile)
}

// RemovePending deletes pending/<name>.
func RemovePending(name string) error {
	return os.RemoveAll(PendingDir(name))
}
