package node

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/mbvlabs/eru/internal/backup"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/hostinfo"
	"github.com/mbvlabs/eru/internal/render"
	"github.com/mbvlabs/eru/internal/steps"
)

// The jobs that run steps (db recover, db fork) carry the step secrets of
// the request that starts them, and their steps' runtime has them: before,
// postgres-roles-v1 in a recover job found no service password, and a
// rebuilt master has no service password files to fall back on.
func TestJobStepsGetTheRequestSecrets(t *testing.T) {
	req := map[string]string{
		steps.SecretAdminPassword: "admin", steps.SecretPgbouncerAuth: "auth", steps.SecretMonitor: "mon",
		steps.SecretMetricsHash: "$2a$hash", steps.SecretBackupKeyID: "AKID", steps.SecretBackupKeySecret: "s3cret",
		steps.SecretBackupCipherPass: "cipher", steps.SecretBackupAgeRecipient: "age1xyz",
		// Not a step's: the dump identity and the master bundle stay out.
		steps.SecretBackupAgeIdentity: "AGE-SECRET-KEY-1", steps.SecretMasterBundle: "tar",
	}
	got := stepSecrets(req)
	want := maps.Clone(req)
	delete(want, steps.SecretBackupAgeIdentity)
	delete(want, steps.SecretMasterBundle)
	if !maps.Equal(got, want) {
		t.Errorf("job secrets %v, want %v", got, want)
	}
	if got := stepSecrets(nil); len(got) != 0 {
		t.Errorf("no request secrets: %v", got)
	}
	rt := jobRuntime(&jobEnv{Secrets: got})
	if rt.SecretValues[steps.SecretPgbouncerAuth] != "auth" || rt.SecretValues[steps.SecretMonitor] != "mon" {
		t.Errorf("the job's steps don't see the request secrets: %v", rt.SecretValues)
	}
}

// Recover renders pgbackrest.conf with the database's stanza section before
// it arms archiving: pgbackrest-v1 runs in the view that keeps the database
// awaiting recovery, and that view's conf has the section archive-push needs
// for the relative WAL path (pg1-path). server init --restore rendered it
// with no section.
func TestRecoverRendersTheStanzaBeforeArming(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Backup = config.Backup{Provider: "r2", Endpoint: "https://x.r2.cloudflarestorage.com", Region: "auto", Bucket: "b", Prefix: "prod", URIStyle: "path", Database: cfg.Backup.Database}
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Stanza: "app-abc123", Port: 5433}, {Name: "other", Stanza: "other-def456", Port: 5434}}
	recs := backup.Records{AwaitingRecovery: map[string]backup.Awaiting{"app": {}, "other": {}}}

	opts := stanzaConfOptions(cfg, "app", nil)
	if !slices.Equal(opts.Only, steps.PgBackRestStepIDs()) || opts.Recovering != "app" || opts.Restarter == nil {
		t.Errorf("options %+v", opts)
	}
	host := hostinfo.Info{VCPU: 2}
	before := string(render.PgBackRestConf(withoutAwaiting(cfg, recs, ""), host, render.BackupSecrets{}))
	if strings.Contains(before, "[app-abc123]") {
		t.Fatal("the init view already has the section; the test proves nothing")
	}
	conf := string(render.PgBackRestConf(withoutAwaiting(cfg, recs, opts.Recovering), host, render.BackupSecrets{}))
	for _, line := range []string{"[app-abc123]", "pg1-path=" + cfg.Server.DataDirectory("app"), "pg1-port=5433", "repo1-retention-full="} {
		if !strings.Contains(conf, line) {
			t.Errorf("pgbackrest.conf lacks %q:\n%s", line, conf)
		}
	}
	if strings.Contains(conf, "[other-def456]") {
		t.Error("a database still awaiting recovery got a section")
	}
	// Its queue share counts every database, so the file doesn't change as
	// the others come back.
	full := string(render.PgBackRestConf(cfg, host, render.BackupSecrets{}))
	queue := func(s string) string {
		i := strings.Index(s, "archive-push-queue-max=")
		return s[i : i+strings.Index(s[i:], "\n")]
	}
	if queue(conf) != queue(full) {
		t.Errorf("queue share %s while recovering, %s once all are back", queue(conf), queue(full))
	}
}

// The view leaves out the databases awaiting recovery, but keeps them in
// Awaiting, which the tuning counts.
func TestStepsViewKeepsTheAwaitingAside(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app"}, {Name: "other"}}
	recs := backup.Records{AwaitingRecovery: map[string]backup.Awaiting{"other": {}}}
	got := withoutAwaiting(cfg, recs, "")
	if len(got.Clusters) != 1 || got.Clusters[0].Name != "app" || len(got.Awaiting) != 1 || got.Awaiting[0].Name != "other" {
		t.Errorf("view %v, awaiting %v", got.Clusters, got.Awaiting)
	}
	if len(cfg.Awaiting) != 0 {
		t.Error("the view changed the configuration it was given")
	}
}
