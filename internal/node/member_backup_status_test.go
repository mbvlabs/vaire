package node

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/rpc"
)

func memberBackupFixture(t *testing.T) *Env {
	t.Helper()

	old := backup.RecordsPath
	backup.RecordsPath = filepath.Join(t.TempDir(), "records.json")
	t.Cleanup(func() { backup.RecordsPath = old })

	cfg := config.DefaultCluster()
	cfg.Backup.Provider = "s3"
	cfg.Clusters = []config.ClusterSpec{
		{Name: "mine", Stanza: "mine-abcdef", CreatorOperatorID: "op-member"},
		{Name: "theirs", Stanza: "theirs-abcdef", CreatorOperatorID: "op-other"},
		{Name: "legacy", Stanza: "legacy-abcdef"},
		{Name: "reused", Stanza: "reused-new", CreatorOperatorID: "op-member"},
		{Name: "duplicate", CreatorOperatorID: "op-member"},
		{Name: "duplicate", CreatorOperatorID: "op-member"},
	}

	return &Env{Cluster: cfg, DeviceIdentity: &rpc.WhoamiReply{
		IdentityKind: rpc.IdentityVerifiedDevice, Role: config.AccessRoleMember, OperatorID: "op-member",
	}}
}

func writeMemberRecords(t *testing.T, records backup.Records) {
	t.Helper()

	raw, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(backup.RecordsPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMemberBackupStatusOnly(t *testing.T) {
	env := memberBackupFixture(t)
	writeMemberRecords(t, backup.Records{
		Repo: map[string]backup.RepoFacts{
			"mine":   {Stanza: "mine-abcdef", Sets: 3, WindowStart: "/secret-path"},
			"theirs": {Stanza: "theirs-abcdef", Sets: 9},
			"reused": {Stanza: "reused-old", Sets: 17},
		},
		Restores:       map[string]backup.Restore{"mine": {Error: "/secret-error"}},
		DroppedStanzas: []backup.DroppedStanza{{Database: "dropped"}},
	})

	for _, tc := range []struct {
		db       string
		contains []string
	}{
		{"", []string{"mine", "available", "reused", "unknown"}},
		{"mine", []string{"mine", "available", `"backup_count":3`}},
		{"reused", []string{"reused", "unknown", `"backup_count":0`}},
	} {
		env.Req = request("backup.list", map[string]any{"database": tc.db})

		res, err := handleBackupList(context.Background(), env)
		if err != nil {
			t.Fatal(err)
		}

		raw, err := json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}

		for _, part := range tc.contains {
			if !strings.Contains(string(raw), part) {
				t.Fatalf("%s: missing %q: %s", tc.db, part, raw)
			}
		}

		for _, secret := range []string{"theirs", "legacy", "duplicate", "dropped", "secret", "mine-abcdef", "reused-old"} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("%s: leaked %q: %s", tc.db, secret, raw)
			}
		}
	}

	for _, db := range []string{"theirs", "legacy", "duplicate", "missing"} {
		env.Req = request("backup.list", map[string]any{"database": db})
		if _, err := handleBackupList(context.Background(), env); err == nil {
			t.Fatalf("permitted %s", db)
		}
	}

	env.DeviceIdentity.IdentityKind = ""
	env.Req = request("backup.list", map[string]any{"database": "mine"})

	if _, err := handleBackupList(context.Background(), env); err == nil {
		t.Fatal("permitted unverified identity")
	}
}

func TestMemberRestoreStatusOnly(t *testing.T) {
	env := memberBackupFixture(t)
	writeMemberRecords(t, backup.Records{
		Restores: map[string]backup.Restore{
			"mine": {Database: "mine", Phase: backup.PhaseFailed, Error: "/secret-error", Target: "/secret-target",
				Attempt: &backup.MaterialisationAttempt{Stanza: "mine-abcdef", DataPath: "/secret-path"}},
			"theirs": {Database: "theirs", Phase: backup.PhaseReady, Attempt: &backup.MaterialisationAttempt{Stanza: "theirs-abcdef"}},
			"reused": {Database: "reused", Phase: backup.PhaseReady, Attempt: &backup.MaterialisationAttempt{Stanza: "reused-old"}},
		},
	})

	env.Req = request("restore.status", nil)

	res, err := handleRestoreStatus(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(raw), `"database":"mine"`) || !strings.Contains(string(raw), `"phase":"failed"`) {
		t.Fatalf("missing own status: %s", raw)
	}

	for _, secret := range []string{"theirs", "reused", "secret", "mine-abcdef"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("leaked %q: %s", secret, raw)
		}
	}

	env.Req = request("restore.status", map[string]any{"database": "theirs"})
	if _, err := handleRestoreStatus(context.Background(), env); err == nil {
		t.Fatal("permitted another operator's restore")
	}

	// The administrative contract retains the full record.
	env.DeviceIdentity = nil
	env.Req = request("restore.status", map[string]any{"database": "mine"})

	admin, err := handleRestoreStatus(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}

	raw, err = json.Marshal(admin)
	if err != nil || !strings.Contains(string(raw), "/secret-error") {
		t.Fatalf("admin record changed: %s, %v", raw, err)
	}
}

func TestMemberStatusSuppressesRecordErrors(t *testing.T) {
	env := memberBackupFixture(t)
	if err := os.WriteFile(backup.RecordsPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, method := range []string{"backup.list", "restore.status"} {
		env.Req = request(method, map[string]any{"database": "mine"})

		var err error
		if method == "backup.list" {
			_, err = handleBackupList(context.Background(), env)
		} else {
			_, err = handleRestoreStatus(context.Background(), env)
		}

		if err == nil || strings.Contains(err.Error(), backup.RecordsPath) {
			t.Fatalf("%s: unsafe error: %v", method, err)
		}
	}
}
