//go:build e2e

package e2e_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eru/e2e/harness"
)

const (
	journalPath  = "/var/lib/eru/journal.json"
	logDir       = "/var/log/eru"
	stubSteps    = 5
	restartToken = "e2e/restart"
)

var stubIDs = []string{"stub-file-v1", "stub-secret-v1", "stub-slow-v1", "stub-fail-v1", "stub-restart-v1"}

// TestE26NodeRuntime drives eru node rpc through incus exec with the e2etest
// stub catalogue (M2 spec §9).
func TestE26NodeRuntime(t *testing.T) {
	vm := harness.NewVM(t, harness.Image())
	node := harness.PrepareNode(t, vm)
	harness.Snapshot(vm)

	requestSecret := "request-secret-" + node.Commit + "-e2e"
	var applyRun harness.RPCResult

	t.Run("1 selfcheck", func(t *testing.T) {
		out, code, err := vm.ExecCode(harness.NodeBinaryPath, "node", "selfcheck", "--json")
		if err != nil {
			t.Fatal(err)
		}
		env := harness.ParseEnvelope(t, out)
		if code != 0 || !env.OK {
			t.Fatalf("selfcheck exit %d\n%s", code, out)
		}
		var data struct {
			Checks []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"checks"`
		}
		decode(t, env.Data, &data)
		// Only items M3 provides may warn.
		m3 := map[string]bool{"node_toml": true, "cluster_toml": true}
		for _, c := range data.Checks {
			if c.Status == "fail" || (c.Status == "warn" && !m3[c.Name]) {
				t.Fatalf("check %s is %s\n%s", c.Name, c.Status, out)
			}
		}
	})

	t.Run("2 plan", func(t *testing.T) {
		res := harness.RPC(t, vm, "plan", nil, harness.RPCOptions{})
		mustOK(t, res)
		plan := planData(t, res)
		if plan.Total != stubSteps || plan.WouldApply != stubSteps || len(plan.Steps) != stubSteps {
			t.Fatalf("plan %+v", plan)
		}
		for i, s := range plan.Steps {
			if s.ID != stubIDs[i] || s.Index != i+1 || s.Action != "apply" || s.Reason != "new" {
				t.Fatalf("plan step %d: %+v", i, s)
			}
		}
	})

	t.Run("3 apply", func(t *testing.T) {
		applyRun = harness.RPC(t, vm, "apply", nil, harness.RPCOptions{
			Secrets: map[string]string{"db_password": requestSecret},
		})
		mustOK(t, applyRun)
		steps := applyRun.Steps()
		var want []string
		for _, id := range stubIDs {
			want = append(want, id+":started", id+":completed")
		}
		if len(steps) != len(want) {
			t.Fatalf("step events %d, want %d\n%s", len(steps), len(want), applyRun.Stdout)
		}
		for i, ev := range steps {
			if ev.Step+":"+ev.State != want[i] {
				t.Fatalf("step event %d is %s:%s, want %s", i, ev.Step, ev.State, want[i])
			}
			if ev.Index == nil || ev.Total == nil || *ev.Index != i/2+1 || *ev.Total != stubSteps {
				t.Fatalf("step event %d index/total %v/%v", i, ev.Index, ev.Total)
			}
		}
		j := readJournal(t, vm)
		for _, id := range stubIDs {
			if j.Steps[id].Status != "completed" {
				t.Fatalf("journal %s = %+v", id, j.Steps[id])
			}
		}
		if mode := statMode(t, vm, journalPath); mode != "600" {
			t.Fatalf("journal mode %s", mode)
		}
		logs := runLogs(t, vm, "apply")
		if len(logs) != 1 {
			t.Fatalf("apply run logs %v", logs)
		}
		if mode := statMode(t, vm, logs[0]); mode != "640" {
			t.Fatalf("run log mode %s", mode)
		}
	})

	t.Run("4 redaction", func(t *testing.T) {
		if applyRun.Stdout == "" {
			t.Skip("apply did not run")
		}
		logs := runLogs(t, vm, "apply")
		if len(logs) == 0 {
			t.Fatal("no apply run log")
		}
		logBody := catFile(t, vm, logs[0])
		journal := catFile(t, vm, journalPath)
		for name, secret := range map[string]string{"stub secret": node.Secret, "request secret": requestSecret} {
			for where, body := range map[string]string{"stdout": applyRun.Stdout, "run log": logBody, "journal": journal} {
				if strings.Contains(body, secret) {
					t.Fatalf("%s leaked into %s", name, where)
				}
			}
		}
		if !strings.Contains(applyRun.Stdout, "«redacted»") {
			t.Fatalf("stdout has no «redacted»\n%s", applyRun.Stdout)
		}
		if !strings.Contains(logBody, "«redacted»") {
			t.Fatalf("run log has no «redacted»\n%s", logBody)
		}
	})

	t.Run("5 rerun skips", func(t *testing.T) {
		res := harness.RPC(t, vm, "apply", nil, harness.RPCOptions{})
		mustOK(t, res)
		steps := res.Steps()
		if len(steps) != stubSteps {
			t.Fatalf("step events %d\n%s", len(steps), res.Stdout)
		}
		for _, ev := range steps {
			if ev.State != "skipped" || ev.Detail != "journal" {
				t.Fatalf("%s: %s/%s", ev.Step, ev.State, ev.Detail)
			}
		}
	})

	t.Run("6 inputs changed", func(t *testing.T) {
		vmExec(t, vm, "sed -i 's/^value = \"a\"$/value = \"b\"/' /etc/eru/cluster.toml && grep -q 'value = \"b\"' /etc/eru/cluster.toml")
		plan := planData(t, harness.RPC(t, vm, "plan", nil, harness.RPCOptions{}))
		if plan.WouldApply != 1 {
			t.Fatalf("plan %+v", plan)
		}
		for _, s := range plan.Steps {
			wantApply := s.ID == "stub-file-v1"
			if (s.Action == "apply") != wantApply || (wantApply && s.Reason != "inputs_changed") || (!wantApply && s.Reason != "journal") {
				t.Fatalf("plan step %+v", s)
			}
		}
		res := harness.RPC(t, vm, "apply", nil, harness.RPCOptions{})
		mustOK(t, res)
		completed := stepsIn(res, "completed")
		if len(completed) != 1 || completed[0] != "stub-file-v1" {
			t.Fatalf("completed %v", completed)
		}
		if got := catFile(t, vm, "/var/lib/eru-e2e/file"); got != "b" {
			t.Fatalf("stub file %q", got)
		}
	})

	t.Run("7 failure and resume", func(t *testing.T) {
		vmExec(t, vm, "touch /etc/eru-e2e/fail")
		res := harness.RPC(t, vm, "apply", nil, harness.RPCOptions{})
		mustFail(t, res, 5, "step_failed")
		var details struct {
			Step string `json:"step"`
		}
		decode(t, res.Envelope.Error.Details, &details)
		if details.Step != "stub-fail-v1" {
			t.Fatalf("details.step %q", details.Step)
		}
		if st := readJournal(t, vm).Steps["stub-fail-v1"].Status; st != "failed" {
			t.Fatalf("journal stub-fail-v1 %q", st)
		}
		vmExec(t, vm, "rm -f /etc/eru-e2e/fail")
		res = harness.RPC(t, vm, "apply", nil, harness.RPCOptions{})
		mustOK(t, res)
		completed := stepsIn(res, "completed")
		if len(completed) != 1 || completed[0] != "stub-fail-v1" {
			t.Fatalf("resume completed %v", completed)
		}
		for _, id := range []string{"stub-file-v1", "stub-secret-v1", "stub-slow-v1"} {
			if !contains(stepsIn(res, "skipped"), id) {
				t.Fatalf("%s did not skip on resume", id)
			}
		}
	})

	t.Run("8 lock contention", func(t *testing.T) {
		vmExec(t, vm, "echo 20 > /etc/eru-e2e/sleep_s")
		first := harness.RPCAsync(t, vm, "apply", nil, harness.RPCOptions{})
		pid := waitLockHolder(t, vm, "apply")
		two := 2
		res := harness.RPC(t, vm, "apply", nil, harness.RPCOptions{LockTimeoutS: &two})
		mustFail(t, res, 11, "precondition_failed")
		var details struct {
			Reason string `json:"reason"`
			Holder *struct {
				PID    int    `json:"pid"`
				Method string `json:"method"`
			} `json:"holder"`
		}
		decode(t, res.Envelope.Error.Details, &details)
		if details.Reason != "lock_held" || details.Holder == nil || details.Holder.PID != pid || details.Holder.Method != "apply" {
			t.Fatalf("lock details %s (want pid %d)", res.Envelope.Error.Details, pid)
		}
		select {
		case done := <-first:
			mustOK(t, done)
		case <-time.After(90 * time.Second):
			t.Fatal("background apply did not finish")
		}
	})

	t.Run("9 restart", func(t *testing.T) {
		vmExec(t, vm, "touch /etc/eru-e2e/restart")
		res := harness.RPC(t, vm, "apply", nil, harness.RPCOptions{})
		mustOK(t, res)
		if !hasWarning(res.Envelope, "restart_pending") {
			t.Fatalf("no restart_pending warning\n%s", res.Stdout)
		}
		var data struct {
			RestartPending []string `json:"restart_pending"`
		}
		decode(t, res.Envelope.Data, &data)
		if len(data.RestartPending) != 1 || data.RestartPending[0] != "stub-restart-v1" {
			t.Fatalf("restart_pending %v", data.RestartPending)
		}
		if fileExists(t, vm, "/var/lib/eru-e2e/restarted") {
			t.Fatal("restart marker written without confirm")
		}
		res = harness.RPC(t, vm, "apply", nil, harness.RPCOptions{Confirm: restartToken})
		mustOK(t, res)
		if !fileExists(t, vm, "/var/lib/eru-e2e/restarted") {
			t.Fatalf("restart marker missing after confirm\n%s", res.Stdout)
		}
		res = harness.RPC(t, vm, "apply", nil, harness.RPCOptions{Confirm: "wrong/restart"})
		mustFail(t, res, 4, "unsafe_action_requires_confirmation")
	})

	t.Run("10 dry run", func(t *testing.T) {
		vmExec(t, vm, "echo 0 > /etc/eru-e2e/sleep_s")
		before := fileSum(t, vm, journalPath)
		logsBefore := len(runLogs(t, vm, ""))
		res := harness.RPC(t, vm, "apply", nil, harness.RPCOptions{DryRun: true})
		mustOK(t, res)
		plan := planData(t, res)
		if plan.Total != stubSteps || plan.WouldApply != 1 || len(plan.Steps) != stubSteps {
			t.Fatalf("dry-run plan %+v", plan)
		}
		if fileSum(t, vm, journalPath) != before {
			t.Fatal("dry run changed the journal")
		}
		if n := len(runLogs(t, vm, "")); n != logsBefore {
			t.Fatalf("dry run wrote a run log: %d -> %d", logsBefore, n)
		}
	})

	t.Run("11 refusals", func(t *testing.T) {
		res := harness.RPC(t, vm, "bogus", nil, harness.RPCOptions{})
		mustFail(t, res, 1, "usage_error")
		var details struct {
			Methods []string `json:"methods"`
		}
		decode(t, res.Envelope.Error.Details, &details)
		if !contains(details.Methods, "apply") || !contains(details.Methods, "upgrade.install") {
			t.Fatalf("details.methods %v", details.Methods)
		}

		res = harness.RPC(t, vm, "plan", nil, harness.RPCOptions{User: "nobody"})
		mustFail(t, res, 11, "precondition_failed")
		if reason := envReason(t, res.Envelope); reason != "root_required" {
			t.Fatalf("nobody reason %q", reason)
		}

		before := fileSum(t, vm, journalPath)
		res = harness.RPC(t, vm, "apply", nil, harness.RPCOptions{ClientCommit: "000000000000"})
		mustFail(t, res, 10, "node_version_mismatch")
		if fileSum(t, vm, journalPath) != before {
			t.Fatal("version mismatch changed the journal")
		}
	})

	t.Run("12 upgrade.install", func(t *testing.T) {
		harness.Restore(vm)
		original := fileSum(t, vm, harness.NodeBinaryPath)
		bCommit := "e2eb" + node.Commit[:8]
		buildB := harness.NodeBuild(t, vm, bCommit)
		sumB := localSum(t, buildB)
		params := func(sum string) map[string]string {
			return map[string]string{"path": "/tmp/eru-b", "sha256": sum, "version": "dev", "commit": bCommit, "channel": "dev"}
		}

		if err := vm.Push(buildB, "/tmp/eru-b", "0600"); err != nil {
			t.Fatal(err)
		}
		wrong := strings.Repeat("0", 64)
		res := harness.RPC(t, vm, "upgrade.install", params(wrong), harness.RPCOptions{})
		mustFail(t, res, 14, "release_verification_failed")
		if fileSum(t, vm, harness.NodeBinaryPath) != original {
			t.Fatal("wrong sha256 changed the binary")
		}
		if fileExists(t, vm, "/tmp/eru-b") {
			t.Fatal("temp file kept after a sha256 mismatch")
		}

		if err := vm.Push(buildB, "/tmp/eru-b", "0600"); err != nil {
			t.Fatal(err)
		}
		// upgrade.install is exempt from the version check.
		res = harness.RPC(t, vm, "upgrade.install", params(sumB), harness.RPCOptions{ClientCommit: "000000000000"})
		mustOK(t, res)
		if got := commitOf(t, vm, harness.NodeBinaryPath); got != bCommit {
			t.Fatalf("installed commit %q, want %q", got, bCommit)
		}
		if got := commitOf(t, vm, harness.NodeBinaryPath+".prev"); got != node.Commit {
			t.Fatalf("eru.prev commit %q, want %q", got, node.Commit)
		}
		if fileExists(t, vm, "/tmp/eru-b") {
			t.Fatal("temp file kept after install")
		}
	})
}

type planResult struct {
	Steps []struct {
		ID     string `json:"id"`
		Index  int    `json:"index"`
		Action string `json:"action"`
		Reason string `json:"reason"`
	} `json:"steps"`
	WouldApply int `json:"would_apply"`
	Total      int `json:"total"`
}

func planData(t *testing.T, res harness.RPCResult) planResult {
	t.Helper()
	mustOK(t, res)
	var plan planResult
	decode(t, res.Envelope.Data, &plan)
	return plan
}

type journal struct {
	Steps map[string]struct {
		Status string `json:"status"`
	} `json:"steps"`
}

func readJournal(t *testing.T, vm *harness.VM) journal {
	t.Helper()
	var j journal
	decode(t, json.RawMessage(catFile(t, vm, journalPath)), &j)
	return j
}

func mustOK(t *testing.T, res harness.RPCResult) {
	t.Helper()
	if res.Exit != 0 || !res.Envelope.OK {
		t.Fatalf("exit %d\n%s\n%s", res.Exit, res.Stdout, res.Stderr)
	}
}

func mustFail(t *testing.T, res harness.RPCResult, exit int, code string) {
	t.Helper()
	if res.Exit != exit || res.Envelope.OK || res.Envelope.Error == nil || res.Envelope.Error.Code != code {
		t.Fatalf("want exit %d %s, got exit %d\n%s", exit, code, res.Exit, res.Stdout)
	}
}

func envReason(t *testing.T, env harness.Envelope) string {
	t.Helper()
	var details struct {
		Reason string `json:"reason"`
	}
	decode(t, env.Error.Details, &details)
	return details.Reason
}

func hasWarning(env harness.Envelope, code string) bool {
	for _, w := range env.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

func stepsIn(res harness.RPCResult, state string) []string {
	var out []string
	for _, ev := range res.Steps() {
		if ev.State == state {
			out = append(out, ev.Step)
		}
	}
	return out
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func decode(t *testing.T, raw json.RawMessage, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("decode: %v\n%s", err, raw)
	}
}

func vmExec(t *testing.T, vm *harness.VM, script string) {
	t.Helper()
	if out, err := vm.Exec("bash", "-c", "set -euo pipefail; "+script); err != nil {
		t.Fatalf("%s: %v\n%s", script, err, out)
	}
}

func catFile(t *testing.T, vm *harness.VM, path string) string {
	t.Helper()
	out, err := vm.Exec("cat", path)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func fileExists(t *testing.T, vm *harness.VM, path string) bool {
	t.Helper()
	_, code, err := vm.ExecCode("test", "-e", path)
	if err != nil {
		t.Fatal(err)
	}
	return code == 0
}

func statMode(t *testing.T, vm *harness.VM, path string) string {
	t.Helper()
	out, err := vm.Exec("stat", "-c", "%a", path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out)
}

func fileSum(t *testing.T, vm *harness.VM, path string) string {
	t.Helper()
	return fileSHA(t, vm, path)
}

func localSum(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: e2e build output.
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// runLogs lists run logs, optionally only those for method.
func runLogs(t *testing.T, vm *harness.VM, method string) []string {
	t.Helper()
	out, err := vm.Exec("bash", "-c", "shopt -s nullglob; for f in "+logDir+"/*.log; do echo \"$f\"; done")
	if err != nil {
		t.Fatal(err)
	}
	var logs []string
	for _, line := range strings.Fields(out) {
		if method == "" || strings.Contains(line, "-"+method+"-") {
			logs = append(logs, line)
		}
	}
	return logs
}

// waitLockHolder waits until a live eru node rpc holds the lock for method and
// returns its PID.
func waitLockHolder(t *testing.T, vm *harness.VM, method string) int {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		out, _, _ := vm.ExecCode("cat", "/run/lock/eru.lock.info")
		var holder struct {
			PID    int    `json:"pid"`
			Method string `json:"method"`
		}
		if json.Unmarshal([]byte(out), &holder) == nil && holder.Method == method && holder.PID > 0 {
			pids, _, _ := vm.ExecCode("pgrep", "-f", "eru node rpc")
			for _, p := range strings.Fields(pids) {
				if p == strconv.Itoa(holder.PID) {
					return holder.PID
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("no live lock holder for " + method)
	return 0
}

func commitOf(t *testing.T, vm *harness.VM, bin string) string {
	t.Helper()
	out, err := vm.Exec(bin, "version", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	env := harness.ParseEnvelope(t, out)
	var data map[string]string
	decode(t, env.Data, &data)
	return data["commit"]
}
