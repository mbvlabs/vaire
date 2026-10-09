// Package rpc holds the node RPC wire types shared by the operator and the node.
package rpc

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/cli/output"
)

// Schema is the request schema. Its fields and the upgrade.install params are
// frozen: later versions may only add optional fields.
const Schema = "eru-rpc/v1"

// MaxRequestBytes caps the request read from stdin.
const MaxRequestBytes = 1 << 20

// DefaultLockTimeout applies when the request omits lock_timeout_s.
const DefaultLockTimeout = 30 * time.Second

// Request is one node RPC call.
type Request struct {
	Schema        string            `json:"schema"`
	RequestID     string            `json:"request_id"`
	Method        string            `json:"method"`
	Params        json.RawMessage   `json:"params,omitempty"`
	Secrets       map[string]string `json:"secrets,omitempty"`
	DryRun        bool              `json:"dry_run"`
	Confirm       string            `json:"confirm"`
	Operator      string            `json:"operator"`
	ClientVersion string            `json:"client_version"`
	ClientCommit  string            `json:"client_commit"`
	ClientChannel string            `json:"client_channel"`
	LockTimeoutS  *int              `json:"lock_timeout_s,omitempty"`
}

// LockTimeout returns the lock wait for this request. Zero tries once.
func (r Request) LockTimeout() time.Duration {
	if r.LockTimeoutS == nil {
		return DefaultLockTimeout
	}
	if *r.LockTimeoutS <= 0 {
		return 0
	}
	return time.Duration(*r.LockTimeoutS) * time.Second
}

// Loggable returns the request with secret values dropped. Only the secret
// names remain.
func (r Request) Loggable() map[string]any {
	names := make([]string, 0, len(r.Secrets))
	for name := range r.Secrets {
		names = append(names, name)
	}
	out := map[string]any{
		"schema":         r.Schema,
		"request_id":     r.RequestID,
		"method":         r.Method,
		"dry_run":        r.DryRun,
		"confirm":        r.Confirm,
		"operator":       r.Operator,
		"client_version": r.ClientVersion,
		"client_commit":  r.ClientCommit,
		"client_channel": r.ClientChannel,
		"lock_timeout_s": r.LockTimeout().Seconds(),
		"secret_names":   names,
	}
	if len(r.Params) > 0 {
		out["params"] = r.Params
	}
	return out
}

var requestIDPattern = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

var methodPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(\.[a-z][a-z0-9]*)*$`)

// DecodeRequest reads exactly one JSON object from r, then EOF, within
// MaxRequestBytes. A request without request_id gets a new one.
func DecodeRequest(r io.Reader) (Request, error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxRequestBytes+1))
	if err != nil {
		return Request{}, usage("could not read the request", err.Error())
	}
	if len(raw) > MaxRequestBytes {
		return Request{}, usage("request is larger than 1 MiB", "Send one JSON object of at most 1 MiB.")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	var req Request
	if err := dec.Decode(&req); err != nil {
		return Request{}, usage("request is not valid JSON", err.Error())
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Request{}, usage("request must be exactly one JSON object", "Send one JSON object, then close stdin.")
	}
	if req.Schema != Schema {
		return Request{}, usage("request schema must be "+Schema, "Set \"schema\": \""+Schema+"\".")
	}
	if !methodPattern.MatchString(req.Method) {
		return Request{}, usage("request method is missing or malformed", "Set \"method\", for example \"plan\".")
	}
	if req.RequestID == "" {
		req.RequestID = NewRequestID()
	}
	req.RequestID = strings.ToUpper(req.RequestID)
	if !requestIDPattern.MatchString(req.RequestID) {
		return Request{}, usage("request_id must be a ULID", "Omit request_id and the node assigns one.")
	}
	if len(req.Params) > 0 && !bytes.HasPrefix(bytes.TrimSpace(req.Params), []byte("{")) && string(bytes.TrimSpace(req.Params)) != "null" {
		return Request{}, usage("params must be a JSON object", "")
	}
	return req, nil
}

func usage(message, hint string) *output.CLIError {
	return output.NewError(output.CodeUsage, message, output.ExitUsage, hint)
}

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewRequestID returns a ULID: a 48-bit millisecond timestamp and 80 random
// bits in Crockford base32.
func NewRequestID() string {
	var id [16]byte
	ms := uint64(time.Now().UnixMilli()) //nolint:gosec // G115: wall-clock milliseconds are positive.
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], ms)
	copy(id[:6], ts[2:])
	if _, err := rand.Read(id[6:]); err != nil {
		panic(err)
	}
	return encodeULID(id)
}

// encodeULID encodes 128 bits as 26 base32 characters; the first character
// carries the top 3 bits.
func encodeULID(id [16]byte) string {
	hi := binary.BigEndian.Uint64(id[:8])
	lo := binary.BigEndian.Uint64(id[8:])
	out := make([]byte, 26)
	for i := 25; i >= 0; i-- {
		out[i] = crockford[lo&0x1f]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out)
}
