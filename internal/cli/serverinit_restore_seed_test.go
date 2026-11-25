package cli

import (
	"encoding/json"
	"maps"
	"testing"

	"github.com/mbvlabs/vaire/internal/masterbackup"
	"github.com/mbvlabs/vaire/internal/pg"
	"github.com/mbvlabs/vaire/internal/workspaces"
)

func TestRequireRestoreClusterCredentials(t *testing.T) {
	creds := &workspaces.DatabaseCreds{
		Admin: workspaces.Role{Password: "admin"},
		Services: map[string]workspaces.Role{
			"pgbouncer_auth": {Password: "auth"},
			"vaire_monitor":  {Password: "monitor"},
		},
	}

	if err := requireRestoreClusterCredentials(creds); err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"admin", "pgbouncer_auth", "vaire_monitor"} {
		changed := *creds
		changed.Services = maps.Clone(creds.Services)

		if key == "admin" {
			changed.Admin.Password = ""
		} else {
			delete(changed.Services, key)
		}

		if err := requireRestoreClusterCredentials(&changed); err == nil {
			t.Errorf("accepted missing %s", key)
		}
	}

	if err := requireRestoreClusterCredentials(nil); err == nil {
		t.Fatal("accepted missing credentials")
	}
}

func TestCheckRestoreClusterCredentials(t *testing.T) {
	values := map[string]string{"admin": "admin-pw", "pgbouncer_auth": "auth-pw", "monitor": "monitor-pw"}
	verifiers := map[string]string{}

	for key, password := range values {
		value, err := pg.SCRAMVerifier(password)
		if err != nil {
			t.Fatal(err)
		}

		verifiers[key] = value
	}

	raw, err := json.Marshal(verifiers)
	if err != nil {
		t.Fatal(err)
	}

	bundle := masterbackup.Bundle{Files: []masterbackup.File{masterbackup.NewFile(masterbackup.ClusterSeedPath, "root", "root", 0o600, raw)}}
	creds := &workspaces.DatabaseCreds{Admin: workspaces.Role{Password: values["admin"]}, Services: map[string]workspaces.Role{
		"pgbouncer_auth": {Password: values["pgbouncer_auth"]},
		"vaire_monitor":  {Password: values["monitor"]},
	}}

	if err := checkRestoreClusterCredentials(bundle, creds); err != nil {
		t.Fatal(err)
	}

	creds.Admin.Password = "wrong"
	if err := checkRestoreClusterCredentials(bundle, creds); err == nil {
		t.Fatal("accepted incorrect password")
	}

	if err := checkRestoreClusterCredentials(masterbackup.Bundle{}, nil); err != nil {
		t.Fatalf("old bundle: %v", err)
	}
}
