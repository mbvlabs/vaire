package node

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
)

func TestUpgradeInstallRefused(t *testing.T) {
	_, err := handleUpgradeInstall(context.Background(), nil)

	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) || !strings.Contains(cliErr.Hint, "trusted administrator") {
		t.Fatalf("upgrade.install error = %v", err)
	}
}
