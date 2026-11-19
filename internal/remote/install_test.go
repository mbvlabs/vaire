package remote

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
)

func TestInstallUpgradeRefusedBeforeRemoteAccess(t *testing.T) {
	var n Node

	_, err := n.InstallUpgrade(context.Background(), Artifact{}, CallOptions{})

	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) || !strings.Contains(cliErr.Hint, "trusted administrator") {
		t.Fatalf("InstallUpgrade error = %v", err)
	}
}

func TestBootstrapInstallDoesNotReplaceExistingBinary(t *testing.T) { //nolint:gocognit // Keep the temporary host setup and both filesystem assertions together.
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")

			if err := os.Mkdir(bin, 0o755); err != nil {
				t.Fatal(err)
			}

			// Avoid requiring root while exercising the actual bootstrap shell.
			install := "#!/bin/sh\nif [ \"$1\" = -o ]; then shift 4; fi\nexec /usr/bin/install \"$@\"\n"
			if err := os.WriteFile(filepath.Join(bin, "install"), []byte(install), 0o755); err != nil {
				t.Fatal(err)
			}

			dst := filepath.Join(bin, "vaire")
			if existing {
				if err := os.WriteFile(dst, []byte("existing"), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			src := filepath.Join(dir, "upload")
			content := []byte("#!/bin/sh\nexit 0\n")

			if err := os.WriteFile(src, content, 0o600); err != nil {
				t.Fatal(err)
			}

			sum := sha256.Sum256(content)
			script := strings.ReplaceAll(bootstrapInstall, "/usr/local/bin", bin)
			cmd := exec.Command("/bin/sh", "-s", "--", src, fmt.Sprintf("%x", sum))
			cmd.Stdin = strings.NewReader(script)
			cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}

			out, err := cmd.CombinedOutput()
			if (err != nil) != existing {
				t.Fatalf("bootstrap error = %v, output = %s", err, out)
			}

			got, err := os.ReadFile(dst)
			if err != nil {
				t.Fatal(err)
			}

			want := string(content)
			if existing {
				want = "existing"
			}

			if string(got) != want {
				t.Fatalf("installed bytes = %q, want %q", got, want)
			}

			if _, err := os.Stat(src); !os.IsNotExist(err) {
				t.Fatalf("upload not removed: %v", err)
			}
		})
	}
}
