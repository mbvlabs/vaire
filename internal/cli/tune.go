package cli

import (
	"fmt"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/remote"
	"github.com/mbvlabs/eru/internal/tune"
	"github.com/spf13/cobra"
)

type tuneSetting struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	Unit    string `json:"unit,omitempty"`
	Reason  string `json:"reason"`
	Context string `json:"context"`
}

// tuneServer is the server's budget sheet (RFC 7.5).
type tuneServer struct {
	RAMMiB               int                 `json:"ram_mib"`
	VCPU                 int                 `json:"vcpu"`
	ReserveMiB           int                 `json:"reserve_mib"`
	ClusterBudgetMiB     int                 `json:"cluster_budget_mib"`
	ClusterMemoryMiB     int                 `json:"cluster_memory_mib"`
	FreeMiB              int                 `json:"free_mib"`
	Clusters             []tuneServerCluster `json:"clusters"`
	CPUShare             int                 `json:"cpu_share"`
	HugePages            int                 `json:"huge_pages"`
	MemoryReserveSetting string              `json:"memory_reserve_setting"`
	IOWeightEnforced     bool                `json:"io_weight_enforced"`
	IOWeight             string              `json:"io_weight"`
	DiskGiB              int                 `json:"disk_gib"`
	DiskUsedGiB          int                 `json:"disk_used_gib"`
	CommitLimitMiB       int                 `json:"commit_limit_mib,omitempty"`
	CommittedMiB         int                 `json:"committed_mib,omitempty"`
	HugePagesFree        int                 `json:"huge_pages_free,omitempty"`
}

// tuneServerCluster is one cluster's row on the sheet.
type tuneServerCluster struct {
	Name             string `json:"name"`
	Size             string `json:"size"`
	MemoryMiB        int    `json:"memory_mib"`
	MemoryHighMiB    int    `json:"memory_high_mib"`
	MemoryMaxMiB     int    `json:"memory_max_mib,omitempty"`
	MemoryCurrentMiB *int   `json:"memory_current_mib,omitempty"`
	CPUWeight        int    `json:"cpu_weight"`
	CPUQuota         string `json:"cpu_quota,omitempty"`
	IOWeight         int    `json:"io_weight"`
	MaxConnections   int    `json:"max_connections"`
	SharedMemoryMiB  int    `json:"shared_memory_mib"`
	HugePages        int    `json:"huge_pages"`
	Running          bool   `json:"running"`
}

// sheetLines prints the budget sheet the way RFC 7.5 shows it.
func sheetLines(name string, sv tuneServer) []string {
	lines := []string{fmt.Sprintf("server %s   %s RAM · %d vCPU · reserve %s · budget %s",
		name, tune.FormatMiB(sv.RAMMiB), sv.VCPU, tune.FormatMiB(sv.ReserveMiB), tune.FormatMiB(sv.ClusterBudgetMiB))}
	for _, c := range sv.Clusters {
		state := "stopped"
		if c.Running {
			state = "running"
		}
		limits := fmt.Sprintf("cpu %3d  io %3d  conns %3d", c.CPUWeight, c.IOWeight, c.MaxConnections)
		if c.CPUQuota != "" {
			limits += "  quota " + c.CPUQuota
		}
		if c.MemoryMaxMiB > 0 {
			limits += "  max " + tune.FormatMiB(c.MemoryMaxMiB)
		}
		if c.MemoryCurrentMiB != nil {
			limits += "  using " + tune.FormatMiB(*c.MemoryCurrentMiB)
		}
		lines = append(lines, fmt.Sprintf("  %-16s %-7s %9s   %s   %s", c.Name, c.Size, tune.FormatMiB(c.MemoryMiB), limits, state))
	}
	free := "free " + tune.FormatMiB(sv.FreeMiB)
	if sv.FreeMiB < 0 {
		free = "overcommitted by " + tune.FormatMiB(-sv.FreeMiB)
	}
	lines = append(lines, fmt.Sprintf("  allocated %s of %s · %s", tune.FormatMiB(sv.ClusterMemoryMiB), tune.FormatMiB(sv.ClusterBudgetMiB), free))
	if sv.CommitLimitMiB > 0 {
		lines = append(lines, fmt.Sprintf("  commit limit %s · committed %s · huge pages %d reserved, %d free",
			tune.FormatMiB(sv.CommitLimitMiB), tune.FormatMiB(sv.CommittedMiB), sv.HugePages, sv.HugePagesFree))
	}
	lines = append(lines, "  io weight: "+sv.IOWeight)
	return lines
}

type tuneShowData struct {
	// Server is the budget sheet: RAM, the reserve, what clusters get.
	Server         tuneServer        `json:"server"`
	// The rest is one database's cluster; empty when no database was picked
	// (none, or several and no --db).
	Database       string            `json:"database,omitempty"`
	Inputs         map[string]any    `json:"inputs,omitempty"`
	Settings       []tuneSetting     `json:"settings,omitempty"`
	Current        map[string]string `json:"current,omitempty"`
	PendingRestart []string          `json:"pending_restart,omitempty"`
}

// fetchTune reads tune.show; the node's oversubscription warnings join the
// session's.
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
	s.warnings = append(s.warnings, reply.Warnings...)
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
	lines := sheetLines(s.ctx.Name, data.Server)
	if data.Database != "" {
		lines = append(lines, "", fmt.Sprintf("%d settings for %s (eru tune explain --db %s shows them)", len(data.Settings), data.Database, data.Database))
	} else if len(data.Server.Clusters) > 1 {
		lines = append(lines, "", "eru tune show --db <name> for a database's settings")
	}
	message := strings.Join(lines, "\n")
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
	if data.Database == "" && len(data.Server.Clusters) > 1 {
		names := make([]string, 0, len(data.Server.Clusters))
		for _, c := range data.Server.Clusters {
			names = append(names, c.Name)
		}
		return output.NewError(output.CodeInputRequired, "this server has several databases; name one with --db", output.ExitInput,
			"Pick a database from details.databases.").WithDetails(map[string]any{"flag": "--db", "databases": names})
	}
	lines := sheetLines(s.ctx.Name, data.Server)
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
