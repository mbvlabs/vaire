package steps

import (
	"os"
	"slices"
	"testing"

	"github.com/mbvlabs/eru/internal/backup"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
)

func TestPgBackRestSkipReason(t *testing.T) {
	none := config.Cluster{}
	if got := (pgbackrestStep{}).SkipReason(none); got != "backups_not_configured" {
		t.Errorf("no [backup]: SkipReason = %q, want backups_not_configured", got)
	}
	// server init and backup setup on a server without databases still run
	// the step, so the node stores the bucket keys and cipher pass before the
	// first db create needs them.
	empty := config.Cluster{}
	empty.Backup.Provider = "r2"
	if got := (pgbackrestStep{}).SkipReason(empty); got != "" {
		t.Errorf("backups on, no databases: SkipReason = %q, want the step to run", got)
	}
	withDB := empty
	withDB.Clusters = []config.ClusterSpec{{Name: "app", Stanza: "app-1"}}
	if got := (pgbackrestStep{}).SkipReason(withDB); got != "" {
		t.Errorf("backups on, one database: SkipReason = %q, want the step to run", got)
	}
}

// onDisk reports whether this machine has a node's backup files, which
// resolveBackupValues reads before the request.
func onDisk() bool {
	for _, p := range []string{BackupEnvPath, backup.ConfPath} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

func TestResolveBackupValuesFromRequest(t *testing.T) {
	if onDisk() {
		t.Skip("this machine has a node's backup files")
	}
	_, missing, err := resolveBackupValues(engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{SecretBackupKeyID, SecretBackupKeySecret, SecretBackupCipherPass}
	if !slices.Equal(missing, want) {
		t.Errorf("nothing on disk or in the request: missing = %v, want %v", missing, want)
	}
	rt := engine.Runtime{SecretValues: map[string]string{
		SecretBackupKeyID:        "AKID",
		SecretBackupKeySecret:    "s3cret",
		SecretBackupCipherPass:   "cipher",
		SecretBackupAgeRecipient: "age1xyz",
	}}
	v, missing, err := resolveBackupValues(rt)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) > 0 {
		t.Errorf("request carries the kit: missing = %v", missing)
	}
	if v.KeyID != "AKID" || v.Secret != "s3cret" || v.CipherPass != "cipher" || v.Recipient != "age1xyz" {
		t.Errorf("values = %+v, want the request's", v)
	}
}
