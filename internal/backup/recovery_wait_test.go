package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testRecoveryCursor(t *testing.T, stale string) (*recoveryLogCursor, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(path, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := newRecoveryLogCursor(path)
	if err != nil {
		t.Fatal(err)
	}
	return c, path
}

func appendRecoveryLog(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0) //nolint:gosec // G304: test file.
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

const mismatchLog = "123:WARNING:  hot standby is not possible because of insufficient parameter settings\n" +
	"123:DETAIL:  max_connections = 30 is a lower setting than on the primary server, where its value was 50.\n"

func TestWaitRecoveryChecksDiagnosticsAfterSQL(t *testing.T) {
	cursor, path := testRecoveryCursor(t, "old log\n")
	probes := recoveryWaitProbes{
		service: func(context.Context, string) recoveryServiceState { return serviceStarting },
		process: func(context.Context, string) recoveryObservation { return recoveryRecovering },
		sql: func(context.Context, int) recoveryObservation {
			appendRecoveryLog(t, path, mismatchLog)
			return recoveryRecovering
		},
	}
	start := time.Now()
	err := waitRecovery(context.Background(), "target", "data", 5555, UntilOpen, TargetLatest, cursor, probes, func(string) {})
	if !RetainRecoveryFailure(err) || time.Since(start) > recoveryPollBudget {
		t.Fatalf("err=%v elapsed=%v", err, time.Since(start))
	}
}

func TestWaitRecoveryWholePollBudgetAndSplitDetail(t *testing.T) {
	cursor, path := testRecoveryCursor(t, "")
	warning, detail, ok := strings.Cut(mismatchLog, "\n")
	if !ok {
		t.Fatal("missing detail")
	}
	appendRecoveryLog(t, path, warning+"\n")
	start := time.Now()
	err := waitRecovery(context.Background(), "target", "data", 5555, UntilOpen, TargetLatest, cursor, recoveryWaitProbes{
		service: func(ctx context.Context, _ string) recoveryServiceState {
			appendRecoveryLog(t, path, detail)
			<-ctx.Done()
			return serviceUnknown
		},
		sql:     func(context.Context, int) recoveryObservation { return recoveryUnknown },
		process: func(context.Context, string) recoveryObservation { return recoveryUnknown },
	}, func(string) {})
	elapsed := time.Since(start)
	if !RetainRecoveryFailure(err) || elapsed > 4*time.Second {
		t.Fatalf("split detail: err=%v elapsed=%v", err, elapsed)
	}
}

func TestWaitRecoveryTargetStates(t *testing.T) {
	tests := []struct {
		name, until, kind, log string
		observation            recoveryObservation
		success                bool
	}{
		{"time pause", UntilPaused, TargetTime, "123:LOG:  recovery stopping before commit of transaction 4, time 2026\n123:LOG:  pausing at the end of recovery\n", recoveryPaused, true},
		{"set pause", UntilPaused, TargetSet, "123:LOG:  recovery stopping after reaching consistency\n123:LOG:  pausing at the end of recovery\n", recoveryPaused, true},
		{"wrong target", UntilPaused, TargetTime, "123:LOG:  recovery stopping after reaching consistency\n123:LOG:  pausing at the end of recovery\n", recoveryPaused, false},
		{"generic pause", UntilPaused, TargetTime, "123:LOG:  recovery has paused\n", recoveryPaused, false},
		{"open paused", UntilOpen, TargetLatest, "123:LOG:  recovery has paused\n", recoveryPaused, false},
		{"open recovering", UntilOpen, TargetLatest, "123:LOG:  consistent recovery state reached\n", recoveryRecovering, true},
		{"promoted", UntilPromoted, TargetLatest, "123:LOG:  recovery done\n", recoveryPromoted, true},
		{"not promoted", UntilPromoted, TargetLatest, "123:LOG:  consistent recovery state reached\n", recoveryRecovering, false},
		{"unknown", UntilOpen, TargetLatest, "123:LOG:  starting\n", recoveryUnknown, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cursor, path := testRecoveryCursor(t, "")
			appendRecoveryLog(t, path, tc.log)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			err := waitRecovery(ctx, "target", "data", 5555, tc.until, tc.kind, cursor, recoveryWaitProbes{
				service: func(context.Context, string) recoveryServiceState { return serviceActive },
				sql:     func(context.Context, int) recoveryObservation { return tc.observation },
				process: func(context.Context, string) recoveryObservation { return recoveryRecovering },
			}, func(string) {})
			if (err == nil) != tc.success {
				t.Fatalf("err=%v success=%v", err, tc.success)
			}
		})
	}
}

func TestWaitRecoveryUnknownEvidenceAndFailure(t *testing.T) {
	for _, mode := range []string{"rotation", "truncated", "unreadable", "stale", "partial", "service-failed", "sql-refused"} {
		t.Run(mode, func(t *testing.T) {
			cursor, path := testRecoveryCursor(t, mismatchLog)
			service := serviceActive
			if mode == "rotation" {
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("new\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "partial" {
				appendRecoveryLog(t, path, "123:LOG:  incomplete")
			}
			if mode == "truncated" {
				if err := os.Truncate(path, 0); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "unreadable" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "service-failed" {
				service = serviceFailed
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			err := waitRecovery(ctx, "target", "data", 5555, UntilPaused, TargetTime, cursor, recoveryWaitProbes{
				service: func(context.Context, string) recoveryServiceState { return service },
				process: func(context.Context, string) recoveryObservation { return recoveryRecovering },
				sql: func(context.Context, int) recoveryObservation {
					if mode == "sql-refused" {
						return recoveryUnknown
					}
					return recoveryPaused
				},
			}, func(string) {})
			if err == nil || RetainRecoveryFailure(err) {
				t.Fatalf("unexpected result: %v", err)
			}
			if mode == "rotation" && !strings.Contains(err.Error(), "diagnostics") {
				t.Fatalf("wrong rotation result: %v", err)
			}
			if mode == "service-failed" && !strings.Contains(err.Error(), "stopped") {
				t.Fatalf("wrong service result: %v", err)
			}
			if mode == "stale" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("stale evidence: %v", err)
			}
		})
	}
}

func TestMismatchAssociationRequiresSamePrefix(t *testing.T) {
	foreign := strings.Replace(mismatchLog, "123:DETAIL", "456:DETAIL", 1)
	if _, ok := recoveryMismatch(foreign); ok {
		t.Fatal("associated a different process's detail")
	}
	fatal := strings.Replace(mismatchLog, "WARNING:  hot standby is not possible", "FATAL:  recovery aborted", 1)
	if _, ok := recoveryMismatch(fatal); !ok {
		t.Fatal("missed fatal mismatch")
	}
}

func TestRecoveryServiceSubmissionAndStartupOptions(t *testing.T) {
	cmd := recoveryStartCommand(context.Background(), "target")
	want := []string{"systemctl", "start", "--no-block", "postgresql@18-target"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("start command %v", cmd.Args)
	}
	_, options, ok := strings.Cut(recoveryCtlOptions, "'")
	if !ok {
		t.Fatal("missing pg_ctl options")
	}
	options, _, _ = strings.Cut(options, "'")
	parts := strings.Fields(options)
	var settings []string
	for i := 0; i < len(parts); i += 2 {
		if i+1 >= len(parts) || parts[i] != "-o" || !strings.HasPrefix(parts[i+1], "-c") {
			t.Fatalf("invalid pg_ctl options: %v", parts)
		}
		settings = append(settings, strings.TrimPrefix(parts[i+1], "-c"))
	}
	if !reflect.DeepEqual(settings, recoveryStartupSettings) {
		t.Fatalf("postgres -C and start overrides differ: %v vs %v", settings, recoveryStartupSettings)
	}
}

func TestRecoveryDiagnosticOverridesAreTemporary(t *testing.T) {
	m := recoveryRegistration(t, nil)
	ctl := filepath.Join(m.ConfDir(), "pg_ctl.conf")
	original := "# original pg_ctl configuration\n"
	if err := os.WriteFile(ctl, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.prepareRecoveryDiagnostics(); err != nil {
		t.Fatal(err)
	}
	if err := m.prepareRecoveryDiagnostics(); err != nil {
		t.Fatalf("preparation retry: %v", err)
	}
	if err := m.restoreRecoveryDiagnostics(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(ctl) //nolint:gosec // G304: test file.
	if err != nil || string(raw) != original {
		t.Fatalf("original pg_ctl.conf: %q, %v", raw, err)
	}
	if err := m.restoreRecoveryDiagnostics(); err != nil {
		t.Fatalf("adoption retry: %v", err)
	}
}

func TestWaitRecoveryStoppedProcess(t *testing.T) {
	cursor, path := testRecoveryCursor(t, "")
	appendRecoveryLog(t, path, "123:LOG:  starting\n")
	err := waitRecovery(context.Background(), "target", "data", 5555, UntilOpen, TargetLatest, cursor, recoveryWaitProbes{
		service: func(context.Context, string) recoveryServiceState { return serviceActive },
		sql:     func(context.Context, int) recoveryObservation { return recoveryUnknown },
		process: func(context.Context, string) recoveryObservation { return recoveryStopped },
	}, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "stopped") || RetainRecoveryFailure(err) {
		t.Fatalf("stopped process: %v", err)
	}
}

func TestRecoveryEvidenceRequiresOrderedMarkers(t *testing.T) {
	e := &recoveryEvidence{}
	raw := "123:LOG:  pausing at the end of recovery\n123:LOG:  recovery stopping after reaching consistency\n"
	e.observe(raw, TargetSet, 0)
	e.observe(raw, TargetSet, 0)
	if e.arrived {
		t.Fatal("a replayed earlier pause was mistaken for target arrival")
	}
}

func TestWaitRecoveryServiceStopsBeforeFirstSQLReady(t *testing.T) {
	cursor, path := testRecoveryCursor(t, "")
	appendRecoveryLog(t, path, "123:LOG:  starting recovery\n")
	calls := 0
	start := time.Now()
	err := waitRecovery(context.Background(), "target", "data", 5555, UntilOpen, TargetLatest, cursor, recoveryWaitProbes{
		service: func(context.Context, string) recoveryServiceState {
			calls++
			if calls == 1 {
				return serviceStarting
			}
			return serviceInactive
		},
		sql:     func(context.Context, int) recoveryObservation { return recoveryUnknown },
		process: func(context.Context, string) recoveryObservation { return recoveryUnknown },
	}, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "stopped") || RetainRecoveryFailure(err) || time.Since(start) > 4*time.Second {
		t.Fatalf("clean early stop: err=%v calls=%d elapsed=%v", err, calls, time.Since(start))
	}
}

func TestWaitRecoveryQueuedInitiallyInactive(t *testing.T) {
	cursor, path := testRecoveryCursor(t, "")
	appendRecoveryLog(t, path, "123:LOG:  startup submitted\n")
	calls := 0
	err := waitRecovery(context.Background(), "target", "data", 5555, UntilOpen, TargetLatest, cursor, recoveryWaitProbes{
		service: func(context.Context, string) recoveryServiceState {
			calls++
			if calls == 1 {
				return serviceInactive
			}
			return serviceActive
		},
		sql: func(context.Context, int) recoveryObservation {
			if calls == 1 {
				return recoveryUnknown
			}
			return recoveryRecovering
		},
		process: func(context.Context, string) recoveryObservation { return recoveryUnknown },
	}, func(string) {})
	if err != nil || calls != 2 {
		t.Fatalf("queued start: err=%v calls=%d", err, calls)
	}
}

func TestWaitRecoveryProcessStopsWhileServiceUnknownBeforeSQLReady(t *testing.T) {
	cursor, path := testRecoveryCursor(t, "")
	appendRecoveryLog(t, path, "123:LOG:  starting recovery\n")
	calls := 0
	err := waitRecovery(context.Background(), "target", "data", 5555, UntilOpen, TargetLatest, cursor, recoveryWaitProbes{
		service: func(context.Context, string) recoveryServiceState {
			calls++
			if calls == 1 {
				return serviceStarting
			}
			return serviceUnknown
		},
		sql: func(context.Context, int) recoveryObservation { return recoveryUnknown },
		process: func(context.Context, string) recoveryObservation {
			if calls == 1 {
				return recoveryRecovering
			}
			return recoveryStopped
		},
	}, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "stopped") || calls != 2 {
		t.Fatalf("early process stop: err=%v calls=%d", err, calls)
	}
}
