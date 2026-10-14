// Package remote is the operator side of node execution: the NDJSON RPC
// client over SSH (or in-process for --local), the version handshake, and
// pushing the node binary (§7.3-§7.5).
package remote

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/mbvlabs/eru/internal/buildinfo"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/node"
	"github.com/mbvlabs/eru/internal/rpc"
	"github.com/mbvlabs/eru/internal/secrets"
	"github.com/mbvlabs/eru/internal/transport"
)

// CallOptions are the per-request fields beyond method and params.
type CallOptions struct {
	Secrets map[string]string
	DryRun  bool
	Confirm string
	// Confirms are further tokens, for methods that take several.
	Confirms    []string
	LockTimeout *int
	// OnEvent receives every event before the result: step, log, progress,
	// warning.
	OnEvent func(output.Event)
}

// Reply is a successful result.
type Reply struct {
	Data     json.RawMessage
	Warnings []output.Warning
}

// Decode decodes the result data into v.
func (r Reply) Decode(v any) error {
	if len(r.Data) == 0 {
		return nil
	}
	return json.Unmarshal(r.Data, v)
}

// Caller runs node RPC methods.
type Caller interface {
	Call(ctx context.Context, method string, params any, opts CallOptions) (Reply, error)
}

// newRequest encodes one request line and returns it with its request id.
func newRequest(method string, params any, operator string, opts CallOptions) ([]byte, string, error) {
	var rawParams json.RawMessage
	if params != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			return nil, "", err
		}
		rawParams = encoded
	}
	for _, v := range opts.Secrets {
		secrets.Add(v)
	}
	req := rpc.Request{
		Schema:        rpc.Schema,
		RequestID:     rpc.NewRequestID(),
		Method:        method,
		Params:        rawParams,
		Secrets:       opts.Secrets,
		DryRun:        opts.DryRun,
		Confirm:       opts.Confirm,
		Confirms:      opts.Confirms,
		Operator:      operator,
		ClientVersion: buildinfo.Version,
		ClientCommit:  buildinfo.Commit,
		ClientChannel: buildinfo.Channel,
		LockTimeoutS:  opts.LockTimeout,
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, "", err
	}
	return append(raw, '\n'), req.RequestID, nil
}

// readEvents reads NDJSON events until the result and returns it.
func readEvents(r io.Reader, onEvent func(output.Event)) (Reply, bool, error) {
	reader := bufio.NewReaderSize(r, 256*1024)
	for {
		line, err := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var ev struct {
				output.Event
				Envelope json.RawMessage `json:"envelope"`
			}
			if jerr := json.Unmarshal(line, &ev); jerr == nil {
				if ev.Type == "result" {
					reply, rerr := parseResult(ev.Envelope)
					_, _ = io.Copy(io.Discard, reader)
					return reply, true, rerr
				}
				if onEvent != nil {
					e := ev.Event
					e.Envelope = nil
					onEvent(e)
				}
			}
		}
		if err != nil {
			return Reply{}, false, nil
		}
	}
}

type resultEnvelope struct {
	OK       bool              `json:"ok"`
	Data     json.RawMessage   `json:"data"`
	Warnings []output.Warning  `json:"warnings"`
	Error    *output.ErrorBody `json:"error"`
}

func parseResult(raw json.RawMessage) (Reply, error) {
	var env resultEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return Reply{}, output.NewError(output.CodeTransportFailed, "the node result does not parse: "+err.Error(), output.ExitTransport, "")
	}
	if env.OK {
		return Reply{Data: env.Data, Warnings: env.Warnings}, nil
	}
	if env.Error == nil {
		return Reply{}, output.NewError(output.CodeError, "the node reported a failure without an error", output.ExitGeneric, "")
	}
	return Reply{}, &output.CLIError{
		Code:     env.Error.Code,
		Message:  env.Error.Message,
		Hint:     env.Error.Hint,
		Details:  env.Error.Details,
		ExitCode: output.ExitFor(env.Error.Code),
	}
}

// Node is a node reached over SSH. Methods run as root through
// sudo -n /usr/local/bin/eru node rpc, or directly when the SSH user is root.
type Node struct {
	SSH      *transport.SSHClient
	Operator string
	// Stderr receives the node's free-form diagnostics for --verbose. Nil
	// keeps only the tail for error details.
	Stderr io.Writer
	// OnUpload, when set, is told how many bytes of a binary upload were
	// sent, for a progress bar.
	OnUpload func(sent, total int64)
}

func (n *Node) asRoot(command string) string {
	if n.SSH.Target.User == "root" {
		return command
	}
	return "sudo -n " + command
}

// Call runs one RPC method and streams its events to opts.OnEvent.
func (n *Node) Call(ctx context.Context, method string, params any, opts CallOptions) (Reply, error) {
	payload, requestID, err := newRequest(method, params, n.Operator, opts)
	if err != nil {
		return Reply{}, err
	}
	pr, pw := io.Pipe()
	stderr := &tail{max: 8 * 1024}
	var errOut io.Writer = stderr
	if n.Stderr != nil {
		errOut = io.MultiWriter(stderr, n.Stderr)
	}
	runErr := make(chan error, 1)
	go func() {
		err := n.SSH.Run(ctx, n.asRoot(node.BinaryPath+" node rpc"), bytes.NewReader(payload), pw, errOut)
		_ = pw.CloseWithError(io.EOF)
		runErr <- err
	}()
	reply, got, perr := readEvents(pr, opts.OnEvent)
	err = <-runErr
	if got {
		return reply, perr
	}
	if ctx.Err() != nil {
		return Reply{}, ctx.Err()
	}
	if n.SSH.StreamLost(err) {
		return Reply{}, streamLost(method, requestID, n.SSH, err)
	}
	detail := strings.TrimSpace(secrets.Redact(stderr.String()))
	status, _ := transport.ExitStatus(err)
	return Reply{}, output.NewError(
		output.CodeTransportFailed,
		fmt.Sprintf("eru node rpc %s on %s ended without a result", method, n.SSH.Target),
		output.ExitTransport,
		"Re-run with --verbose to see the node's stderr. sudo may be refusing eru node rpc.",
	).WithDetails(map[string]any{"exit_status": status, "stderr": detail, "error": errString(err)})
}

// streamLost is the error for a connection that died before the result. The
// node does not notice a dropped stream: it finishes the request and writes
// the result to its run log, which is named after the request id.
func streamLost(method, requestID string, c *transport.SSHClient, err error) *output.CLIError {
	runLog := fmt.Sprintf("%s/*-%s-%s.log", engine.LogDir, method, requestID)
	why := "the connection closed"
	if c.Lost() {
		why = "the node stopped answering SSH keepalives"
	}
	return output.NewError(
		output.CodeTransportFailed,
		fmt.Sprintf("lost the connection to %s during %s (%s); the node keeps running it", c.Target, method, why),
		output.ExitTransport,
		"Its outcome will be in the node's run log "+runLog+" (eru ssh, then sudo cat it). Check it, or eru plan, before re-running.",
	).WithDetails(map[string]any{"reason": "stream_lost", "request_id": requestID, "run_log": runLog, "error": errString(err)})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return secrets.Redact(err.Error())
}

// Local runs methods in-process through the node dispatcher, for
// server init --local. The process must be root.
type Local struct {
	Operator string
}

// Call runs one RPC method in-process.
func (l Local) Call(_ context.Context, method string, params any, opts CallOptions) (Reply, error) {
	payload, _, err := newRequest(method, params, l.Operator, opts)
	if err != nil {
		return Reply{}, err
	}
	pr, pw := io.Pipe()
	go func() {
		node.Serve(bytes.NewReader(payload), pw)
		_ = pw.Close()
	}()
	reply, got, perr := readEvents(pr, opts.OnEvent)
	if !got {
		return Reply{}, errors.New("the in-process node dispatcher ended without a result")
	}
	return reply, perr
}

type tail struct {
	buf bytes.Buffer
	max int
}

func (t *tail) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if over := t.buf.Len() - t.max; over > 0 {
		t.buf.Next(over)
	}
	return len(p), nil
}

func (t *tail) String() string { return t.buf.String() }

// Stream runs eru node <args> as root and copies its standard output to w,
// for commands that produce raw bytes instead of NDJSON (eru node dump). The
// operator has already done the version handshake on this connection.
func (n *Node) Stream(ctx context.Context, args string, w io.Writer) error {
	stderr := &tail{max: 8 * 1024}
	err := n.SSH.Run(ctx, n.asRoot(node.BinaryPath+" node "+args), nil, w, stderr)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return output.NewError(output.CodeExternalCommand, "eru node "+args+" failed on "+n.SSH.Target.String(), output.ExitExternal,
		"Read details.stderr.").WithDetails(map[string]any{"stderr": strings.TrimSpace(secrets.Redact(stderr.String())), "error": errString(err)})
}
