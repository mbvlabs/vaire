package cli

import (
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/contexts"
	"github.com/mbvlabs/vaire/internal/steps"
	"golang.org/x/crypto/bcrypt"
)

func TestMetricsSecretTopsUpOnce(t *testing.T) {
	var creds *contexts.MetricsCreds
	rpc := map[string]string{}
	generated, err := addMetricsSecret(&creds, rpc)
	if err != nil || !generated {
		t.Fatalf("first top-up: generated %v, %v", generated, err)
	}
	if creds.Username != "metrics" || len(creds.Password) != 32 {
		t.Fatalf("creds %+v", creds)
	}
	hash := rpc[steps.SecretMetricsHash]
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(creds.Password)); err != nil {
		t.Fatalf("the hash does not verify the password: %v", err)
	}
	if cost, _ := bcrypt.Cost([]byte(hash)); cost != metricsBcryptCost {
		t.Errorf("cost %d", cost)
	}
	password := creds.Password
	generated, err = addMetricsSecret(&creds, rpc)
	if err != nil || generated || creds.Password != password {
		t.Errorf("second top-up regenerated: %v %v", generated, err)
	}
	for _, v := range rpc {
		if v == password {
			t.Error("the plaintext password is in the RPC secrets")
		}
	}
}

func TestMetricsDocument(t *testing.T) {
	if metricsDocument(nil, "db1.example.com", "") != nil {
		t.Error("no credential, no section")
	}
	m := metricsDocument(&contexts.MetricsCreds{Username: "metrics", Password: "pw"}, "db1.example.com", "")
	if m.BaseURL != "https://db1.example.com/metrics" || m.Password != "pw" {
		t.Errorf("section %+v", m)
	}
	if got := metricsBaseURL("db1.example.com", "/m"); got != "https://db1.example.com/m" {
		t.Errorf("base url %s", got)
	}
}

// An upgrade that only tops up the /metrics credential says so, and the next
// apply generates nothing: the credential is saved in the context.
func TestServiceSecretsNameWhatWasGenerated(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	creds := contexts.Credentials{Database: &contexts.DatabaseCreds{
		Admin:    contexts.Role{User: "vaire_admin", Password: "admin-pw"},
		Services: map[string]contexts.Role{},
	}}
	for _, role := range steps.ServiceRoles {
		creds.Database.Services[role] = contexts.Role{User: role, Password: role + "-pw"}
	}
	if err := contexts.SaveCredentials("phase3", creds); err != nil {
		t.Fatal(err)
	}
	_, _, generated, err := serviceSecrets("phase3")
	if err != nil {
		t.Fatal(err)
	}
	w := staleWarning(generated)
	if len(w) != 1 || w[0].Message != "a /metrics credential was generated; run vaire secrets export to refresh the secrets document" {
		t.Errorf("warning %+v", w)
	}
	_, _, generated, err = serviceSecrets("phase3")
	if err != nil || len(generated) != 0 {
		t.Errorf("second run generated %v, %v", generated, err)
	}
	if staleWarning(nil) != nil {
		t.Error("nothing generated, still a warning")
	}
	if got := staleWarning([]string{"the vaire_admin password", "a /metrics credential"})[0].Message; got != "the vaire_admin password and a /metrics credential were generated; run vaire secrets export to refresh the secrets document" {
		t.Errorf("two items: %q", got)
	}
}

func TestMetricsPasswordNote(t *testing.T) {
	if note := metricsPasswordNote(true, false); !strings.Contains(note, "--reveal") {
		t.Fatalf("hidden password note = %q, want it to name --reveal", note)
	}
	if note := metricsPasswordNote(true, true); note != "" {
		t.Fatalf("revealed password note = %q, want none", note)
	}
	if note := metricsPasswordNote(false, false); note != "" {
		t.Fatalf("no-credential note = %q, want none", note)
	}
}
