package masterbackup

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"
	"time"
)

// EpochSchema is epoch.json's schema.
const EpochSchema = "vaire-master-epoch/v1"

// Epoch is <prefix>/master/epoch.json, and the node's copy of it: the one
// instance allowed to write to the bucket.
type Epoch struct {
	Schema     string `json:"schema"`
	Server     string `json:"server"`
	Epoch      int64  `json:"epoch"`
	InstanceID string `json:"instance_id"`
	WrittenAt  string `json:"written_at"`
	WrittenBy  string `json:"written_by,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// Epoch reasons.
const (
	ReasonFirst   = "first"
	ReasonRestore = "restore"
)

// NewEpoch builds an epoch record written now.
func NewEpoch(server string, epoch int64, instance, by, reason string) Epoch {
	return Epoch{Schema: EpochSchema, Server: server, Epoch: epoch, InstanceID: instance,
		WrittenAt: time.Now().UTC().Format(time.RFC3339), WrittenBy: by, Reason: reason}
}

// WrittenTime parses WrittenAt.
func (e Epoch) WrittenTime() time.Time {
	t, _ := time.Parse(time.RFC3339, e.WrittenAt)
	return t
}

// FenceState is what a node concludes from the bucket's epoch.
type FenceState string

// Fence states. Unknown never fences: a bucket outage mustn't stop a
// healthy master.
const (
	FenceOK      FenceState = "ok"
	FenceFenced  FenceState = "fenced"
	FenceUnknown FenceState = "unknown"
	// FenceAbsent means the bucket has no epoch.json yet.
	FenceAbsent FenceState = "absent"
)

// Local is what a node knows about itself for the fence decision: its
// server name, instance id and the epoch it last confirmed (0 when none).
type Local struct {
	Server   string
	Instance string
	Epoch    int64
}

// Decide compares the bucket's epoch (nil when the object is absent, readErr
// when it couldn't be read) with this node. It fences only on positive
// proof: a well-formed epoch.json of this server that names another
// instance at an epoch newer than the one this node last confirmed. A read
// error, a node without an instance id, an object that isn't a valid epoch,
// one of another server (a misconfigured prefix, not a takeover) or one not
// newer than ours (a bucket rolled back) is unknown, never fenced: a
// healthy master must not stop on anything less.
func Decide(bucket *Epoch, readErr error, local Local) FenceState {
	switch {
	case readErr != nil || local.Instance == "":
		// No local instance id yet (a host before master-backup-v1): never
		// fence on missing local state.
		return FenceUnknown
	case bucket == nil:
		return FenceAbsent
	case bucket.InstanceID == local.Instance:
		return FenceOK
	case bucket.Schema != EpochSchema || bucket.InstanceID == "" || bucket.Epoch < 1:
		return FenceUnknown
	case local.Server != "" && bucket.Server != local.Server:
		return FenceUnknown
	case bucket.Epoch <= local.Epoch:
		return FenceUnknown
	default:
		return FenceFenced
	}
}

// NewInstanceID returns a random 128-bit id, hex.
func NewInstanceID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ReadInstance reads InstancePath under root. A missing file is "".
func ReadInstance(root string) (string, error) {
	raw, err := os.ReadFile(root + InstancePath) //nolint:gosec // G304: fixed node path.
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return strings.TrimSpace(string(raw)), err
}

// ReadLocalEpoch reads EpochPath under root. A missing file is a zero Epoch.
func ReadLocalEpoch(root string) (Epoch, error) {
	raw, err := os.ReadFile(root + EpochPath) //nolint:gosec // G304: fixed node path.
	if errors.Is(err, fs.ErrNotExist) {
		return Epoch{}, nil
	}
	if err != nil {
		return Epoch{}, err
	}
	var e Epoch
	return e, json.Unmarshal(raw, &e)
}

// Fenced is FencedPath's content.
type Fenced struct {
	Epoch      int64  `json:"epoch"`
	InstanceID string `json:"instance_id"`
	SeenAt     string `json:"seen_at"`
}

// ReadFenced reads FencedPath under root; ok is false when the node isn't
// fenced.
func ReadFenced(root string) (Fenced, bool) {
	raw, err := os.ReadFile(root + FencedPath) //nolint:gosec // G304: fixed node path.
	if err != nil {
		return Fenced{}, false
	}
	var f Fenced
	_ = json.Unmarshal(raw, &f)
	return f, true
}

// FencedFile is FencedPath under the test root.
func FencedFile() string { return testRoot + FencedPath }

// IsFenced reports whether the node is fenced: a stat, nothing else, so the
// archive wrapper can call it per WAL segment.
func IsFenced() bool {
	_, err := os.Stat(testRoot + FencedPath)
	return err == nil
}
