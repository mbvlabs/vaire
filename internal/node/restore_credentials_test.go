package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/secrets"
	"github.com/mbvlabs/vaire/internal/steps"
)

// restoreCredentialSQLFake models pg_authid's managed login roles. Its Exec
// accepts only the password transaction and never any grant or role attribute
// statement.
type restoreCredentialSQLFake struct {
	roles      map[string]restoreCredentialTestRole
	queries    int
	execs      []string
	queryErr   error
	execErr    error
	ignoreExec bool
	unexpected []string
}

type restoreCredentialTestRole struct {
	login    bool
	null     bool
	verifier string
}

func newRestoreCredentialSQLFake(db string) *restoreCredentialSQLFake {
	f := &restoreCredentialSQLFake{roles: map[string]restoreCredentialTestRole{}}
	for i, role := range restoreManagedLoginRoles(db) {
		f.roles[role] = restoreCredentialTestRole{
			login:    true,
			verifier: fmt.Sprintf("SCRAM-SHA-256$4096:c2FsdA%02d==$c3RvcmVk%02d:c2VydmVy%02d", i, i, i),
		}
	}
	return f
}

func (f *restoreCredentialSQLFake) Query(_ context.Context, db, _ string, _ map[string]string) ([]map[string]string, error) {
	f.queries++
	if db != steps.AuthDB {
		return nil, fmt.Errorf("query database = %s, want %s", db, steps.AuthDB)
	}
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	roles := make([]string, 0, len(f.roles))
	for role := range f.roles {
		roles = append(roles, role)
	}
	slices.Sort(roles)
	rows := make([]map[string]string, 0, len(roles))
	for _, role := range roles {
		credential := f.roles[role]
		login, state := "f", "set"
		if credential.login {
			login = "t"
		}
		if credential.null {
			state = "null"
		}
		rows = append(rows, map[string]string{
			"rolname": role, "rolcanlogin": login,
			"password_state": state, "verifier": credential.verifier,
		})
	}
	return rows, nil
}

func (f *restoreCredentialSQLFake) Exec(_ context.Context, db, sql string, _ map[string]string) error {
	f.execs = append(f.execs, sql)
	if db != steps.AuthDB {
		return fmt.Errorf("exec database = %s, want %s", db, steps.AuthDB)
	}
	if f.execErr != nil {
		return f.execErr
	}
	if f.ignoreExec {
		return nil
	}
	lines := strings.Split(strings.TrimSuffix(sql, "\n"), "\n")
	if len(lines) < 4 || lines[0] != "BEGIN;" || lines[1] != "SET LOCAL standard_conforming_strings = on;" ||
		lines[len(lines)-1] != "COMMIT;" {
		return errors.New("password changes are not one transaction")
	}
	for _, line := range lines[2 : len(lines)-1] {
		role, verifier, null, ok := parseRestoreCredentialAlter(line)
		if !ok {
			f.unexpected = append(f.unexpected, line)
			return fmt.Errorf("unexpected SQL %q", line)
		}
		credential, ok := f.roles[role]
		if !ok {
			return fmt.Errorf("SQL names unmanaged role %q", role)
		}
		credential.null, credential.verifier = null, ""
		if !null {
			credential.verifier = verifier
		}
		f.roles[role] = credential
	}
	return nil
}

func parseRestoreCredentialAlter(line string) (role, verifier string, null, ok bool) {
	const prefix = "ALTER ROLE "
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, ";") {
		return "", "", false, false
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(line, prefix), ";")
	idx := strings.Index(rest, " PASSWORD ")
	if idx < 0 {
		return "", "", false, false
	}
	quotedRole, value := rest[:idx], rest[idx+len(" PASSWORD "):]
	if len(quotedRole) < 2 || quotedRole[0] != '"' || quotedRole[len(quotedRole)-1] != '"' {
		return "", "", false, false
	}
	role = strings.ReplaceAll(quotedRole[1:len(quotedRole)-1], `""`, `"`)
	if value == "NULL" {
		return role, "", true, true
	}
	if len(value) < 2 || value[0] != '\'' || value[len(value)-1] != '\'' {
		return "", "", false, false
	}
	return role, strings.ReplaceAll(value[1:len(value)-1], "''", "'"), false, true
}

func restoreCredentialTestPromotion(t *testing.T) (*restorePromotion, *restoreCredentialSQLFake) {
	t.Helper()
	root := t.TempDir()
	oldRecords, oldUID, oldWrite, oldInstance := backup.RecordsPath, restoreCredentialsUID, restoreCredentialsWrite, followHostInstance
	backup.RecordsPath = filepath.Join(root, "records.json")
	restoreCredentialsUID = uint32(os.Getuid())
	followHostInstance = func() (string, error) { return "test-instance", nil }
	restoreCredentialsWrite = func(path string, data []byte, perm os.FileMode, owner, group string) error {
		if perm != restoreCredentialFileMode || owner != restoreCredentialOwner || group != restoreCredentialGroup {
			t.Errorf("snapshot writer arguments = (%o, %q, %q), want (%o, %q, %q)",
				perm, owner, group, restoreCredentialFileMode, restoreCredentialOwner, restoreCredentialGroup)
		}
		return engine.WriteFileAtomic(path, data, perm)
	}
	t.Cleanup(func() {
		backup.RecordsPath, restoreCredentialsUID, restoreCredentialsWrite, followHostInstance = oldRecords, oldUID, oldWrite, oldInstance
	})
	dataRoot := filepath.Join(root, "data")
	mustNoErr(t, os.Mkdir(dataRoot, 0o700))
	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = dataRoot
	spec := config.ClusterSpec{Name: "app", Port: 5432, Stanza: "app-abcdef", Size: "small"}
	cfg.Clusters = []config.ClusterSpec{spec}
	const id = "01K2V7A0Q4X9Y8Z7W6V5T4S3R2"
	const aside = "app__pre_restore_20261117T100000"
	p := &restorePromotion{
		cfg: cfg, db: "app", spec: spec,
		r:     backup.Restore{ID: id, Database: "app", Kind: backup.TargetLatest, Phase: backup.PhaseReady},
		aside: aside,
		fenceID: activationFenceIdentity{
			Database: "app", Kind: "restore-promote", OperationID: id,
			TargetStanza: spec.Stanza, Aside: aside,
		},
	}
	return p, newRestoreCredentialSQLFake(p.db)
}

func restoreCredentialTestSnapshot(t *testing.T, p *restorePromotion) (restoreCredentialSnapshot, []byte) {
	t.Helper()
	path, err := restoreCredentialSnapshotPath(p.db, p.r.ID)
	mustNoErr(t, err)
	raw, err := os.ReadFile(path)
	mustNoErr(t, err)
	var snap restoreCredentialSnapshot
	mustNoErr(t, json.Unmarshal(raw, &snap))
	return snap, raw
}

func restoreCredentialTestWriteSnapshot(t *testing.T, p *restorePromotion, snap restoreCredentialSnapshot) {
	t.Helper()
	path, err := restoreCredentialSnapshotPath(p.db, p.r.ID)
	mustNoErr(t, err)
	raw, err := json.Marshal(snap)
	mustNoErr(t, err)
	mustNoErr(t, os.Remove(path))
	mustNoErr(t, os.WriteFile(path, append(raw, '\n'), 0o600))
}

func restoreCredentialTestIdentity(t *testing.T, p *restorePromotion) restoreCredentialIdentity {
	t.Helper()
	id, err := restoreCredentialIdentityFor(p)
	mustNoErr(t, err)
	return id
}

func rejectRestoreCredentialLeak(t *testing.T, err error, fake *restoreCredentialSQLFake) {
	t.Helper()
	if err == nil {
		return
	}
	message := err.Error()
	for role, credential := range fake.roles {
		if credential.verifier != "" && strings.Contains(message, credential.verifier) {
			t.Fatalf("error for %s leaked a stored verifier", role)
		}
	}
}

func TestRestoreCredentialManagedLoginRoles(t *testing.T) {
	want := []string{"app_migrate", "app_app", "app_ro", "pgbouncer_auth", "vaire_monitor", "vaire_admin"}
	if got := restoreManagedLoginRoles("app"); !slices.Equal(got, want) {
		t.Fatalf("managed roles = %v, want %v", got, want)
	}
}

func TestRestoreCredentialsSeamsUseTheProductionWiring(t *testing.T) {
	p, fake := restoreCredentialTestPromotion(t)
	oldPG := restoreCredentialsPG
	restoreCredentialsPG = func(config.ClusterSpec) sqlRunner { return fake }
	t.Cleanup(func() { restoreCredentialsPG = oldPG })
	ctx := context.Background()
	mustNoErr(t, restoreCredentialsCapture(ctx, p))
	mustNoErr(t, restoreCredentialsApply(ctx, p))
	mustNoErr(t, restoreCredentialsClear(p))
	path, err := restoreCredentialSnapshotPath(p.db, p.r.ID)
	mustNoErr(t, err)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot remained after the seam cleanup: %v", err)
	}
	mustNoErr(t, restoreCredentialsCapture(ctx, p))
	mustNoErr(t, restoreCredentialsClearAborted(p.cfg, p.r, p.aside))
}

func TestRestoreCredentialsCaptureAndApply(t *testing.T) {
	p, fake := restoreCredentialTestPromotion(t)
	ctx := context.Background()
	mustNoErr(t, captureRestoreCredentialsAt(ctx, p, fake))
	if fake.queries != 1 {
		t.Fatalf("capture queries = %d, want 1", fake.queries)
	}
	path, err := restoreCredentialSnapshotPath(p.db, p.r.ID)
	mustNoErr(t, err)
	info, err := os.Stat(path)
	mustNoErr(t, err)
	if info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
		t.Fatalf("snapshot mode = %v, want regular 0600", info.Mode())
	}
	snap, raw := restoreCredentialTestSnapshot(t, p)
	if err := validateRestoreCredentialSnapshot(snap, restoreCredentialTestIdentity(t, p)); err != nil {
		t.Fatalf("snapshot does not validate: %v", err)
	}
	if len(snap.Roles) != len(restoreManagedLoginRoles(p.db)) {
		t.Fatalf("snapshot roles = %d, want %d", len(snap.Roles), len(restoreManagedLoginRoles(p.db)))
	}
	for _, role := range snap.Roles {
		if role.PasswordNull || role.Verifier != fake.roles[role.Role].verifier {
			t.Fatalf("snapshot %s = %+v, want the live verifier", role.Role, role)
		}
	}
	lower := strings.ToLower(string(raw))
	for _, forbidden := range []string{"grant", "member", "superuser", "createrole", "login"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("snapshot contains privilege or attribute text %q", forbidden)
		}
	}
	canary := fake.roles[p.db+"_app"].verifier
	if got := secrets.Redact(canary); got != secrets.Placeholder {
		t.Fatalf("captured verifier is not registered for redaction: %q", got)
	}

	// The promoted copy still has historical verifiers; apply must put the
	// captured ones back exactly and verify the result.
	for role, credential := range fake.roles {
		credential.verifier = "SCRAM-SHA-256$4096:aGlzdG9yaWNhbA==$c3RvcmVk:c2VydmVy"
		fake.roles[role] = credential
	}
	mustNoErr(t, applyRestoreCredentialsAt(ctx, p, fake))
	if len(fake.execs) != 1 {
		t.Fatalf("apply execs = %d, want 1", len(fake.execs))
	}
	for _, role := range snap.Roles {
		got := fake.roles[role.Role]
		if got.null != role.PasswordNull || got.verifier != role.Verifier {
			t.Fatalf("role %s = %+v after apply, want %+v", role.Role, got, role)
		}
	}
	if len(fake.unexpected) != 0 {
		t.Fatalf("apply ran unexpected SQL: %v", fake.unexpected)
	}
}

func TestRestoreCredentialsApplyNullAndQuotedVerifier(t *testing.T) {
	p, fake := restoreCredentialTestPromotion(t)
	ctx := context.Background()
	nullRole := p.db + "_ro"
	quotedRole := p.db + "_app"
	null := fake.roles[nullRole]
	null.null, null.verifier = true, ""
	fake.roles[nullRole] = null
	quoted := fake.roles[quotedRole]
	quoted.verifier = "SCRAM-SHA-256$4096:aW1wb3J0ZWQn$aGFzaA==:c2VydmVy"
	fake.roles[quotedRole] = quoted
	mustNoErr(t, captureRestoreCredentialsAt(ctx, p, fake))

	for role, credential := range fake.roles {
		credential.null, credential.verifier = false, "SCRAM-SHA-256$4096:d3Jvbmc=$c3RvcmVk:c2VydmVy"
		fake.roles[role] = credential
	}
	mustNoErr(t, applyRestoreCredentialsAt(ctx, p, fake))
	if got := fake.roles[nullRole]; !got.null || got.verifier != "" {
		t.Fatalf("%s = %+v after apply, want SQL NULL", nullRole, got)
	}
	if got := fake.roles[quotedRole]; got.null || got.verifier != quoted.verifier {
		t.Fatalf("%s = %+v after apply, want the exact quoted verifier", quotedRole, got)
	}
	if !strings.Contains(fake.execs[0], "ALTER ROLE \"app_ro\" PASSWORD NULL;") {
		t.Fatalf("apply SQL does not set NULL exactly:\n%s", fake.execs[0])
	}
}

func TestRestoreCredentialsCaptureIsRetrySafe(t *testing.T) {
	p, first := restoreCredentialTestPromotion(t)
	mustNoErr(t, captureRestoreCredentialsAt(context.Background(), p, first))
	before, _ := restoreCredentialTestSnapshot(t, p)

	second := newRestoreCredentialSQLFake(p.db)
	second.queryErr = errors.New("a retry must not read the live cluster again")
	mustNoErr(t, captureRestoreCredentialsAt(context.Background(), p, second))
	if second.queries != 0 {
		t.Fatalf("retry queries = %d, want 0", second.queries)
	}
	after, _ := restoreCredentialTestSnapshot(t, p)
	if !slices.Equal(before.Roles, after.Roles) || before.Instance != after.Instance {
		t.Fatal("a retry changed the captured snapshot")
	}
}

func TestRestoreCredentialsCaptureSkipsRecoverAndDump(t *testing.T) {
	for _, mode := range []string{"recover", "dump"} {
		t.Run(mode, func(t *testing.T) {
			p, fake := restoreCredentialTestPromotion(t)
			fake.queryErr = errors.New("no SQL may run")
			if mode == "recover" {
				p.r.Recover = true
			} else {
				p.r.Kind = targetDump
			}
			mustNoErr(t, captureRestoreCredentialsAt(context.Background(), p, fake))
			mustNoErr(t, applyRestoreCredentialsAt(context.Background(), p, fake))
			if fake.queries != 0 || len(fake.execs) != 0 {
				t.Fatalf("queries/execs = %d/%d, want 0/0", fake.queries, len(fake.execs))
			}
			path, err := restoreCredentialSnapshotPath(p.db, p.r.ID)
			mustNoErr(t, err)
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("snapshot exists for %s: %v", mode, err)
			}
		})
	}
}

func TestRestoreCredentialsCaptureRefusesMissingRole(t *testing.T) {
	p, fake := restoreCredentialTestPromotion(t)
	delete(fake.roles, "vaire_admin")
	err := captureRestoreCredentialsAt(context.Background(), p, fake)
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("capture error = %v, want a missing managed role", err)
	}
	path, pathErr := restoreCredentialSnapshotPath(p.db, p.r.ID)
	mustNoErr(t, pathErr)
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("an incomplete snapshot was written: %v", statErr)
	}
}

func TestRestoreCredentialsCaptureRefusesAfterSwapBegan(t *testing.T) {
	for _, mode := range []string{backup.SwapDone, backup.SwapHalfway, backup.SwapConflict} {
		t.Run(mode, func(t *testing.T) {
			p, fake := restoreCredentialTestPromotion(t)
			aside := filepath.Join(p.cfg.Server.DataRoot, p.aside)
			live := filepath.Join(p.cfg.Server.DataRoot, p.db)
			from := filepath.Join(p.cfg.Server.DataRoot, backup.RestoringName(p.db))
			mustNoErr(t, os.Mkdir(aside, 0o700))
			switch mode {
			case backup.SwapDone:
				mustNoErr(t, os.Mkdir(live, 0o700))
			case backup.SwapHalfway:
				mustNoErr(t, os.Mkdir(from, 0o700))
			case backup.SwapConflict:
				mustNoErr(t, os.Mkdir(live, 0o700))
				mustNoErr(t, os.Mkdir(from, 0o700))
			}
			err := captureRestoreCredentialsAt(context.Background(), p, fake)
			if err == nil || !strings.Contains(err.Error(), "swap began") {
				t.Fatalf("capture error = %v, want a swapped-state refusal", err)
			}
			if fake.queries != 0 {
				t.Fatalf("capture read the restored target after the swap")
			}
			path, pathErr := restoreCredentialSnapshotPath(p.db, p.r.ID)
			mustNoErr(t, pathErr)
			if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("a historical snapshot was written: %v", statErr)
			}
		})
	}
}

func TestRestoreCredentialsApplyRefusesMissingSnapshot(t *testing.T) {
	p, fake := restoreCredentialTestPromotion(t)
	err := applyRestoreCredentialsAt(context.Background(), p, fake)
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("apply error = %v, want a missing snapshot refusal", err)
	}
	if fake.queries != 0 || len(fake.execs) != 0 {
		t.Fatalf("apply queried/executed without a snapshot: %d/%d", fake.queries, len(fake.execs))
	}
}

func TestRestoreCredentialsApplyVerifiesExactResult(t *testing.T) {
	p, fake := restoreCredentialTestPromotion(t)
	ctx := context.Background()
	mustNoErr(t, captureRestoreCredentialsAt(ctx, p, fake))
	snap, _ := restoreCredentialTestSnapshot(t, p)
	for role, credential := range fake.roles {
		credential.verifier = "SCRAM-SHA-256$4096:d3Jvbmc=$c3RvcmVk:c2VydmVy"
		fake.roles[role] = credential
	}
	fake.ignoreExec = true
	err := applyRestoreCredentialsAt(ctx, p, fake)
	if err == nil || !strings.Contains(err.Error(), "did not verify") {
		t.Fatalf("apply error = %v, want a verification refusal", err)
	}
	for _, role := range snap.Roles {
		if strings.Contains(err.Error(), role.Verifier) {
			t.Fatalf("verification error for %s leaked its verifier", role.Role)
		}
	}
	rejectRestoreCredentialLeak(t, err, fake)
}

func TestRestoreCredentialsSnapshotTamperRefused(t *testing.T) {
	cases := []struct {
		name   string
		tamper func(t *testing.T, p *restorePromotion, path string, snap restoreCredentialSnapshot)
	}{
		{"instance", func(t *testing.T, p *restorePromotion, path string, snap restoreCredentialSnapshot) {
			snap.Instance = "another-installation"
			restoreCredentialTestWriteSnapshot(t, p, snap)
		}},
		{"database", func(t *testing.T, p *restorePromotion, path string, snap restoreCredentialSnapshot) {
			snap.Database = "other"
			restoreCredentialTestWriteSnapshot(t, p, snap)
		}},
		{"restore id", func(t *testing.T, p *restorePromotion, path string, snap restoreCredentialSnapshot) {
			snap.RestoreID = "01K2V7A0Q4X9Y8Z7W6V5T4S3R3"
			restoreCredentialTestWriteSnapshot(t, p, snap)
		}},
		{"stanza", func(t *testing.T, p *restorePromotion, path string, snap restoreCredentialSnapshot) {
			snap.Stanza = "app-ffffff"
			restoreCredentialTestWriteSnapshot(t, p, snap)
		}},
		{"aside", func(t *testing.T, p *restorePromotion, path string, snap restoreCredentialSnapshot) {
			snap.Aside = "app__pre_restore_20261117T100001"
			restoreCredentialTestWriteSnapshot(t, p, snap)
		}},
		{"extra role", func(t *testing.T, p *restorePromotion, path string, snap restoreCredentialSnapshot) {
			snap.Roles = append(snap.Roles, restoreCredential{Role: "postgres", Verifier: "SCRAM-SHA-256$4096:eA==$eA==:eA=="})
			restoreCredentialTestWriteSnapshot(t, p, snap)
		}},
		{"duplicate role", func(t *testing.T, p *restorePromotion, path string, snap restoreCredentialSnapshot) {
			snap.Roles[1] = snap.Roles[0]
			restoreCredentialTestWriteSnapshot(t, p, snap)
		}},
		{"missing role", func(t *testing.T, p *restorePromotion, path string, snap restoreCredentialSnapshot) {
			snap.Roles = snap.Roles[1:]
			restoreCredentialTestWriteSnapshot(t, p, snap)
		}},
		{"empty verifier", func(t *testing.T, p *restorePromotion, path string, snap restoreCredentialSnapshot) {
			snap.Roles[0].PasswordNull, snap.Roles[0].Verifier = false, ""
			restoreCredentialTestWriteSnapshot(t, p, snap)
		}},
		{"null and verifier", func(t *testing.T, p *restorePromotion, path string, snap restoreCredentialSnapshot) {
			snap.Roles[0].PasswordNull = true
			restoreCredentialTestWriteSnapshot(t, p, snap)
		}},
		{"unknown field", func(t *testing.T, p *restorePromotion, path string, snap restoreCredentialSnapshot) {
			raw, err := os.ReadFile(path)
			mustNoErr(t, err)
			var value map[string]any
			mustNoErr(t, json.Unmarshal(raw, &value))
			value["privileges"] = map[string]any{"superuser": true}
			raw, err = json.Marshal(value)
			mustNoErr(t, err)
			mustNoErr(t, os.WriteFile(path, raw, 0o600))
		}},
		{"mode", func(t *testing.T, p *restorePromotion, path string, snap restoreCredentialSnapshot) {
			mustNoErr(t, os.Chmod(path, 0o644))
		}},
		{"symlink", func(t *testing.T, p *restorePromotion, path string, snap restoreCredentialSnapshot) {
			target := filepath.Join(filepath.Dir(path), "foreign.json")
			raw, err := json.Marshal(snap)
			mustNoErr(t, err)
			mustNoErr(t, os.WriteFile(target, raw, 0o600))
			mustNoErr(t, os.Remove(path))
			mustNoErr(t, os.Symlink(target, path))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, fake := restoreCredentialTestPromotion(t)
			mustNoErr(t, captureRestoreCredentialsAt(context.Background(), p, fake))
			path, err := restoreCredentialSnapshotPath(p.db, p.r.ID)
			mustNoErr(t, err)
			snap, _ := restoreCredentialTestSnapshot(t, p)
			c.tamper(t, p, path, snap)
			queries, execs := fake.queries, len(fake.execs)
			err = applyRestoreCredentialsAt(context.Background(), p, fake)
			if err == nil {
				t.Fatal("apply accepted a tampered snapshot")
			}
			rejectRestoreCredentialLeak(t, err, fake)
			if fake.queries != queries || len(fake.execs) != execs {
				t.Fatal("apply contacted the cluster with a tampered snapshot")
			}
		})
	}
}

func TestRestoreCredentialsCleanupOwnership(t *testing.T) {
	t.Run("exact", func(t *testing.T) {
		p, fake := restoreCredentialTestPromotion(t)
		mustNoErr(t, captureRestoreCredentialsAt(context.Background(), p, fake))
		mustNoErr(t, clearRestoreCredentials(p))
		path, err := restoreCredentialSnapshotPath(p.db, p.r.ID)
		mustNoErr(t, err)
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("snapshot remained after cleanup: %v", err)
		}
		mustNoErr(t, clearRestoreCredentials(p))
	})

	t.Run("mismatched identity keeps it", func(t *testing.T) {
		p, fake := restoreCredentialTestPromotion(t)
		mustNoErr(t, captureRestoreCredentialsAt(context.Background(), p, fake))
		original := p.aside
		p.aside = "app__pre_restore_20261117T100001"
		if err := clearRestoreCredentials(p); err == nil {
			t.Fatal("cleanup accepted a changed aside")
		}
		p.aside = original
		path, err := restoreCredentialSnapshotPath(p.db, p.r.ID)
		mustNoErr(t, err)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("cleanup removed a snapshot it did not own: %v", err)
		}
	})

	t.Run("malformed keeps it", func(t *testing.T) {
		p, fake := restoreCredentialTestPromotion(t)
		mustNoErr(t, captureRestoreCredentialsAt(context.Background(), p, fake))
		path, err := restoreCredentialSnapshotPath(p.db, p.r.ID)
		mustNoErr(t, err)
		mustNoErr(t, os.WriteFile(path, []byte("{not json"), 0o600))
		if err := clearRestoreCredentials(p); err == nil {
			t.Fatal("cleanup removed a malformed snapshot")
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("malformed snapshot was removed: %v", err)
		}
	})

	t.Run("symlink keeps its target", func(t *testing.T) {
		p, fake := restoreCredentialTestPromotion(t)
		mustNoErr(t, captureRestoreCredentialsAt(context.Background(), p, fake))
		path, err := restoreCredentialSnapshotPath(p.db, p.r.ID)
		mustNoErr(t, err)
		target := filepath.Join(filepath.Dir(path), "foreign.json")
		raw, err := os.ReadFile(path)
		mustNoErr(t, err)
		mustNoErr(t, os.WriteFile(target, raw, 0o600))
		mustNoErr(t, os.Remove(path))
		mustNoErr(t, os.Symlink(target, path))
		if err := clearRestoreCredentials(p); err == nil {
			t.Fatal("cleanup accepted a symlink")
		}
		if _, err := os.Stat(target); err != nil {
			t.Fatalf("cleanup removed the symlink target: %v", err)
		}
	})
}

func TestRestoreCredentialsAbortedCleanupUsesRecordIdentity(t *testing.T) {
	p, fake := restoreCredentialTestPromotion(t)
	mustNoErr(t, captureRestoreCredentialsAt(context.Background(), p, fake))
	r := p.r
	if err := clearRestoreCredentialsForRestore(p.cfg, r, "app__pre_restore_20261117T100001"); err == nil {
		t.Fatal("abort cleanup accepted another aside")
	}
	r.Aside = "app__pre_restore_20261117T100001"
	if err := clearRestoreCredentialsForRestore(p.cfg, r, p.aside); err == nil {
		t.Fatal("abort cleanup accepted a record with another aside")
	}
	r = p.r
	mustNoErr(t, clearRestoreCredentialsForRestore(p.cfg, r, p.aside))

	dump := r
	dump.Kind, dump.Aside = targetDump, ""
	mustNoErr(t, clearRestoreCredentialsForRestore(p.cfg, dump, ""))
	recover := r
	recover.Recover, recover.Aside = true, ""
	mustNoErr(t, clearRestoreCredentialsForRestore(p.cfg, recover, ""))
}
