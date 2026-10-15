package cli

import (
	"testing"

	"github.com/mbvlabs/eru/internal/contexts"
	"github.com/mbvlabs/eru/internal/steps"
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
