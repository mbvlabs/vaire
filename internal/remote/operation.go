package remote

import (
	"context"
	"errors"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/rpc"
)

// operationKey is the context key for the id that groups the RPCs of one
// operator command.
type operationKey struct{}

// WithNewOperation returns ctx for one logical operator command. Every RPC
// made with the returned context carries the same operation_id, so the
// requests of one apply, config change, backup or server init form one
// operation in the node's history. It only correlates requests: it does not
// authenticate the operator or grant any privilege.
func WithNewOperation(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, operationKey{}, rpc.NewRequestID())
}

// operationIDFromContext returns the operation id carried by ctx, if any.
func operationIDFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	id, ok := ctx.Value(operationKey{}).(string)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

// effectiveOperationID returns the operation id the request actually carries.
// newRequest falls back to the request's own id when opts has none.
func effectiveOperationID(operationID, requestID string) string {
	if operationID != "" {
		return operationID
	}
	return requestID
}

// callOptionsForContext fills in the operation id from ctx unless the caller
// set one explicitly. Everything else in opts is left alone.
func callOptionsForContext(ctx context.Context, opts CallOptions) CallOptions {
	if opts.OperationID != "" {
		return opts
	}
	if id, ok := operationIDFromContext(ctx); ok {
		opts.OperationID = id
	}
	return opts
}

// annotateRPCError adds the operator's operation and request ids to a typed
// node error, so a failed command leaves the agent a stable handle for the
// node's history and run log. It copies the error and its details rather than
// mutating them: a shared error keeps its original shape. Untyped errors are
// returned unchanged, because no faithful error envelope exists to enrich.
func annotateRPCError(err error, operationID, requestID string) error {
	if err == nil || (operationID == "" && requestID == "") {
		return err
	}
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) || cliErr == nil {
		return err
	}
	copied := *cliErr
	copied.Details = correlateDetails(cliErr.Details, operationID, requestID)
	return &copied
}

// correlateDetails returns details with the ids added. An object is copied so
// the caller's map never changes. Any other value keeps its exact meaning
// under the "details" key, alongside the ids.
func correlateDetails(details any, operationID, requestID string) any {
	ids := make(map[string]any, 2)
	if operationID != "" {
		ids["operation_id"] = operationID
	}
	if requestID != "" {
		ids["request_id"] = requestID
	}
	if len(ids) == 0 {
		return details
	}
	switch value := details.(type) {
	case nil:
		return ids
	case map[string]any:
		out := make(map[string]any, len(value)+len(ids))
		for key, item := range value {
			out[key] = item
		}
		for key, item := range ids {
			if _, exists := out[key]; !exists {
				out[key] = item
			}
		}
		return out
	default:
		out := make(map[string]any, len(ids)+1)
		for key, item := range ids {
			out[key] = item
		}
		out["details"] = details
		return out
	}
}
