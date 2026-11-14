package backup

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
)

// Source-derived excerpt, not a captured runtime pg_controldata fixture.
const controlExcerpt = `max_connections setting:              30
max_worker_processes setting:         8
max_wal_senders setting:              10
max_prepared_xacts setting:            0
max_locks_per_xact setting:           64
`

func TestControlRequirements(t *testing.T) {
	values, err := parseControlRequirements(controlExcerpt, "")
	if err != nil || values["max_connections"] != 30 || values["max_prepared_transactions"] != 0 || values["max_locks_per_transaction"] != 64 {
		t.Fatalf("values=%v err=%v", values, err)
	}
	for name, tc := range map[string]struct{ stdout, stderr string }{
		"missing":   {strings.Replace(controlExcerpt, "max_connections setting:", "Max connections setting:", 1), ""},
		"duplicate": {controlExcerpt + "max_connections setting: 30\n", ""},
		"negative":  {strings.Replace(controlExcerpt, "30", "-1", 1), ""},
		"suffix":    {strings.Replace(controlExcerpt, "30", "30 MB", 1), ""},
		"zero":      {strings.Replace(controlExcerpt, "30", "0", 1), ""},
		"overflow":  {strings.Replace(controlExcerpt, "30", "9999999999999999999999999", 1), ""},
		"crc":       {controlExcerpt, "WARNING: calculated CRC does not match value stored in file"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseControlRequirements(tc.stdout, tc.stderr); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestRecoveryFloorBounds(t *testing.T) {
	for _, floor := range recoveryFloors {
		for _, n := range []int64{floor.minimum, floor.maximum} {
			if _, err := recoveryNumber(fmt.Sprint(n), floor); err != nil {
				t.Fatalf("%s valid %d: %v", floor.setting, n, err)
			}
		}
		for _, n := range []int64{floor.minimum - 1, floor.maximum + 1} {
			if _, err := recoveryNumber(fmt.Sprint(n), floor); err == nil {
				t.Fatalf("%s accepted %d", floor.setting, n)
			}
		}
	}
}

func TestRequirementsFailure(t *testing.T) {
	required, _ := parseControlRequirements(controlExcerpt, "")
	configured, _ := parseControlRequirements(controlExcerpt, "")
	configured["max_connections"] = 50
	if err := RecoveryRequirementsUnmet("target", required, configured); err != nil {
		t.Fatal(err)
	}
	required["max_connections"] = 50
	configured["max_connections"] = 30
	err := RecoveryRequirementsUnmet("target", required, configured)
	var cli *output.CLIError
	if !errors.As(err, &cli) || cli.Code != output.CodePrecondition || cli.ExitCode != output.ExitPrecondition || !RetainRecoveryFailure(fmt.Errorf("wrapper: %w", err)) {
		t.Fatalf("unexpected failure: %#v", err)
	}
	wrapped := output.WrapError(output.CodeExternalCommand, err, output.ExitExternal, "")
	if !RetainRecoveryFailure(wrapped) {
		t.Fatal("outer CLI error hid retained cause")
	}
	delete(configured, "max_wal_senders")
	if err := RecoveryRequirementsUnmet("target", required, configured); err == nil || RetainRecoveryFailure(err) {
		t.Fatalf("missing inspection: %v", err)
	}
}

func TestRecoveryMismatch(t *testing.T) {
	for _, floor := range recoveryFloors {
		log := "2026 [123] WARNING:  hot standby is not possible because of insufficient parameter settings\n" +
			"2026 [123] DETAIL:  " + floor.setting + " = 30 is a lower setting than on the primary server, where its value was 50.\n"
		got, ok := recoveryMismatch(log)
		if !ok || got.Setting != floor.setting || got.Configured != 30 || got.Required != 50 {
			t.Fatalf("%s: %#v %v", floor.setting, got, ok)
		}
		if !RetainRecoveryFailure(recoveryMismatchFailure("target", got)) {
			t.Fatal("mismatch must be retained")
		}
	}
	for _, log := range []string{
		"LOG:  recovery has paused",
		"WARNING:  hot standby is not possible because of insufficient parameter settings\nDETAIL:  secret = 1 is a lower setting than on the primary server, where its value was 2.",
		"WARNING:  hot standby is not possible because of insufficient parameter settings\nLOG: unrelated\nDETAIL: max_connections = 1 is a lower setting than on the primary server, where its value was 2.",
	} {
		if _, ok := recoveryMismatch(log); ok {
			t.Fatalf("incorrect match: %s", log)
		}
	}
}
