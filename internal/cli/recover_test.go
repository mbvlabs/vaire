package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/contexts"
	"github.com/mbvlabs/vaire/internal/node"
	"github.com/mbvlabs/vaire/internal/steps"
)

// The full backup after db recover --all decodes as backup.now's params,
// strictly, as the node decodes them: it once sent "db", which the node
// refused ("unknown field db"), so no recovered database got its full.
func TestRecoverFullParamsDecode(t *testing.T) {
	strict := func(v any) (node.BackupNowParams, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		var p node.BackupNowParams
		return p, dec.Decode(&p)
	}
	p, err := strict(recoverFullParams("app"))
	if err != nil {
		t.Fatalf("the recover path's backup request does not decode: %v", err)
	}
	if p.Database != "app" || p.Type != backup.TypeFull {
		t.Errorf("params %+v", p)
	}
	if _, err := strict(map[string]any{"db": "app", "type": "full"}); err == nil {
		t.Error("the strict decode accepts the old db field, so this test proves nothing")
	}
}

func TestRecoverInspectionLineShowsApplicationNotReady(t *testing.T) {
	line := recoverLine("app", map[string]any{
		"phase": "ready", "restoring": "app__restoring",
		"access_notice": "The read-only restore is ready for inspection; application access is not ready.",
	})
	if !strings.Contains(line, "ready to inspect as app__restoring") || !strings.Contains(line, "application access is not ready") {
		t.Fatalf("inspection readiness was not explicit: %s", line)
	}
}

// db.recover carries what an apply carries: the recovered database's steps
// run in the job, and a rebuilt master has no service password files.
func TestRecoverSecrets(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	creds := contexts.Credentials{
		Database: &contexts.DatabaseCreds{Admin: contexts.Role{User: "vaire_admin", Password: "admin-pw"}, Services: map[string]contexts.Role{}},
		Metrics:  &contexts.MetricsCreds{Username: "metrics", Password: "metrics-pw"},
		Backup:   &contexts.BackupCreds{AccessKeyID: "AKID", SecretAccessKey: "s3cret", Bucket: "vaire-backups-test", Prefix: "mbvlabs"},
	}
	creds.Backup.PgBackRest.CipherPass = "cipher"
	creds.Backup.Dumps.AgeIdentity, creds.Backup.Dumps.AgeRecipient = "AGE-SECRET-KEY-1", "age1xyz"
	for _, role := range steps.ServiceRoles {
		creds.Database.Services[role] = contexts.Role{User: role, Password: role + "-pw"}
	}
	if err := contexts.SaveCredentials("dr", creds); err != nil {
		t.Fatal(err)
	}
	values, generated, err := recoverSecrets("dr")
	if err != nil {
		t.Fatal(err)
	}
	if len(generated) != 0 {
		t.Errorf("generated %v from a complete context", generated)
	}
	want := map[string]string{
		steps.SecretPgbouncerAuth: "pgbouncer_auth-pw", steps.SecretMonitor: "vaire_monitor-pw", steps.SecretAdminPassword: "admin-pw",
		steps.SecretBackupKeyID: "AKID", steps.SecretBackupKeySecret: "s3cret", steps.SecretBackupCipherPass: "cipher",
		steps.SecretBackupAgeRecipient: "age1xyz",
	}
	for k, v := range want {
		if values[k] != v {
			t.Errorf("%s = %q, want %q", k, values[k], v)
		}
	}
	if !strings.HasPrefix(values[steps.SecretMetricsHash], "$2") {
		t.Errorf("no /metrics hash: %q", values[steps.SecretMetricsHash])
	}
	if _, ok := values[steps.SecretBackupAgeIdentity]; ok {
		t.Error("the age identity never leaves the operator outside a dump restore")
	}
}

// A recovered follower's job result carries its structured warnings under
// "warnings"; they must reach the standard warning list with their original
// codes, and the human summary must not say only that it follows again when
// part of finishing the recovery failed (Phase 6, B-03).
func TestRecoverPreservesFollowerJobWarnings(t *testing.T) {
	raw := `{
		"source": "app", "follows": "app", "name": "app_follower", "port": 5434, "from": "stanza",
		"warnings": [
			{"code": "pooler_reload_failed", "message": "the poolers were not reloaded, so app_follower is not reachable through them until vaire apply: timeout"},
			{"code": "follower_not_streaming", "message": "app_follower is not streaming from app yet: vaire doctor --db app_follower and vaire logs postgres --db app_follower show why"},
			{"code": "follower_metrics_failed", "message": "the follower metrics were not written: bad path"}
		]
	}`
	var res map[string]any
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatal(err)
	}
	w := recoverWarnings("app_follower", res)
	if len(w) != 3 {
		t.Fatalf("warnings %+v, want the three follower job warnings", w)
	}
	for i, code := range []string{"pooler_reload_failed", "follower_not_streaming", "follower_metrics_failed"} {
		if w[i].Code != code || w[i].Message == "" {
			t.Errorf("warning %d = %+v, want code %s", i, w[i], code)
		}
	}
	line := recoverLine("app_follower", res)
	if !strings.Contains(line, "following app again (stanza)") || !strings.Contains(line, "3 part(s) of finishing it failed") {
		t.Errorf("line %q does not qualify the follower's completion", line)
	}

	clean := `{"source": "app", "follows": "app", "name": "app_follower", "port": 5434, "from": "stanza", "warnings": []}`
	if err := json.Unmarshal([]byte(clean), &res); err != nil {
		t.Fatal(err)
	}
	if w := recoverWarnings("app_follower", res); len(w) != 0 {
		t.Errorf("clean warnings %+v", w)
	}
	if got := recoverLine("app_follower", res); got != "app_follower: following app again (stanza)" {
		t.Errorf("clean line %q", got)
	}
}

// A failure after the data is back is on the database's line and among the
// warnings, including the promote's own warning, nested in its result.
func TestRecoverSummaryReportsWhatFailed(t *testing.T) {
	clean := map[string]any{"phase": "recovered", "recovered_to": "2026-10-15T09:30:00Z", "duration_s": float64(42),
		"promoted": map[string]any{"database": "app"}}
	if got := recoverLine("app", clean); got != "app: recovered to 2026-10-15T09:30:00Z in 42s" {
		t.Errorf("clean line %q", got)
	}
	if w := recoverWarnings("app", clean); len(w) != 0 {
		t.Errorf("clean warnings %+v", w)
	}
	failed := map[string]any{"phase": "recovered", "duration_s": float64(42),
		"steps_warning": "the data is recovered, but its configuration did not finish: step postgres-roles-v1@app failed: x; vaire apply finishes it",
		"promoted":      map[string]any{"warning": map[string]any{"code": "after_cutover_failed", "message": "app is restored, but y"}}}
	w := recoverWarnings("app", failed)
	if len(w) != 2 || w[0].Code != "after_cutover_failed" || w[1].Code != "recover_steps" || !strings.HasPrefix(w[1].Message, "app: the data is recovered") {
		t.Errorf("warnings %+v", w)
	}
	if got := recoverLine("app", failed); !strings.Contains(got, "2 part(s) of finishing it failed") {
		t.Errorf("failed line %q", got)
	}
}
