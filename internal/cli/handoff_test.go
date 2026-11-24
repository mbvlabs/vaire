package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/secretsdoc"
)

func TestOwnerRecoveryHandoffExplainsWhatToKeep(t *testing.T) {
	h := handoff{doc: secretsdoc.Document{RecoveryCode: "test-code"}}

	var out bytes.Buffer
	h.showHeader(handoffTerminal{w: &out})

	for _, want := range []string{"Owner recovery kit", "vaire recover", "one new owner device", "encrypted kit and its passphrase"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in %s", want, out.String())
		}
	}
}
