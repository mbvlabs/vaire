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
	// Server is the memory budget: RAM, the reserve, what clusters get.
	Server         map[string]any    `json:"server"`
	Database       string            `json:"database,omitempty"`
	Inputs         map[string]any    `json:"inputs"`
	Settings       []tuneSetting     `json:"settings"`
	Current        map[string]string `json:"current"`
	PendingRestart []string          `json:"pending_restart"`
}

func fetchTune(cmd *cobra.Command, s *session, p *progress) (tuneShowData, error) {
	var params any
	if db := stringFlag(cmd, "db"); db != "" {
		params = map[string]string{"db": db}
	}
	reply, err := s.node.Call(commandContext(cmd), "tune.show", params, remote.CallOptions{OnEvent: p.event})
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
	message := fmt.Sprintf("%d settings", len(data.Settings))
	if data.Database != "" {
		message += " for " + data.Database
	}
	return output.Success(cmd, output.Result{Data: data, Message: message, Warnings: s.warnings})
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
	lines := []string{"server:"}
	for _, key := range sortedKeys(data.Server) {
		lines = append(lines, fmt.Sprintf("  %s = %v", key, data.Server[key]))
	}
	if data.Database == "" {
		return output.Success(cmd, output.Result{Data: data, Message: strings.Join(append(lines, "", "no databases yet"), "\n"), Warnings: s.warnings})
	}
	lines = append(lines, "", "inputs for "+data.Database+":")
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
	confirm, confirms := confirmOptions(cmd)
	reply, err := s.node.Call(commandContext(cmd), "tune.apply", nil, remote.CallOptions{
		DryRun:   dryRun(cmd),
		Confirm:  confirm,
		Confirms: confirms,
		OnEvent:  p.event,
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
	if data.Restarted() {
		message += "; restarted " + restartSummary(data.Restarts)
	}
	var next []string
	if len(data.RestartPending) > 0 {
		message += "; restart pending: " + strings.Join(data.RestartPending, ", ")
		next = []string{"eru tune apply " + confirmFlags(data.ConfirmTokens)}
	}
	return output.Success(cmd, output.Result{Data: data, Message: message, Warnings: warnings, Next: next})
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
