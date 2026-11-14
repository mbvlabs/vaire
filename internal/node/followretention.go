package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

// This is failure metadata, not proof of ownership of filesystem inodes.
// Callers must hold the job's existing locks; no attempt here authorizes
// deletion, adoption, or another job to continue its materialisation.
const (
	followAttemptPending   = "pending"
	followAttemptRetained  = "retained"
	followAttemptCompleted = "completed"
)

type followRetentionAttempt struct {
	Job                 string                     `json:"job"`
	Operation           string                     `json:"operation"`
	SourceStanza        string                     `json:"source_stanza,omitempty"`
	TemporaryPort       int                        `json:"temporary_port"`
	State               string                     `json:"state"`
	Reason              string                     `json:"reason,omitempty"`
	ArtifactStage       string                     `json:"artifact_stage,omitempty"`
	DataIdentity        materialisationFileID      `json:"data_identity,omitempty"`
	ConfigIdentity      materialisationFileID      `json:"config_identity,omitempty"`
	ReservationIdentity backup.ReservationIdentity `json:"reservation_identity,omitempty"`
}

// followAttemptKey is supplied from the active job, the source spec and the
// constructor's actual reserved port; none of it is a path from the record.
type followAttemptKey struct {
	Job, Operation, SourceStanza string
	TemporaryPort                int
}

var followAttemptStanza = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}-[0-9a-f]{6}$`)
var followReservationDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
var followRetentionWrite = engine.WriteFileAtomic

func validFollowAttemptKey(k followAttemptKey) bool {
	return validJobID(k.Job) && validJobID(k.Operation) &&
		(k.SourceStanza == "" || followAttemptStanza.MatchString(k.SourceStanza)) &&
		k.TemporaryPort >= config.TemporaryPortMin && k.TemporaryPort <= config.TemporaryPortMax
}

func validateFollowRetentionAttempt(o followOwnership) error {
	a := o.Attempt
	if a == nil {
		return errors.New("missing or invalid follower attempt identity")
	}
	key := followAttemptKey{a.Job, a.Operation, a.SourceStanza, a.TemporaryPort}
	if a.State == followAttemptCompleted && key.TemporaryPort == 0 {
		// Some historical records lack a port; birth identities still must
		// be verified before any adoption and the mapping must be retained.
		key.TemporaryPort = config.TemporaryPortMin
	}
	if !validFollowAttemptKey(key) {
		return errors.New("missing or invalid follower attempt identity")
	}
	if (a.ReservationIdentity.Inode == 0 && a.ReservationIdentity != (backup.ReservationIdentity{})) ||
		(a.ReservationIdentity.Inode != 0 && a.ReservationIdentity.UID != materialisationRecordUID) ||
		(a.ReservationIdentity.Digest != "" && !followReservationDigest.MatchString(a.ReservationIdentity.Digest)) {
		return errors.New("invalid follower reservation identity")
	}
	switch a.ArtifactStage {
	case "":
		if a.DataIdentity != (materialisationFileID{}) || a.ConfigIdentity != (materialisationFileID{}) {
			return errors.New("unexpected follower artifact identity")
		}
	case materialisationDataCreated:
		uid, err := materialisationPostgresUID()
		if err != nil || a.DataIdentity.Inode == 0 || a.DataIdentity.UID != uid || a.ConfigIdentity != (materialisationFileID{}) {
			return errors.New("invalid follower data identity")
		}
	case materialisationRegistered:
		uid, err := materialisationPostgresUID()
		if err != nil || a.DataIdentity.Inode == 0 || a.ConfigIdentity.Inode == 0 ||
			a.DataIdentity.UID != uid || a.ConfigIdentity.UID != uid {
			return errors.New("invalid follower artifact identities")
		}
	default:
		return errors.New("invalid follower artifact stage")
	}
	switch a.State {
	case followAttemptPending:
		if o.Phase != followPhaseCopying || a.Reason != "" {
			return errors.New("invalid pending follower attempt")
		}
	case followAttemptRetained:
		if o.Phase != followPhaseCopying || (a.Reason != "recovery_requirements_unmet" && a.Reason != "recovery_parameter_mismatch") {
			return errors.New("invalid retained follower attempt")
		}
	case followAttemptCompleted:
		if (o.Phase != followPhaseCopied && o.Phase != followPhaseStarted) || a.Reason != "" {
			return errors.New("invalid completed follower attempt")
		}
	default:
		return errors.New("unknown follower attempt state")
	}
	return nil
}

func writeFollowRetention(o followOwnership) error {
	if err := validateFollowRetentionAttempt(o); err != nil {
		return err
	}
	path, err := followOwnershipPath(o.Database)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(o)
	if err != nil {
		return err
	}
	return followRetentionWrite(path, append(raw, '\n'), 0o600)
}

// beginFollowAttempt is called after the legacy copying ownership is written
// and the constructor reserves its port, but before any restore or basebackup
// effect. A crash after this write leaves pending metadata that must block a
// future retry even if recording a more specific failure was impossible.
func beginFollowAttempt(owner followOwner, k followAttemptKey) error {
	return beginFollowAttemptWithReservation(owner, k, backup.ReservationIdentity{})
}

func beginFollowAttemptWithReservation(owner followOwner, k followAttemptKey, id backup.ReservationIdentity) error {
	if !validFollowAttemptKey(k) {
		return errors.New("invalid follower materialisation attempt identity")
	}
	o, owned, err := readFollowOwnership(owner)
	if err != nil {
		return err
	}
	if !owned || o.Phase != followPhaseCopying || o.Attempt != nil {
		return errors.New("follower materialisation is not available for a new attempt")
	}
	o.Attempt = &followRetentionAttempt{
		Job: k.Job, Operation: k.Operation, SourceStanza: k.SourceStanza,
		TemporaryPort: k.TemporaryPort, State: followAttemptPending, ReservationIdentity: id,
	}
	return writeFollowRetention(o)
}

func pendingFollowAttempt(owner followOwner, k followAttemptKey) (followOwnership, error) {
	if !validFollowAttemptKey(k) {
		return followOwnership{}, errors.New("invalid follower materialisation attempt identity")
	}
	o, owned, err := readFollowOwnership(owner)
	if err != nil {
		return followOwnership{}, err
	}
	if !owned || o.Attempt == nil || o.Attempt.State != followAttemptPending ||
		o.Attempt.Job != k.Job || o.Attempt.Operation != k.Operation ||
		o.Attempt.SourceStanza != k.SourceStanza || o.Attempt.TemporaryPort != k.TemporaryPort {
		return followOwnership{}, errors.New("follower materialisation attempt ownership changed")
	}
	return o, nil
}

// retainFollowAttempt records only the two proven recovery failures. If its
// write fails the last pending record is still a refusal, not permission to
// drop, recopy, resume or start the uncertain target.
func retainFollowAttempt(owner followOwner, k followAttemptKey, reason string) error {
	if reason != "recovery_requirements_unmet" && reason != "recovery_parameter_mismatch" {
		return errors.New("invalid follower materialisation failure reason")
	}
	o, err := pendingFollowAttempt(owner, k)
	if err != nil {
		return err
	}
	o.Attempt.State, o.Attempt.Reason = followAttemptRetained, reason
	return writeFollowRetention(o)
}

// completeFollowAttempt must be called only after the copy really completed.
// It records completion and clears the pending state in the same atomic write.
func completeFollowAttempt(owner followOwner, k followAttemptKey) error {
	o, err := pendingFollowAttempt(owner, k)
	if err != nil {
		return err
	}
	if o.Attempt.ArtifactStage != materialisationRegistered {
		return errors.New("follower artifacts are not bound")
	}
	o.Phase, o.Attempt.State = followPhaseCopied, followAttemptCompleted
	return writeFollowRetention(o)
}

// refuseIncompleteFollowAttempt must be called immediately after reading the
// record, before inspecting artifacts or choosing any retry/cleanup/adoption.
// It deliberately refuses even a foreign owner's pending record. Legacy
// records with no attempt retain their previous behavior.
func refuseIncompleteFollowAttempt(o followOwnership) error {
	if o.Attempt == nil {
		return nil
	}
	if err := validateFollowRetentionAttempt(o); err != nil {
		return fmt.Errorf("invalid follower materialisation attempt: %w", err)
	}
	if o.Attempt.State == followAttemptCompleted {
		return nil
	}
	details := map[string]any{
		"reason": "materialisation_incomplete", "database": o.Database,
		"source": o.Source, "job_id": o.Attempt.Job,
	}
	if o.Attempt.Reason != "" {
		details["failure_reason"] = o.Attempt.Reason
	}
	return output.NewError(output.CodePrecondition,
		"follower "+o.Database+" has an incomplete materialisation", output.ExitPrecondition,
		"The partial target was retained; do not retry, adopt or remove it without ownership-checked cleanup.").WithDetails(details)
}
