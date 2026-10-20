package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
)

type countedStep struct{ id string }

func (s countedStep) ID() string                     { return s.id }
func (s countedStep) Describe(config.Cluster) string { return s.id }
func (s countedStep) Inputs(config.Cluster, Runtime) ([]byte, error) {
	return []byte(s.id), nil
}
func (s countedStep) Check(context.Context, config.Cluster, Runtime) (CheckResult, error) {
	return CheckResult{}, nil
}
func (s countedStep) Apply(context.Context, config.Cluster, Runtime, Reporter) (ApplyResult, error) {
	return ApplyResult{}, nil
}

// A promote runs the poolers, then a database's steps, then the backup
// steps: one sequence, [1/6] to [6/6].
func TestNumberingAcrossRuns(t *testing.T) {
	catalog := []Step{
		countedStep{"one-v1"}, countedStep{"two-v1"}, countedStep{"three-v1"},
		countedStep{"four-v1"}, countedStep{"five-v1"}, countedStep{"six-v1"},
	}
	runs := []Options{{Only: []string{"one-v1"}}, {Only: []string{"two-v1", "three-v1", "four-v1"}}, {Only: []string{"five-v1", "six-v1"}}}
	n := NewNumbering(catalog, config.Cluster{}, runs...)
	if n.Total() != 6 {
		t.Fatalf("total = %d, want 6", n.Total())
	}
	ctx := WithNumbering(context.Background(), n)
	journal := filepath.Join(t.TempDir(), "journal.json")
	var got []string
	report := Reporter(func(ev output.Event) {
		if ev.State == "started" {
			got = append(got, fmt.Sprintf("%d/%d", *ev.Index, *ev.Total))
		}
	})
	for _, opts := range runs {
		opts.JournalPath, opts.Report = journal, report
		data, _, err := Apply(ctx, catalog, config.Cluster{}, Runtime{}, opts)
		if err != nil {
			t.Fatal(err)
		}
		// The result keeps the run's own numbering.
		if data.Steps[0].Index != 1 {
			t.Errorf("result index %d, want 1", data.Steps[0].Index)
		}
	}
	want := []string{"1/6", "2/6", "3/6", "4/6", "5/6", "6/6"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("events %v, want %v", got, want)
	}

	// Without a Numbering each run counts on its own.
	got = nil
	if _, _, err := Apply(context.Background(), catalog, config.Cluster{}, Runtime{}, Options{Only: []string{"one-v1", "two-v1"}, JournalPath: filepath.Join(t.TempDir(), "j.json"), Report: report}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[1/2 2/2]" {
		t.Errorf("events %v, want [1/2 2/2]", got)
	}
}
