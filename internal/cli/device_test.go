package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/mbvlabs/vaire/internal/rpc"
)

type deviceRevokeCaller struct {
	params   []rpc.DeviceRevokeParams
	confirms []string
	target   rpc.Device
}

func (c *deviceRevokeCaller) Call(_ context.Context, method string, params any, opts remote.CallOptions) (remote.Reply, error) {
	if method != "device.revoke" {
		panic(method)
	}

	p, ok := params.(rpc.DeviceRevokeParams)
	if !ok {
		panic("unexpected device revoke parameters")
	}

	c.params = append(c.params, p)
	c.confirms = append(c.confirms, opts.Confirm)
	data, _ := json.Marshal(c.target)

	return remote.Reply{Data: data}, nil
}

func TestDeviceRevokePinsConfirmedID(t *testing.T) {
	for _, tc := range []struct {
		name, confirm string
		target        rpc.Device
		calls         int
		fail          bool
	}{
		{"confirmed", "dev-123", rpc.Device{ID: "dev-123", User: "alice", Label: "laptop", Status: "revoked", SetupCode: "must-not-leak"}, 2, false},
		{"wrong confirmation", "dev-other", rpc.Device{ID: "dev-123", User: "alice", Label: "laptop"}, 1, true},
		{"mismatched target", "dev-123", rpc.Device{ID: "dev-123", User: "bob", Label: "laptop"}, 1, true},
		{"missing ID", "", rpc.Device{User: "alice", Label: "laptop"}, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := NewRoot()

			cmd, _, err := root.Find([]string{"device", "revoke"})
			if err != nil {
				t.Fatal(err)
			}

			if err := cmd.Flags().Set("confirm", tc.confirm); err != nil {
				t.Fatal(err)
			}

			var out bytes.Buffer
			cmd.SetOut(&out)

			caller := &deviceRevokeCaller{target: tc.target}

			err = revokeDevice(cmd, newProgress(cmd), caller, "alice", "laptop", nil)
			if (err != nil) != tc.fail || len(caller.params) != tc.calls {
				t.Fatalf("err=%v calls=%v", err, caller.params)
			}

			checkDeviceRevokeCalls(t, caller)

			if strings.Contains(out.String(), "must-not-leak") {
				t.Fatal("setup code leaked from revoke")
			}
		})
	}
}

func checkDeviceRevokeCalls(t *testing.T, caller *deviceRevokeCaller) {
	t.Helper()

	if caller.params[0].ID != "" || caller.confirms[0] != "" {
		t.Fatal("first call must resolve without confirmation")
	}

	if len(caller.params) == 2 && (caller.params[1].ID != "dev-123" || caller.confirms[1] != "dev-123") {
		t.Fatalf("confirmation not pinned: %+v %v", caller.params, caller.confirms)
	}
}

func TestDeviceCommandsHelp(t *testing.T) {
	for _, args := range [][]string{{"device", "--help"}, {"device", "add", "--help"}, {"device", "list", "--help"}, {"device", "revoke", "--help"}, {"user", "revoke", "--help"}} {
		root := NewRoot()
		root.SetOut(&bytes.Buffer{})
		root.SetArgs(args)

		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

func TestDeviceAddRejectsQuietHumanOutput(t *testing.T) {
	root := NewRoot()
	root.SetOut(&bytes.Buffer{})
	root.SetArgs([]string{"device", "add", "alice", "laptop", "--quiet"})

	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "hide the one-time setup code") {
		t.Fatalf("expected refusal before issuing a hidden code, got %v", err)
	}
}
