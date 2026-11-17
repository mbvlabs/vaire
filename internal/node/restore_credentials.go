package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/pg"
	"github.com/mbvlabs/vaire/internal/secrets"
	"github.com/mbvlabs/vaire/internal/steps"
)

func restoreCredentialCleanupWarning(db string, err error) output.Warning {
	return output.Warning{
		Code:    "restore_credentials_cleanup_failed",
		Message: db + "'s restore is retired, but its root-only credential snapshot could not be removed: " + err.Error() + "; inspect the node state as root.",
	}
}

// A physical restore copies the backup's pg_authid, so promoting it would roll
// every managed login password back to the backup's verifier. The snapshot
// below preserves the verifiers the live cluster had immediately before the
// first swap and reapplies exactly those values after the restored copy is
// promoted and writable. It touches passwords only: role attributes,
// memberships and ACLs remain the promoted data's own.
//
// The file is node-local root-only state, like the activation records. It is
// written once, atomically, before the swap and removed only after the
// restore's durable retirement, when a retry can no longer need it. A missing
// snapshot after the swap has begun is a hard failure: the restored copy
// already holds historical verifiers and must never be used to reconstruct
// them.

const (
	restoreCredentialSnapshotVersion  = 1
	restoreCredentialSnapshotMaxBytes = 1 << 16
	restoreCredentialFileMode         = os.FileMode(0o600)
	restoreCredentialOwner            = "root"
	restoreCredentialGroup            = "root"
)

// restoreCredential is one managed login role's exact pg_authid.rolpassword.
// PasswordNull distinguishes SQL NULL from an empty string; an empty non-NULL
// verifier is refused rather than guessed.
type restoreCredential struct {
	Role         string `json:"role"`
	PasswordNull bool   `json:"password_null,omitempty"`
	Verifier     string `json:"verifier,omitempty"`
}

// restoreCredentialSnapshot is bound to the installation, database, restore,
// immutable stanza and aside it was captured for. It never stores role
// attributes, memberships or grants.
type restoreCredentialSnapshot struct {
	Version    int                 `json:"version"`
	Instance   string              `json:"instance"`
	Database   string              `json:"database"`
	RestoreID  string              `json:"restore_id"`
	Stanza     string              `json:"stanza"`
	Aside      string              `json:"aside"`
	CapturedAt string              `json:"captured_at"`
	Roles      []restoreCredential `json:"roles"`
}

type restoreCredentialIdentity struct {
	Instance  string
	Database  string
	RestoreID string
	Stanza    string
	Aside     string
}

var (
	// restoreCredentialsUID is the state directory and snapshot owner. Tests
	// run unprivileged and set it to their own uid; production leaves it 0.
	restoreCredentialsUID uint32
	// restoreCredentialsWrite is the root-owned atomic writer. A test may
	// substitute a wrapper that keeps the same arguments but does not chown.
	restoreCredentialsWrite = engine.WriteFileAtomicOwned
	// restoreCredentialsPG is the SQL surface. Production is the cluster's
	// pg.Local, which feeds SQL on psql's stdin, never argv.
	restoreCredentialsPG = func(spec config.ClusterSpec) sqlRunner { return steps.ClusterPG(spec) }
	// These seams let callers that never reach a real cluster stub the
	// credential lifecycle without weakening the real implementation.
	restoreCredentialsCapture      = captureRestoreCredentials
	restoreCredentialsApply        = applyRestoreCredentials
	restoreCredentialsClear        = clearRestoreCredentials
	restoreCredentialsClearAborted = clearRestoreCredentialsForRestore
)

// captureRestoreCredentials records the live managed login verifiers before
// the first swap. It is idempotent: an existing snapshot is validated and
// reused, never replaced. A missing snapshot may be captured only while
// backup.SwapState is SwapNotStarted.
func captureRestoreCredentials(ctx context.Context, p *restorePromotion) error {
	if p == nil {
		return errors.New("restore credential capture has no promotion state")
	}
	return captureRestoreCredentialsAt(ctx, p, restoreCredentialsPG(p.spec))
}

func captureRestoreCredentialsAt(ctx context.Context, p *restorePromotion, local sqlRunner) error {
	if p == nil {
		return errors.New("restore credential capture has no promotion state")
	}
	if p.r.Recover || p.r.Kind == targetDump {
		return nil
	}
	id, err := restoreCredentialIdentityFor(p)
	if err != nil {
		return err
	}
	path, err := restoreCredentialSnapshotPath(id.Database, id.RestoreID)
	if err != nil {
		return err
	}
	snap, exists, err := readRestoreCredentialSnapshot(path)
	if err != nil {
		return err
	}
	if exists {
		return validateRestoreCredentialSnapshot(snap, id)
	}
	if state := backup.SwapState(p.cfg.Server.DataRoot, p.db, backup.RestoringName(p.db), p.aside); state != backup.SwapNotStarted {
		return fmt.Errorf("the restore credential snapshot of %s is missing after its data swap began (%s); vaire will not capture the restored target's historical passwords", p.db, state)
	}
	if local == nil {
		return errors.New("restore credential capture has no SQL runner")
	}
	roles, err := readRestoreCredentialValues(ctx, local, p.db)
	if err != nil {
		return err
	}
	snap = restoreCredentialSnapshot{
		Version:  restoreCredentialSnapshotVersion,
		Instance: id.Instance, Database: id.Database, RestoreID: id.RestoreID,
		Stanza: id.Stanza, Aside: id.Aside, CapturedAt: stamp(),
		Roles: append([]restoreCredential(nil), roles...),
	}
	if err := writeRestoreCredentialSnapshot(path, snap); err != nil {
		return err
	}
	return nil
}

// applyRestoreCredentials reapplies the persisted snapshot to the promoted,
// writable cluster. A missing or mismatched snapshot fails closed. The ALTER
// ROLE statements run in one transaction and their stored results are read
// back exactly.
func applyRestoreCredentials(ctx context.Context, p *restorePromotion) error {
	if p == nil {
		return errors.New("restore credential apply has no promotion state")
	}
	return applyRestoreCredentialsAt(ctx, p, restoreCredentialsPG(p.spec))
}

func applyRestoreCredentialsAt(ctx context.Context, p *restorePromotion, local sqlRunner) error {
	if p == nil {
		return errors.New("restore credential apply has no promotion state")
	}
	if p.r.Recover || p.r.Kind == targetDump {
		return nil
	}
	id, err := restoreCredentialIdentityFor(p)
	if err != nil {
		return err
	}
	path, err := restoreCredentialSnapshotPath(id.Database, id.RestoreID)
	if err != nil {
		return err
	}
	snap, exists, err := readRestoreCredentialSnapshot(path)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("the restore credential snapshot of %s is missing; it can only be captured before the data swap and must not be rebuilt from the restored target", p.db)
	}
	if err := validateRestoreCredentialSnapshot(snap, id); err != nil {
		return err
	}
	if local == nil {
		return errors.New("restore credential apply has no SQL runner")
	}
	if err := local.Exec(ctx, steps.AuthDB, restoreCredentialApplySQL(snap.Roles), nil); err != nil {
		return fmt.Errorf("restore the managed login credentials of %s: %w", p.db, err)
	}
	got, err := readRestoreCredentialValues(ctx, local, p.db)
	if err != nil {
		return fmt.Errorf("verify the managed login credentials of %s after restore: %w", p.db, err)
	}
	want := make(map[string]restoreCredential, len(snap.Roles))
	for _, role := range snap.Roles {
		want[role.Role] = role
	}
	for _, role := range got {
		w, ok := want[role.Role]
		if !ok || w != role {
			return fmt.Errorf("the managed login credential of role %s did not verify after restore", role.Role)
		}
	}
	return nil
}

// clearRestoreCredentials removes the snapshot of a restore whose promote has
// durably retired it into routing-only. A missing file is already clear. The
// complete snapshot identity must match; a foreign or malformed file is kept
// for inspection, never repaired or force-removed.
func clearRestoreCredentials(p *restorePromotion) error {
	if p == nil {
		return errors.New("restore credential cleanup has no promotion state")
	}
	if p.r.Recover || p.r.Kind == targetDump {
		return nil
	}
	id, err := restoreCredentialIdentityFor(p)
	if err != nil {
		return err
	}
	return removeRestoreCredentialSnapshot(id)
}

// clearRestoreCredentialsForRestore is the ownership-checked cleanup for an
// abort or a retired restore whose promotion state is no longer available. It
// uses the same ready record, stanza and aside the promote fixed; anything
// else leaves the snapshot untouched.
func clearRestoreCredentialsForRestore(cfg config.Cluster, r backup.Restore, aside string) error {
	if r.Recover || r.Kind == targetDump {
		return nil
	}
	if r.ID == "" || r.Database == "" {
		return fmt.Errorf("the restore credential snapshot of %s cannot be identified without its restore", r.Database)
	}
	if aside == "" {
		// Capture requires the promote's aside, so a restore without one has
		// no snapshot to clear.
		return nil
	}
	if r.Aside != "" && r.Aside != aside {
		return fmt.Errorf("the restore credential snapshot of %s cannot be identified without its restore, stanza and aside", r.Database)
	}
	spec, ok := cfg.ClusterByName(r.Database)
	if !ok || spec.Name != r.Database || spec.Stanza == "" {
		return fmt.Errorf("the restore credential snapshot of %s has no matching cluster.toml stanza", r.Database)
	}
	instance, err := currentFollowInstance()
	if err != nil {
		return err
	}
	return removeRestoreCredentialSnapshot(restoreCredentialIdentity{
		Instance: instance, Database: r.Database, RestoreID: r.ID,
		Stanza: spec.Stanza, Aside: aside,
	})
}

func removeRestoreCredentialSnapshot(id restoreCredentialIdentity) error {
	path, err := restoreCredentialSnapshotPath(id.Database, id.RestoreID)
	if err != nil {
		return err
	}
	snap, exists, err := readRestoreCredentialSnapshot(path)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if err := validateRestoreCredentialSnapshot(snap, id); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove the restore credential snapshot of %s: %w", id.Database, err)
	}
	return nil
}

func restoreCredentialIdentityFor(p *restorePromotion) (restoreCredentialIdentity, error) {
	if p.db == "" || p.spec.Name != p.db || p.r.Database != p.db ||
		p.r.ID == "" || p.r.Phase != backup.PhaseReady {
		return restoreCredentialIdentity{}, errors.New("the restore credential snapshot needs the ready restore record of the promoted database")
	}
	if p.aside == "" {
		return restoreCredentialIdentity{}, errors.New("the restore credential snapshot needs the promote's aside")
	}
	if p.fenceID.Database != p.db || p.fenceID.Kind != "restore-promote" ||
		p.fenceID.OperationID != p.r.ID || p.fenceID.TargetStanza == "" ||
		p.fenceID.TargetStanza != p.spec.Stanza || p.fenceID.Aside != p.aside {
		return restoreCredentialIdentity{}, errors.New("the restore credential snapshot does not match the prepared promote fence")
	}
	instance, err := currentFollowInstance()
	if err != nil {
		return restoreCredentialIdentity{}, err
	}
	return restoreCredentialIdentity{
		Instance: instance, Database: p.db, RestoreID: p.r.ID,
		Stanza: p.fenceID.TargetStanza, Aside: p.aside,
	}, nil
}

// restoreManagedLoginRoles is the exact set the snapshot may touch: the
// database's prefixed login roles, the cluster-wide service logins and
// vaire_admin. Nothing else is read or written.
func restoreManagedLoginRoles(db string) []string {
	roles := make([]string, 0, len(steps.DatabaseRoles)+len(steps.ServiceRoles)+1)
	for _, role := range steps.DatabaseRoles {
		roles = append(roles, db+"_"+role)
	}
	roles = append(roles, steps.ServiceRoles...)
	roles = append(roles, pg.RoleAdmin)
	return roles
}

func readRestoreCredentialValues(ctx context.Context, local sqlRunner, db string) ([]restoreCredential, error) {
	expected := restoreManagedLoginRoles(db)
	rows, err := local.Query(ctx, steps.AuthDB, restoreCredentialQuery(expected), nil)
	if err != nil {
		return nil, fmt.Errorf("read the managed login credentials of %s: %w", db, err)
	}
	expectedSet := make(map[string]bool, len(expected))
	for _, role := range expected {
		expectedSet[role] = true
	}
	found := make(map[string]restoreCredential, len(expected))
	for _, row := range rows {
		role := row["rolname"]
		if !expectedSet[role] {
			return nil, fmt.Errorf("the managed login credential query returned unexpected role %q", role)
		}
		if _, ok := found[role]; ok {
			return nil, fmt.Errorf("the managed login credential query returned role %q twice", role)
		}
		if row["rolcanlogin"] != "t" {
			return nil, fmt.Errorf("managed role %q is not a login role", role)
		}
		switch row["password_state"] {
		case "null":
			found[role] = restoreCredential{Role: role, PasswordNull: true}
		case "set":
			verifier := row["verifier"]
			if verifier == "" || strings.ContainsRune(verifier, 0) {
				return nil, fmt.Errorf("managed login role %q has an unusable stored password", role)
			}
			secrets.Add(verifier)
			found[role] = restoreCredential{Role: role, Verifier: verifier}
		default:
			return nil, fmt.Errorf("managed login role %q has an unknown stored password state", role)
		}
	}
	out := make([]restoreCredential, 0, len(expected))
	for _, role := range expected {
		credential, ok := found[role]
		if !ok {
			return nil, fmt.Errorf("managed login role %q is missing", role)
		}
		out = append(out, credential)
	}
	return out, nil
}

func restoreCredentialQuery(roles []string) string {
	quoted := make([]string, len(roles))
	for i, role := range roles {
		quoted[i] = restoreCredentialSQLString(role)
	}
	return "SELECT rolname, rolcanlogin, CASE WHEN rolpassword IS NULL THEN 'null' ELSE 'set' END AS password_state, " +
		"COALESCE(rolpassword, '') AS verifier FROM pg_catalog.pg_authid WHERE rolname IN (" + strings.Join(quoted, ", ") + ") ORDER BY rolname"
}

func restoreCredentialApplySQL(roles []restoreCredential) string {
	var b strings.Builder
	b.WriteString("BEGIN;\n")
	// The verifier is a literal below. Force the literal rules the escaping
	// assumes, even if the server configuration changed the default.
	b.WriteString("SET LOCAL standard_conforming_strings = on;\n")
	for _, role := range roles {
		if role.PasswordNull {
			fmt.Fprintf(&b, "ALTER ROLE %s PASSWORD NULL;\n", pg.Quote(role.Role))
			continue
		}
		fmt.Fprintf(&b, "ALTER ROLE %s PASSWORD %s;\n", pg.Quote(role.Role), restoreCredentialSQLString(role.Verifier))
	}
	b.WriteString("COMMIT;\n")
	return b.String()
}

func restoreCredentialSQLString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func validateRestoreCredentialSnapshot(snap restoreCredentialSnapshot, want restoreCredentialIdentity) error {
	if snap.Version != restoreCredentialSnapshotVersion {
		return fmt.Errorf("the restore credential snapshot of %s has unsupported version %d", want.Database, snap.Version)
	}
	if snap.Instance != want.Instance || snap.Database != want.Database ||
		snap.RestoreID != want.RestoreID || snap.Stanza != want.Stanza || snap.Aside != want.Aside {
		return fmt.Errorf("the restore credential snapshot of %s belongs to another installation, restore, stanza or aside", want.Database)
	}
	expected := restoreManagedLoginRoles(want.Database)
	if len(snap.Roles) != len(expected) {
		return fmt.Errorf("the restore credential snapshot of %s does not hold exactly its managed login roles", want.Database)
	}
	expectedSet := make(map[string]bool, len(expected))
	for _, role := range expected {
		expectedSet[role] = true
	}
	seen := make(map[string]bool, len(expected))
	for _, role := range snap.Roles {
		if !expectedSet[role.Role] || seen[role.Role] {
			return fmt.Errorf("the restore credential snapshot of %s holds an unexpected or duplicate role %q", want.Database, role.Role)
		}
		seen[role.Role] = true
		if role.PasswordNull {
			if role.Verifier != "" {
				return fmt.Errorf("the restore credential snapshot of %s has both a NULL and a stored password for role %s", want.Database, role.Role)
			}
			continue
		}
		if role.Verifier == "" || strings.ContainsRune(role.Verifier, 0) {
			return fmt.Errorf("the restore credential snapshot of %s has an unusable password for role %s", want.Database, role.Role)
		}
		secrets.Add(role.Verifier)
	}
	for _, role := range expected {
		if !seen[role] {
			return fmt.Errorf("the restore credential snapshot of %s is missing the managed login role %s", want.Database, role)
		}
	}
	return nil
}

func restoreCredentialSnapshotPath(db, restoreID string) (string, error) {
	for _, value := range []string{db, restoreID} {
		if value == "" || value == "." || value == ".." || filepath.Base(value) != value || strings.ContainsAny(value, `/\`) {
			return "", fmt.Errorf("restore credential snapshot name %q is unsafe", value)
		}
	}
	return filepath.Join(filepath.Dir(backup.RecordsPath), "restore-credentials-"+db+"-"+restoreID+".json"), nil
}

func writeRestoreCredentialSnapshot(path string, snap restoreCredentialSnapshot) error {
	if err := checkRestoreCredentialDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("encode the restore credential snapshot: %w", err)
	}
	if err := restoreCredentialsWrite(path, append(raw, '\n'), restoreCredentialFileMode, restoreCredentialOwner, restoreCredentialGroup); err != nil {
		return fmt.Errorf("write the restore credential snapshot: %w", err)
	}
	return nil
}

func readRestoreCredentialSnapshot(path string) (restoreCredentialSnapshot, bool, error) {
	var snap restoreCredentialSnapshot
	if err := checkRestoreCredentialDirectory(filepath.Dir(path)); err != nil {
		return snap, false, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return snap, false, nil
	}
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return snap, false, fmt.Errorf("the restore credential snapshot %s is a symlink", path)
		}
		return snap, false, fmt.Errorf("open the restore credential snapshot: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return snap, false, fmt.Errorf("inspect the restore credential snapshot: %w", err)
	}
	if err := checkRestoreCredentialFile(info); err != nil {
		return snap, false, err
	}
	if info.Size() > restoreCredentialSnapshotMaxBytes {
		return snap, false, errors.New("the restore credential snapshot is too large")
	}
	dec := json.NewDecoder(io.LimitReader(f, restoreCredentialSnapshotMaxBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&snap); err != nil {
		return snap, false, fmt.Errorf("the restore credential snapshot is corrupt: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return snap, false, errors.New("the restore credential snapshot has trailing data")
	}
	return snap, true, nil
}

func checkRestoreCredentialDirectory(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect the node state directory: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != restoreCredentialsUID || info.Mode()&os.ModeSymlink != 0 ||
		!info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return errors.New("the node state directory is not a safe root-owned directory")
	}
	return nil
}

func checkRestoreCredentialFile(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != restoreCredentialsUID || stat.Nlink != 1 ||
		!info.Mode().IsRegular() || info.Mode().Perm() != restoreCredentialFileMode ||
		info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return errors.New("the restore credential snapshot is not a private root-owned regular file")
	}
	return nil
}
