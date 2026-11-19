package node

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/state"
)

func TestDelegatedBootstrapRequiresTrustedRoot(t *testing.T) {
	for _, phase := range []string{PhaseHost, PhaseRestore} {
		t.Run(phase, func(t *testing.T) {
			params, err := json.Marshal(bootstrapParams{Phase: phase})
			if err != nil {
				t.Fatal(err)
			}

			_, err = handleClusterBootstrap(t.Context(), &Env{Req: rpc.Request{Params: params}})

			var cliErr *output.CLIError
			if !errors.As(err, &cliErr) || cliErr.Message != "this operation requires a trusted root session" {
				t.Fatalf("delegated bootstrap refusal: %v", err)
			}
		})
	}
}

func TestExposureProposalRejectsDifferentConfigAndRevision(t *testing.T) {
	old := config.DefaultCluster()
	proposed := old
	proposed.Network.ClientAllowlist = []string{"192.0.2.10/32"}

	store := state.FileStore{Path: filepath.Join(t.TempDir(), "cluster.toml")}
	raw := []byte("[network]\nclient_allowlist = [\"192.0.2.10/32\"]\n")
	preview := exposureProposal("revision", raw)

	for _, tc := range []struct {
		name, revision string
		raw            []byte
	}{
		{"different revision", "different", raw},
		{"different config", "revision", append([]byte("# changed\n"), raw...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			save, err := configExposureSave(t.Context(), &Env{}, store, tc.raw, old, proposed,
				configSetParams{ExposureProposal: preview}, tc.revision)

			var cliErr *output.CLIError

			if save != nil || !errors.As(err, &cliErr) || cliErr.Message != "the exposure proposal is missing or no longer matches" {
				t.Fatalf("mismatched proposal: %v", err)
			}
		})
	}
}
