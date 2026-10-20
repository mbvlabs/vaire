package cli

import (
	"fmt"
	"strings"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/spf13/cobra"
)

type queryStatement struct {
	DB      string  `json:"db"`
	Role    string  `json:"role"`
	QueryID string  `json:"queryid"`
	Calls   int64   `json:"calls"`
	TotalMS float64 `json:"total_ms"`
	MeanMS  float64 `json:"mean_ms"`
	Rows    int64   `json:"rows"`
	HitPct  float64 `json:"hit_pct"`
	Query   string  `json:"query"`
}

type queriesData struct {
	Order      string           `json:"order"`
	Statements []queryStatement `json:"statements"`
	Missing    []string         `json:"missing,omitempty"`
}

// statsToken is the confirm token for queries --reset (Phase 4 D12).
func statsToken(server, db string) string {
	if db == "" {
		return server + "/stats"
	}
	return db + "/stats"
}

func runQueries(cmd *cobra.Command, _ []string) error {
	order := stringFlag(cmd, "order")
	if order != "total" && order != "mean" && order != "calls" {
		return output.NewError(output.CodeUsage, "--order must be total, mean or calls", output.ExitUsage, "")
	}
	limit, _ := cmd.Flags().GetInt("limit")
	db := stringFlag(cmd, "db")
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	params := map[string]any{"order": order, "limit": limit}
	if db != "" {
		params["db"] = db
	}
	if flagTrue(cmd, "all-sessions") {
		params["all_sessions"] = true
	}
	if flagTrue(cmd, "reset") {
		server := s.ctx.Server
		if server == "" {
			server = s.ctx.Name
		}
		token := statsToken(server, db)
		if err := Confirm(cmd, token); err != nil {
			return err
		}
		reply, err := p.call(commandContext(cmd), s.node, "queries.reset", params, remote.CallOptions{Confirm: token, OnEvent: p.event})
		if err != nil {
			return err
		}
		var data map[string]any
		_ = reply.Decode(&data)
		return output.Success(cmd, output.Result{Data: data, Message: fmt.Sprintf("reset pg_stat_statements in %v", data["reset"]),
			Warnings: append(s.warnings, reply.Warnings...)})
	}
	reply, err := p.call(commandContext(cmd), s.node, "queries.top", params, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return err
	}
	var data queriesData
	if err := reply.Decode(&data); err != nil {
		return err
	}
	return output.Success(cmd, output.Result{Data: data, Message: queriesMessage(data, flagTrue(cmd, "all-sessions")), Warnings: append(s.warnings, reply.Warnings...)})
}

func queriesMessage(d queriesData, allSessions bool) string {
	if len(d.Statements) == 0 {
		return "no statements recorded yet"
	}
	lines := []string{fmt.Sprintf("%-12s %-14s %9s %11s %9s %6s  %s", "DB", "ROLE", "CALLS", "TOTAL ms", "MEAN ms", "HIT%", "QUERY")}
	for _, st := range d.Statements {
		q := st.Query
		if len(q) > 80 {
			q = q[:77] + "..."
		}
		lines = append(lines, fmt.Sprintf("%-12s %-14s %9d %11.1f %9.2f %6.1f  %s", st.DB, st.Role, st.Calls, st.TotalMS, st.MeanMS, st.HitPct, q))
	}
	lines = append(lines, "sorted by "+d.Order+"; --json has the full query text")
	if !allSessions {
		lines = append(lines, "internal/administrative roles (including postgres and maintenance roles) are hidden; --all-sessions includes them")
	}
	return strings.Join(lines, "\n")
}
