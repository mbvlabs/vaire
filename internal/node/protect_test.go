package node

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/rpc"
)

func protectedCluster() config.Cluster {
	return config.Cluster{Clusters: []config.ClusterSpec{
		{Name: "app", Size: "medium", Protected: true},
		{Name: "other", Size: "medium"},
	}}
}

func isProtectedErr(err error) bool {
	var cliErr *output.CLIError
	return errors.As(err, &cliErr) && cliErr.Code == output.CodeDatabaseProtected && cliErr.ExitCode == output.ExitUnsafe
}

func TestCheckNotProtected(t *testing.T) {
	cfg := protectedCluster()
	if err := CheckNotProtected(cfg, "app", "db drop"); !isProtectedErr(err) {
		t.Fatalf("app: got %v, want database_protected", err)
	}
	for _, name := range []string{"other", "missing"} {
		if err := CheckNotProtected(cfg, name, "db drop"); err != nil {
			t.Fatalf("%s: got %v", name, err)
		}
	}
}

func TestRefuseProtected(t *testing.T) {
	called := false
	h := refuseProtected("rotating its credentials", "name", rotateRequested, func(_ context.Context, _ *Env) (Result, error) {
		called = true
		return Result{}, nil
	})
	run := func(params string) error {
		called = false
		_, err := h(context.Background(), &Env{Cluster: protectedCluster(), Req: rpc.Request{Params: json.RawMessage(params)}})
		return err
	}
	if err := run(`{"name":"app","rotate":true}`); !isProtectedErr(err) || called {
		t.Fatalf("rotate app: err %v, called %v", err, called)
	}
	if err := run(`{"name":"app","rotate":false}`); err != nil || !called {
		t.Fatalf("no rotate: err %v, called %v", err, called)
	}
	if err := run(`{"name":"other","rotate":true}`); err != nil || !called {
		t.Fatalf("other: err %v, called %v", err, called)
	}
}

func TestResizeShrinks(t *testing.T) {
	cfg := protectedCluster()
	medium := cfg.Clusters[0]
	small, large := medium, medium
	small.Size, large.Size = "small", "large"
	if !ResizeShrinks(cfg, medium, small) {
		t.Fatal("medium to small should shrink")
	}
	if ResizeShrinks(cfg, medium, large) || ResizeShrinks(cfg, medium, medium) {
		t.Fatal("growing or keeping the size should not shrink")
	}
}

func TestProtectionOf(t *testing.T) {
	protected, all := protectionOf(protectedCluster())
	if len(protected) != 1 || protected[0] != "app" {
		t.Fatalf("protected = %v", protected)
	}
	want := []ProtectedDatabase{{Name: "app", Protected: true}, {Name: "other"}}
	if len(all) != len(want) || all[0] != want[0] || all[1] != want[1] {
		t.Fatalf("databases = %+v, want %+v", all, want)
	}
	protected, all = protectionOf(config.Cluster{})
	if protected == nil || all == nil || len(protected)+len(all) != 0 {
		t.Fatalf("empty cluster: %v %v, want empty, non-nil lists", protected, all)
	}
}
