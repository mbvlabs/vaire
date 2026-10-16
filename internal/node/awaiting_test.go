package node

import (
	"errors"
	"testing"

	"github.com/mbvlabs/eru/internal/backup"
	"github.com/mbvlabs/eru/internal/cli/output"
)

// isAwaitingErr reports whether err is the refusal for a database awaiting
// recovery.
func isAwaitingErr(err error) bool {
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) || cliErr.Code != output.CodePrecondition {
		return false
	}
	details, _ := cliErr.Details.(map[string]any)
	reason, _ := details["reason"].(string)
	return reason == "awaiting_recovery"
}

// Every method in awaitingRefused refuses a database awaiting recovery.
// db.write-access is among them: a write role made past the TOTP gate would
// otherwise build on a cluster the server doesn't have.
func TestAwaitingRefusedMethods(t *testing.T) {
	recs := backup.Records{AwaitingRecovery: map[string]backup.Awaiting{"app": {}}}
	if !isAwaitingErr(refuseAwaitingIn(map[string]any{"db": "app"}, recs, "writing to it")) {
		t.Fatal("db.write-access must refuse a database awaiting recovery")
	}
	for method, action := range awaitingRefused {
		// refuseAwaitingIn checks every name field, so fill them all.
		param := map[string]any{"db": "app", "database": "app", "name": "app", "source": "app"}
		if err := refuseAwaitingIn(param, recs, action); !isAwaitingErr(err) {
			t.Errorf("%s: got %v, want awaiting_recovery", method, err)
		}
	}
}

// A method that never needs a database's cluster, and a database that isn't
// awaiting, are left alone.
func TestAwaitingNotRefused(t *testing.T) {
	for _, method := range []string{"db.drop", "config.set", "db.recover"} {
		if _, ok := awaitingRefused[method]; ok {
			t.Errorf("%s must not be refused while its database awaits recovery", method)
		}
	}
	recs := backup.Records{AwaitingRecovery: map[string]backup.Awaiting{"app": {}}}
	if err := refuseAwaitingIn(map[string]any{"db": "other"}, recs, "a backup"); err != nil {
		t.Fatalf("other: got %v", err)
	}
	if err := refuseAwaitingIn(nil, recs, "a backup"); err != nil {
		t.Fatalf("no params: got %v", err)
	}
}
