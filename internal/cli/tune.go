package cli

import (
	"fmt"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/remote"
	"github.com/spf13/cobra"
)

type tuneSetting struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	Unit    string `json:"unit,omitempty"`
	Reason  string `json:"reason"`
	Context string `json:"context"`
}

type tuneShowData struct {
	Inputs         map[string]any    `json:"inputs"`
	Settings       []tuneSetting     `json:"settings"`
	Current        map[string]string `json:"current"`
	PendingRestart []string          `json:"pending_restart"`
}

func fetchTune(cmd *cobra.Command, s *session, p *progress) (tuneShowData, error) {
	reply, err := s.node.Call(commandContext(cmd), "tune.show", nil, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return tuneShowData{}, err
	}
	var data tuneShowData
	if err := reply.Decode(&data); err != nil {
		return tuneShowData{}, err
	}
	return data, nil
}

func runTuneShow(cmd *cobra.Command, _ []string) error {
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	data, err := fetchTune(cmd, s, p)
	if err != nil {
		return err
	}
	return output.Success(cmd, output.Result{Data: data, Message: fmt.Sprintf("%d settings", len(data.Settings)), Warnings: s.warnings})
}

func runTuneExplain(cmd *cobra.Command, _ []string) error {
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	data, err := fetchTune(cmd, s, p)
	if err != nil {
		return err
	}
	lines := []string{"inputs:"}
	for _, key := range sortedKeys(data.Inputs) {
		lines = append(lines, fmt.Sprintf("  %s = %v", key, data.Inputs[key]))
	}
	lines = append(lines, "", "settings:")
	for _, st := range data.Settings {
		lines = append(lines, fmt.Sprintf("  %-32s %-12s %-12s %s", st.Name, st.Value, data.Current[st.Name], st.Reason))
	}
	if len(data.PendingRestart) > 0 {
		lines = append(lines, "", "restart pending for: "+strings.Join(data.PendingRestart, ", "))
	}
	return output.Success(cmd, output.Result{Data: data, Message: strings.Join(lines, "\n"), Warnings: s.warnings})
}

func runTuneApply(cmd *cobra.Command, _ []string) error {
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	reply, err := s.node.Call(commandContext(cmd), "tune.apply", nil, remote.CallOptions{
		DryRun:  dryRun(cmd),
		Confirm: stringFlag(cmd, "confirm"),
		OnEvent: p.event,
	})
	if err != nil {
		return err
	}
	warnings := append(s.warnings, reply.Warnings...)
	if dryRun(cmd) {
		var data engine.PlanData
		if err := reply.Decode(&data); err != nil {
			return err
		}
		return output.Success(cmd, output.Result{Data: data, Message: fmt.Sprintf("dry run: %d of %d tuning steps would apply", data.WouldApply, data.Total), Warnings: warnings})
	}
	var data engine.ApplyData
	if err := reply.Decode(&data); err != nil {
		return err
	}
	message := fmt.Sprintf("applied %d, skipped %d of %d tuning steps", data.Applied, data.Skipped, data.Total)
	if len(data.RestartPending) > 0 {
		message += "; restart pending: " + strings.Join(data.RestartPending, ", ")
	}
	return output.Success(cmd, output.Result{Data: data, Message: message, Warnings: warnings})
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
