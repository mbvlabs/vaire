package steps

import (
	"reflect"
	"slices"
	"testing"

	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/hostinfo"
)

// The secrets a job hands its steps are the ones the steps in db recover
// and db fork read: the service passwords (postgres-roles-v1 writes their
// files), the /metrics hash (metrics-v1) and the recovery kit
// (pgbackrest-v1). The dump identity and the master bundle are never a
// step's.
func TestStepSecrets(t *testing.T) {
	for _, role := range ServiceRoles {
		if !slices.Contains(StepSecrets, ServiceRoleSecret(role)) {
			t.Errorf("%s is missing", ServiceRoleSecret(role))
		}
	}
	for _, key := range []string{SecretAdminPassword, SecretMetricsHash, SecretBackupKeyID, SecretBackupKeySecret, SecretBackupCipherPass, SecretBackupAgeRecipient} {
		if !slices.Contains(StepSecrets, key) {
			t.Errorf("%s is missing", key)
		}
	}
	for _, key := range []string{SecretBackupAgeIdentity, SecretMasterBundle} {
		if slices.Contains(StepSecrets, key) {
			t.Errorf("%s is not a step's", key)
		}
	}
}

// pg_file_settings rows that only say a restart is needed aren't a rejected
// configuration: a changed postmaster setting, and one the files no longer
// set, which PostgreSQL reports without a file or name (a promote's dropped
// recovery_target_timeline was one).
func TestRestartOnly(t *testing.T) {
	for _, c := range []struct {
		row  map[string]string
		want bool
	}{
		{map[string]string{"sourcefile": "/etc/postgresql/18/app/conf.d/10-eru-tuning.conf", "name": "shared_buffers", "error": "setting could not be applied", "context": "postmaster"}, true},
		{map[string]string{"error": `parameter "recovery_target_timeline" cannot be changed without restarting the server`}, true},
		{map[string]string{"sourcefile": "/etc/postgresql/18/app/conf.d/50-eru-settings.conf", "sourceline": "3", "name": "work_mem", "error": "setting could not be applied", "context": "user"}, false},
		{map[string]string{"sourcefile": "/etc/postgresql/18/app/conf.d/50-eru-settings.conf", "sourceline": "4", "name": "bogus", "error": "unrecognized configuration parameter"}, false},
	} {
		if got := restartOnly(c.row); got != c.want {
			t.Errorf("%v: %v, want %v", c.row, got, c.want)
		}
	}
}

// A database recovered first is tuned as it will run once the others are
// back: the CPU and disk shares count the databases still awaiting recovery,
// so its postgres-config-v1 doesn't change (or wait for a restart) when they
// come back.
func TestTuningCountsTheDatabasesAwaitingRecovery(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Size: "small"}, {Name: "other", Size: "small"}}
	host := hostinfo.Info{VCPU: 16, DataDisk: hostinfo.Disk{SizeGiB: 400}}
	all := tuningFor(cfg, cfg.Clusters[0], host)
	view := cfg
	view.Clusters, view.Awaiting = cfg.Clusters[:1], cfg.Clusters[1:]
	if got := tuningFor(view, view.Clusters[0], host); !reflect.DeepEqual(got, all) {
		t.Errorf("tuning while other awaits recovery:\n%+v\nonce it is back:\n%+v", got, all)
	}
	alone := cfg
	alone.Clusters = cfg.Clusters[:1]
	if reflect.DeepEqual(tuningFor(alone, alone.Clusters[0], host), all) {
		t.Error("one database alone tunes as two; the test proves nothing")
	}
}
