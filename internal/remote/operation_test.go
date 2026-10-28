package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/transport"
)

// requestFor builds one request the way Call does: the context supplies the
// operation, then newRequest fills in the request id. It needs neither SSH nor
// a node.
func requestFor(t *testing.T, ctx context.Context, opts CallOptions) rpc.Request {
	t.Helper()
	opts = callOptionsForContext(ctx, opts)
	payload, requestID, err := newRequest("plan", nil, "tester", opts)
	if err != nil {
		t.Fatalf("newRequest: %v", err)
	}
	var req rpc.Request
	if err := json.Unmarshal(bytes.TrimSpace(payload), &req); err != nil {
		t.Fatalf("request does not decode: %v", err)
	}
	if req.RequestID != requestID {
		t.Fatalf("request id = %s, payload says %s", requestID, req.RequestID)
	}
	return req
}

func TestOperationGroupsRequestsWithoutSharingRequestIDs(t *testing.T) {
	ctx := WithNewOperation(context.Background())
	first := requestFor(t, ctx, CallOptions{})
	second := requestFor(t, ctx, CallOptions{})

	operationID, ok := operationIDFromContext(ctx)
	if !ok || operationID == "" {
		t.Fatal("context carries no operation id")
	}
	if first.OperationID != operationID || second.OperationID != operationID {
		t.Fatalf("operation ids = %q and %q, want %q", first.OperationID, second.OperationID, operationID)
	}
	if first.RequestID == second.RequestID {
		t.Fatalf("both requests have request id %s", first.RequestID)
	}
	if first.RequestID == first.OperationID || second.RequestID == second.OperationID {
		t.Fatal("a context operation must not double as the request id")
	}
}

func TestExplicitOperationWinsOverContext(t *testing.T) {
	ctx := WithNewOperation(context.Background())
	explicit := rpc.NewRequestID()
	req := requestFor(t, ctx, CallOptions{OperationID: explicit})
	if req.OperationID != explicit {
		t.Fatalf("operation id = %s, want explicit %s", req.OperationID, explicit)
	}
	contextOperation, _ := operationIDFromContext(ctx)
	if req.OperationID == contextOperation {
		t.Fatal("explicit operation id was replaced by the context's")
	}
}

func TestOperationFallsBackToRequestID(t *testing.T) {
	req := requestFor(t, context.Background(), CallOptions{})
	if req.OperationID != req.RequestID {
		t.Fatalf("operation id = %s, want the request id %s", req.OperationID, req.RequestID)
	}
}

func TestWithNewOperationPreservesContext(t *testing.T) {
	type valueKey struct{}
	deadline := time.Now().Add(time.Hour)
	base, cancel := context.WithDeadline(context.WithValue(context.Background(), valueKey{}, "kept"), deadline)
	defer cancel()
	ctx := WithNewOperation(base)

	if got := ctx.Value(valueKey{}); got != "kept" {
		t.Fatalf("context value = %v, want kept", got)
	}
	gotDeadline, ok := ctx.Deadline()
	if !ok || !gotDeadline.Equal(deadline) {
		t.Fatalf("deadline = %v, %v, want %v", gotDeadline, ok, deadline)
	}
	cancel()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("cancellation did not reach the operation context")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("context error = %v, want context.Canceled", ctx.Err())
	}
}

func TestDistinctOperationsDoNotCollide(t *testing.T) {
	first := requestFor(t, WithNewOperation(context.Background()), CallOptions{})
	second := requestFor(t, WithNewOperation(context.Background()), CallOptions{})
	if first.OperationID == second.OperationID {
		t.Fatalf("separate operations share %s", first.OperationID)
	}
	if first.RequestID == second.RequestID {
		t.Fatalf("separate requests share %s", first.RequestID)
	}
}

func TestAnnotateRPCErrorPreservesShapeAndCopiesDetails(t *testing.T) {
	original := map[string]any{"reason": "lock_held", "database": "app"}
	originalErr := output.NewError(output.CodePrecondition, "database is busy", output.ExitPrecondition, "Wait for the lock.").
		WithDetails(original)
	operationID := rpc.NewRequestID()
	requestID := rpc.NewRequestID()

	err := annotateRPCError(originalErr, operationID, requestID)
	var got *output.CLIError
	if !errors.As(err, &got) {
		t.Fatalf("annotated error is not a CLIError: %T", err)
	}
	if got == originalErr {
		t.Fatal("annotateRPCError mutated the shared error")
	}
	if got.Code != originalErr.Code || got.Message != originalErr.Message ||
		got.Hint != originalErr.Hint || got.ExitCode != originalErr.ExitCode {
		t.Fatalf("error shape changed: %+v", got)
	}
	details, ok := got.Details.(map[string]any)
	if !ok {
		t.Fatalf("details = %T, want an object", got.Details)
	}
	if details["reason"] != "lock_held" || details["database"] != "app" {
		t.Fatalf("original details changed: %+v", details)
	}
	if details["operation_id"] != operationID || details["request_id"] != requestID {
		t.Fatalf("correlation ids = %v/%v", details["operation_id"], details["request_id"])
	}
	if len(original) != 2 {
		t.Fatalf("input details gained keys: %+v", original)
	}
	if _, exists := original["operation_id"]; exists {
		t.Fatal("input details were mutated")
	}
}

func TestAnnotateRPCErrorNestsNonObjectDetails(t *testing.T) {
	originalErr := output.NewError(output.CodeStepFailed, "step failed", output.ExitStep, "").WithDetails("plain detail")
	err := annotateRPCError(originalErr, rpc.NewRequestID(), rpc.NewRequestID())
	var got *output.CLIError
	if !errors.As(err, &got) {
		t.Fatalf("annotated error is not a CLIError: %T", err)
	}
	details, ok := got.Details.(map[string]any)
	if !ok {
		t.Fatalf("details = %T, want an object", got.Details)
	}
	if details["details"] != "plain detail" {
		t.Fatalf("nested detail = %v", details["details"])
	}
	if details["operation_id"] == "" || details["request_id"] == "" {
		t.Fatalf("missing correlation ids: %+v", details)
	}
}

func TestAnnotateRPCErrorLeavesUntypedErrorsAlone(t *testing.T) {
	plain := errors.New("boom")
	if got := annotateRPCError(plain, rpc.NewRequestID(), rpc.NewRequestID()); !errors.Is(got, plain) {
		t.Fatalf("untyped error was replaced: %v", got)
	}
}

func TestLocalCallAnnotatesNodeFailure(t *testing.T) {
	ctx := WithNewOperation(context.Background())
	_, err := (Local{}).Call(ctx, "Bad Method", nil, CallOptions{})
	var got *output.CLIError
	if !errors.As(err, &got) {
		t.Fatalf("local failure is not a CLIError: %T: %v", err, err)
	}
	details, ok := got.Details.(map[string]any)
	if !ok {
		t.Fatalf("details = %T, want an object", got.Details)
	}
	operationID, _ := operationIDFromContext(ctx)
	if details["operation_id"] != operationID {
		t.Fatalf("operation id = %v, want %s", details["operation_id"], operationID)
	}
	requestID, _ := details["request_id"].(string)
	if requestID == "" || requestID == operationID {
		t.Fatalf("request id = %q, operation id = %q", requestID, operationID)
	}
}

// countingParams counts how often a request payload is encoded. A replay
// would marshal it a second time.
type countingParams struct{ calls *int }

func (p countingParams) MarshalJSON() ([]byte, error) {
	*p.calls++
	return []byte("{}"), nil
}

func TestFailedCallDoesNotReplay(t *testing.T) {
	ctx := WithNewOperation(context.Background())
	calls := 0
	_, err := (Local{}).Call(ctx, "Bad Method", countingParams{calls: &calls}, CallOptions{})
	if err == nil {
		t.Fatal("the malformed method should fail")
	}
	if calls != 1 {
		t.Fatalf("encoded the request %d times, want 1: a failed call must not replay", calls)
	}
}

func TestFailureBeforeRequestHasNoCorrelationIDs(t *testing.T) {
	ctx := WithNewOperation(context.Background())
	// A non-marshalable parameter fails before newRequest constructs a
	// request, so there is nothing to correlate and no false handle.
	_, err := (Local{}).Call(ctx, "plan", make(chan int), CallOptions{})
	if err == nil {
		t.Fatal("marshal failure went unreported")
	}
	var cliErr *output.CLIError
	if errors.As(err, &cliErr) {
		t.Fatalf("marshal failure became a typed RPC error: %+v", cliErr)
	}

	// Node.Call must fail the same way without touching a nil SSH client.
	_, err = (&Node{}).Call(ctx, "plan", make(chan int), CallOptions{})
	if err == nil {
		t.Fatal("node marshal failure went unreported")
	}
	if strings.Contains(err.Error(), "operation_id") {
		t.Fatalf("error invented a correlation id: %v", err)
	}
}

func TestStreamLostKeepsReasonAndAddsOperation(t *testing.T) {
	requestID := rpc.NewRequestID()
	operationID := rpc.NewRequestID()
	c := &transport.SSHClient{Target: transport.SSHTarget{User: "root", Addr: "db2.example"}}
	err := streamLost("plan", requestID, operationID, c, nil)
	details, ok := err.Details.(map[string]any)
	if !ok {
		t.Fatalf("details = %T, want an object", err.Details)
	}
	if details["reason"] != "stream_lost" {
		t.Fatalf("reason = %v", details["reason"])
	}
	if details["request_id"] != requestID {
		t.Fatalf("request id = %v, want %s", details["request_id"], requestID)
	}
	if details["operation_id"] != operationID {
		t.Fatalf("operation id = %v, want %s", details["operation_id"], operationID)
	}
	runLog, _ := details["run_log"].(string)
	if !strings.Contains(runLog, requestID) {
		t.Fatalf("run log %q does not name request %s", runLog, requestID)
	}
	if !strings.Contains(err.Hint, "before re-running") {
		t.Fatalf("hint does not tell the operator to check before a replay: %q", err.Hint)
	}
	if !strings.Contains(err.Hint, "vaire activity show --request "+requestID) || !strings.Contains(err.Hint, "unknown outcome") {
		t.Fatalf("hint does not lead to metadata-only evidence with honest outcome coverage: %q", err.Hint)
	}
}

func TestParseResultCarriesFailureWarnings(t *testing.T) {
	warning := output.Warning{Code: "activity_not_recorded", Message: "the job's evidence could not be recorded"}
	raw := []byte(`{"ok":false,"warnings":[{"code":"activity_not_recorded","message":"the job's evidence could not be recorded"}],"error":{"code":"error","message":"job failed","hint":"read the journal","details":{"reason":"job_failed"}}}`)
	_, err := parseResult(raw)
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("parseResult error = %v, want a typed failure", err)
	}
	if cliErr.Code != output.CodeError || cliErr.Message != "job failed" || cliErr.Hint != "read the journal" {
		t.Fatalf("error fields changed: %+v", cliErr)
	}
	details, _ := cliErr.Details.(map[string]any)
	if details["reason"] != "job_failed" {
		t.Fatalf("error details changed: %#v", cliErr.Details)
	}
	if len(cliErr.Warnings) != 1 || cliErr.Warnings[0] != warning {
		t.Fatalf("warnings = %+v, want %+v", cliErr.Warnings, warning)
	}
	annotated := annotateRPCError(err, "op", "req")
	var withIDs *output.CLIError
	if !errors.As(annotated, &withIDs) {
		t.Fatalf("annotated error = %v", annotated)
	}
	if len(withIDs.Warnings) != 1 || withIDs.Warnings[0] != warning {
		t.Fatalf("annotated warnings = %+v", withIDs.Warnings)
	}
}
