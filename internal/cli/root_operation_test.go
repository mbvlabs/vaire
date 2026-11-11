package cli

import (
	"errors"
	"fmt"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/spf13/cobra"
)

// TestRootStartsFreshOperationPerExecution reuses one root, as tests do, and
// checks that each Execute gets its own operation. The probe calls the local
// dispatcher with a malformed method, which fails before any node path or SSH
// connection is touched but still carries the enriched error details.
func TestRootStartsFreshOperationPerExecution(t *testing.T) {
	root := NewRoot()
	var operations []string
	probe := &cobra.Command{
		Use: "operation-probe",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := (remote.Local{}).Call(cmd.Context(), "Bad Method", nil, remote.CallOptions{})
			operation, request, err := correlationFromError(err)
			if err != nil {
				return err
			}
			if operation == "" || request == "" {
				return fmt.Errorf("correlation ids are empty: operation=%q request=%q", operation, request)
			}
			if operation == request {
				return fmt.Errorf("operation fell back to the request id %s", request)
			}
			operations = append(operations, operation)
			return nil
		},
	}
	root.AddCommand(probe)

	for i := 0; i < 2; i++ {
		root.SetArgs([]string{"operation-probe"})
		if _, err := root.ExecuteC(); err != nil {
			t.Fatalf("execution %d: %v", i+1, err)
		}
	}
	if len(operations) != 2 {
		t.Fatalf("ran %d probes, want 2", len(operations))
	}
	if operations[0] == operations[1] {
		t.Fatalf("reused root kept operation %s", operations[0])
	}
}

func correlationFromError(err error) (string, string, error) {
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		return "", "", fmt.Errorf("local failure is not a typed RPC error: %T: %w", err, err)
	}
	details, ok := cliErr.Details.(map[string]any)
	if !ok {
		return "", "", fmt.Errorf("details = %T, want an object", cliErr.Details)
	}
	operation, _ := details["operation_id"].(string)
	request, _ := details["request_id"].(string)
	return operation, request, nil
}
