package cli

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/transport"
)

// tunnelHint names the cause: the daemon not running, the daemon not
// answering, or a dial that timed out after a real handshake window. Only the
// last, a real timeout, mentions break-glass.
func TestTunnelHint(t *testing.T) {
	wrap := func(inner error) error {
		return output.WrapError(output.CodeRemoteUnreachable,
			fmt.Errorf("cannot reach 10.42.0.1:22: %w", inner), output.ExitTransport, "")
	}
	cases := []struct {
		name    string
		inner   error
		want    string
		notWant string
	}{
		{"no daemon", &netOpError{syscall.ECONNREFUSED}, "tunnel daemon isn't running", "break-glass"},
		{"no answer", fmt.Errorf("%w: i/o timeout", transport.ErrDaemonNoAnswer), "did not reply", "break-glass"},
		{"handshake timeout", &transport.TunnelDialError{Addr: "10.42.0.1:22", Message: "i/o timeout"}, "UDP 51820", ""},
	}
	for _, c := range cases {
		err := tunnelHint(wrap(c.inner), "203.0.113.5", 51820)
		var cliErr *output.CLIError
		if !errors.As(err, &cliErr) || cliErr.Code != output.CodeTransportFailed {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !strings.Contains(cliErr.Hint, c.want) {
			t.Errorf("%s: hint %q lacks %q", c.name, cliErr.Hint, c.want)
		}
		if c.notWant != "" && strings.Contains(cliErr.Hint, c.notWant) {
			t.Errorf("%s: hint %q should not mention %q", c.name, cliErr.Hint, c.notWant)
		}
	}
}

// netOpError wraps a syscall errno the way a net dial does, so IsNoDaemon
// sees it.
type netOpError struct{ err error }

func (e *netOpError) Error() string { return "dial unix: " + e.err.Error() }
func (e *netOpError) Unwrap() error { return e.err }
