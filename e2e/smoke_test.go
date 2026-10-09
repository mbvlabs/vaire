//go:build e2e

package e2e_test

import (
	"encoding/json"
	"testing"

	"github.com/mbvlabs/eru/e2e/harness"
)

func TestSmokeVMVersion(t *testing.T) {
	env := harness.StartServices(t)
	if env.MinIOBucket == "" || env.PebbleDirectory == "" || env.MinIOSecretKey == "" {
		t.Fatalf("services were not bootstrapped: %+v", env)
	}
	vm := harness.NewVM(t, harness.Image())
	if err := vm.Push(harness.NodeBinary(t, vm), "/usr/local/bin/eru", "0755"); err != nil {
		t.Fatal(err)
	}
	out, err := vm.Exec("/usr/local/bin/eru", "version", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	got := harness.ParseEnvelope(t, out)
	if !got.OK || got.Schema != "eru/v1" || got.Command != "version" {
		t.Fatalf("vm version: %s", out)
	}
	harness.Snapshot(vm)
	harness.Restore(vm)
	out, err = vm.Exec("/usr/local/bin/eru", "version", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	again := harness.ParseEnvelope(t, out)
	if !again.OK {
		t.Fatalf("version after restore: %s", out)
	}
	var first, second map[string]string
	if err := json.Unmarshal(got.Data, &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(again.Data, &second); err != nil {
		t.Fatal(err)
	}
	if first["version"] == "" || first["version"] != second["version"] {
		t.Fatalf("versions %q %q", first["version"], second["version"])
	}
}
