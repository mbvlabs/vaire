package node

import (
	"context"

	"github.com/mbvlabs/vaire/internal/cli/output"
)

// BuildID names one vaire build.
type BuildID struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Channel string `json:"channel"`
}

// InstallResult is retained for clients of the former upgrade.install method.
type InstallResult struct {
	Previous   *BuildID `json:"previous"`
	Installed  BuildID  `json:"installed"`
	RolledBack bool     `json:"rolled_back"`
}

func handleUpgradeInstall(_ context.Context, _ *Env) (Result, error) {
	return Result{}, output.NewError(output.CodePrecondition,
		"node binary installation through RPC is disabled",
		output.ExitPrecondition,
		"Have a trusted administrator install the matching vaire binary on the node.")
}
