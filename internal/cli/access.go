package cli

import (
	"fmt"
	"strings"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/mbvlabs/vaire/internal/steps"
	"github.com/spf13/cobra"
)

type databaseAccess struct {
	Database string               `json:"database"`
	Desired  string               `json:"desired"`
	State    string               `json:"state,omitempty"`
	Source   string               `json:"source,omitempty"`
	Owner    string               `json:"owner"`
	App      string               `json:"app"`
	RO       string               `json:"ro"`
	Coverage steps.AccessCoverage `json:"coverage"`
	Notice   string               `json:"notice"`
}

func runDBAccessShow(cmd *cobra.Command, args []string) error {
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	reply, err := p.call(commandContext(cmd), s.node, "db.access.show", map[string]any{"name": args[0]}, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return err
	}
	var data databaseAccess
	if err := reply.Decode(&data); err != nil {
		return err
	}
	return output.Success(cmd, output.Result{
		Data: data, Message: accessMessage(data), Warnings: append(s.warnings, reply.Warnings...),
	})
}

func accessMessage(data databaseAccess) string {
	lines := []string{
		fmt.Sprintf("%s: desired %s, coverage %s", data.Database, data.Desired, data.Coverage.Status),
		fmt.Sprintf("owner %s; app %s; read-only %s", data.Owner, data.App, data.RO),
	}
	if data.State != "" {
		lines = append(lines, "state: "+data.State)
	}
	if data.Source != "" {
		lines = append(lines, "inherited from: "+data.Source)
	}
	if data.Coverage.Status != "unknown" {
		c := data.Coverage
		lines = append(lines, fmt.Sprintf("missing: defaults %d, schemas %d, tables %d, sequences %d, routines %d",
			c.DefaultMissing, c.SchemaMissing, c.TableMissing, c.SequenceMissing, c.RoutineMissing))
		if len(c.UncoveredCreators) > 0 {
			lines = append(lines, "uncovered creators: "+strings.Join(c.UncoveredCreators, ", "))
		}
	}
	return strings.Join(append(lines, data.Notice), "\n")
}
