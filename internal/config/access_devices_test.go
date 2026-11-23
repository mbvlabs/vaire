package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func accessTestDevice(n byte) AccessDevice {
	seed := make([]byte, ed25519.SeedSize)
	seed[0] = n

	pub, err := ssh.NewPublicKey(ed25519.NewKeyFromSeed(seed).Public())
	if err != nil {
		panic(err)
	}

	wg := make([]byte, 32)
	wg[0] = n

	return AccessDevice{
		ID: fmt.Sprintf("dev-%d", n), OperatorID: "op-1", Label: fmt.Sprintf("device-%d", n),
		State: DevicePending, SSHPublicKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))),
		WGPublicKey: base64.StdEncoding.EncodeToString(wg), Address: fmt.Sprintf("10.0.0.%d", n),
		UnixLogin: fmt.Sprintf("vdev-%d", n), RequestDigest: fmt.Sprintf("%064x", n),
	}
}

func TestAccessRegistryRoundTrip(t *testing.T) {
	var r AccessRegistry
	if err := r.AddOperator(AccessOperator{ID: "op-1", Name: "alice", Role: AccessRoleOwner}); err != nil {
		t.Fatal(err)
	}

	for i, state := range []string{DevicePending, DeviceActive, DeviceRevoked} {
		d := accessTestDevice(byte(i + 1))
		d.State = state

		if err := r.AddDevice(d); err != nil {
			t.Fatal(err)
		}
	}

	raw, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}

	got, err := DecodeAccessRegistry(raw)
	if err != nil || !reflect.DeepEqual(got, r) {
		t.Fatalf("round trip: got %#v, err %v", got, err)
	}

	if _, err := DecodeAccessRegistry(append(raw, []byte("\nunknown = true\n")...)); err == nil {
		t.Fatal("accepted unknown field")
	}
}

func TestAccessRegistryRejectsCollisions(t *testing.T) {
	first := accessTestDevice(1)
	first.State = DeviceRevoked
	base := AccessRegistry{Operators: []AccessOperator{{ID: "op-1", Name: "alice", Role: AccessRoleOwner}}, Devices: []AccessDevice{first}}
	tests := map[string]func(*AccessDevice){
		"id":                         func(d *AccessDevice) { d.ID = first.ID },
		"ssh with different comment": func(d *AccessDevice) { d.SSHPublicKey = first.SSHPublicKey + " another-comment" },
		"wireguard":                  func(d *AccessDevice) { d.WGPublicKey = first.WGPublicKey },
		"address":                    func(d *AccessDevice) { d.Address = first.Address },
		"login":                      func(d *AccessDevice) { d.UnixLogin = first.UnixLogin },
		"request":                    func(d *AccessDevice) { d.RequestDigest = first.RequestDigest },
	}

	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			r := base
			d := accessTestDevice(2)
			change(&d)

			if err := r.AddDevice(d); err == nil {
				t.Fatal("collision accepted")
			}

			if !reflect.DeepEqual(r, base) {
				t.Fatal("failed addition changed registry")
			}
		})
	}

	for _, op := range []AccessOperator{
		{ID: "op-1", Name: "bob", Role: AccessRoleMember},
		{ID: "op-2", Name: "alice", Role: AccessRoleMember},
	} {
		r := base
		if err := r.AddOperator(op); err == nil || !reflect.DeepEqual(r, base) {
			t.Fatal("duplicate operator accepted or registry changed")
		}
	}
}

func TestAccessRegistryLabelReuseRequiresRevocation(t *testing.T) {
	first, replacement := accessTestDevice(1), accessTestDevice(2)
	replacement.Label = first.Label

	r := AccessRegistry{
		Operators: []AccessOperator{{ID: "op-1", Name: "alice", Role: AccessRoleMember}},
		Devices:   []AccessDevice{first},
	}
	if err := r.AddDevice(replacement); err == nil {
		t.Fatal("replaced a pending device")
	}

	r.Devices[0].State = DeviceRevoked
	if err := r.AddDevice(replacement); err != nil {
		t.Fatal(err)
	}

	if len(r.Devices) != 2 || r.Devices[0].ID == r.Devices[1].ID {
		t.Fatal("lost the original device identity")
	}
}

func TestAccessRegistryRejectsInvalidDevice(t *testing.T) {
	tests := map[string]func(*AccessDevice){
		"missing operator": func(d *AccessDevice) { d.OperatorID = "missing" },
		"invalid id":       func(d *AccessDevice) { d.ID = "../bad" },
		"blank label":      func(d *AccessDevice) { d.Label = " " },
		"control label":    func(d *AccessDevice) { d.Label = "device\x1b[0m" },
		"uppercase label":  func(d *AccessDevice) { d.Label = "Laptop" },
		"invalid state":    func(d *AccessDevice) { d.State = "unknown" },
		"malformed ssh":    func(d *AccessDevice) { d.SSHPublicKey = "ssh-ed25519 bad" },
		"ignored ssh line": func(d *AccessDevice) { d.SSHPublicKey = "invalid\n" + d.SSHPublicKey },
		"ssh option":       func(d *AccessDevice) { d.SSHPublicKey = "restrict " + d.SSHPublicKey },
		"extra ssh key":    func(d *AccessDevice) { d.SSHPublicKey += "\n" + d.SSHPublicKey },
		"invalid wg":       func(d *AccessDevice) { d.WGPublicKey = base64.StdEncoding.EncodeToString(make([]byte, 32)) },
		"ipv6":             func(d *AccessDevice) { d.Address = "::1" },
		"invalid login":    func(d *AccessDevice) { d.UnixLogin = "root\nbad" },
		"root login":       func(d *AccessDevice) { d.UnixLogin = "root" },
		"admin login":      func(d *AccessDevice) { d.UnixLogin = "vaire" },
		"empty suffix":     func(d *AccessDevice) { d.UnixLogin = "vdev-" },
		"uppercase login":  func(d *AccessDevice) { d.UnixLogin = "vdev-A" },
		"invalid digest":   func(d *AccessDevice) { d.RequestDigest = "bad" },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			d := accessTestDevice(1)
			change(&d)

			r := AccessRegistry{Operators: []AccessOperator{{ID: "op-1", Name: "alice", Role: AccessRoleMember}}}
			if err := r.AddDevice(d); err == nil {
				t.Fatal("invalid device accepted")
			}
		})
	}

	if err := (AccessRegistry{Operators: []AccessOperator{{ID: "op", Name: "alice", Role: "admin"}}}).Validate(); err == nil {
		t.Fatal("invalid role accepted")
	}
}

func TestAccessOperatorName(t *testing.T) {
	for _, name := range []string{"Alice", "alice\x1b[0m", "alice bob", "-alice", strings.Repeat("a", 64)} {
		r := AccessRegistry{}
		if err := r.AddOperator(AccessOperator{ID: "op", Name: name, Role: AccessRoleMember}); err == nil {
			t.Fatalf("accepted operator name %q", name)
		}
	}

	r := AccessRegistry{}
	if err := r.AddOperator(AccessOperator{ID: "op", Name: "alice.dev_1", Role: AccessRoleMember}); err != nil {
		t.Fatal(err)
	}
}
