package config

import "testing"

func TestOperatorsUpsertKeepsForeignRegistration(t *testing.T) {
	identity := Operator{
		Name:         "machine-abcdefghjkmn",
		WGAddress:    "10.42.255.1",
		WGPublicKey:  "wg-key",
		SSHPublicKey: "ssh-ed25519 AAAA old-comment",
	}
	ops := Operators{Operators: []Operator{identity}}

	foreign := identity
	foreign.SSHPublicKey = "ssh-ed25519 BBBB foreign-comment"

	if err := ops.Upsert(foreign); err == nil {
		t.Fatal("a different identity under the same name was accepted")
	}

	if len(ops.Operators) != 1 || ops.Operators[0] != identity {
		t.Fatalf("foreign registration changed the registry: %+v", ops.Operators)
	}

	renamed := identity
	renamed.SSHPublicKey = "ssh-ed25519 AAAA new-workspace-name"

	if err := ops.Upsert(renamed); err != nil {
		t.Fatalf("the same key under a different SSH comment was refused: %v", err)
	}

	if ops.Operators[0] != renamed {
		t.Fatalf("same identity was not updated: %+v", ops.Operators[0])
	}

	updated := renamed
	updated.WGAddress = "10.42.255.2"

	if err := ops.Upsert(updated); err != nil {
		t.Fatal(err)
	}

	if ops.Operators[0] != updated {
		t.Fatalf("same identity was not updated: %+v", ops.Operators[0])
	}

	other := identity
	other.Name = "machine-0123456789ab"

	if err := ops.Upsert(other); err != nil || len(ops.Operators) != 2 {
		t.Fatalf("new operator was not added: %+v %v", ops.Operators, err)
	}
}
