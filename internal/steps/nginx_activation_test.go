package steps

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

// activationStep models the relevant nginx behavior: the file converges before
// activation, and Check cannot tell which configuration the workers loaded.
type activationStep struct {
	id      string
	input   string
	file    string
	fail    bool
	applied int
}

func (s *activationStep) ID() string                     { return s.id }
func (s *activationStep) Describe(config.Cluster) string { return s.id }
func (s *activationStep) Inputs(config.Cluster, engine.Runtime) ([]byte, error) {
	return []byte(s.input), nil
}
func (s *activationStep) Check(context.Context, config.Cluster, engine.Runtime) (engine.CheckResult, error) {
	return engine.CheckResult{Complete: s.file == s.input}, nil
}
func (s *activationStep) Apply(context.Context, config.Cluster, engine.Runtime, engine.Reporter) (engine.ApplyResult, error) {
	s.applied++

	s.file = s.input
	if s.fail {
		return engine.ApplyResult{}, errors.New("activation failed")
	}

	return engine.ApplyResult{}, nil
}

func TestNginxActivationFailureIsRetriedDespiteConvergedFile(t *testing.T) { //nolint:gocognit // Assert the completed, failed, retried and interrupted states in sequence.
	for _, id := range []string{tlsID, metricsID} {
		t.Run(id, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal.json")
			s := &activationStep{id: id, input: "old"}

			run := func() error {
				_, _, err := engine.Apply(context.Background(), []engine.Step{s}, config.Cluster{}, engine.Runtime{}, engine.Options{JournalPath: path})
				return err
			}
			if err := run(); err != nil {
				t.Fatal(err)
			}

			s.input, s.fail = "restricted", true

			if err := run(); err == nil {
				t.Fatal("expected activation failure")
			}

			j, err := engine.LoadJournal(path)
			if err != nil || j.Steps[id].Status != engine.StatusFailed {
				t.Fatalf("failed journal: %+v, %v", j, err)
			}

			s.fail = false

			if err := run(); err != nil {
				t.Fatal(err)
			}

			if s.applied != 3 {
				t.Fatalf("applied %d times, want retry despite matching file", s.applied)
			}

			j, err = engine.LoadJournal(path)
			if err != nil || j.Steps[id].Status != engine.StatusCompleted {
				t.Fatalf("completed journal: %+v, %v", j, err)
			}

			// A process interrupted after writing the file leaves running on
			// disk, which must also force activation on the next invocation.
			entry := j.Steps[id]
			entry.Status = engine.StatusRunning

			j.Steps[id] = entry
			if err := j.Save(path); err != nil {
				t.Fatal(err)
			}

			if err := run(); err != nil {
				t.Fatal(err)
			}

			if s.applied != 4 {
				t.Fatalf("interrupted activation was not retried: %d applies", s.applied)
			}
		})
	}
}

func TestMetricsScriptPropagatesExporterStopFailure(t *testing.T) {
	for _, phase := range []string{"disable", "start"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "calls")

			stub := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CALLS\"\n[ \"$1\" != disable ]\n"
			if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(stub), 0o755); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command("/bin/bash", "-s")
			cmd.Stdin = strings.NewReader(Script("metrics"))

			cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "CALLS=" + log, "PHASE=" + phase, "ALL_UNITS=exporter.service", "STOP_UNITS=exporter.service"}
			if out, err := cmd.CombinedOutput(); err == nil {
				t.Fatalf("expected stop failure; output %s", out)
			}

			calls, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}

			if !strings.Contains(string(calls), "disable --now exporter.service") || strings.Contains(string(calls), "reload nginx") {
				t.Fatalf("unexpected systemctl calls: %s", calls)
			}
		})
	}
}
