package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/activity"
	"github.com/mbvlabs/vaire/internal/buildinfo"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/diagnostics"
	"github.com/mbvlabs/vaire/internal/secrets"
)

func TestDiagnosticsCommandsInTree(t *testing.T) {
	collect := commandAt(t, "diagnostics collect")
	if collect == nil {
		t.Fatal("diagnostics collect is missing")
	}
	for _, name := range []string{
		"since", "until", "operation", "request", "job", "db", "action", "outcome",
		"limit", "cursor", "out", "dry-run",
	} {
		if collect.Flags().Lookup(name) == nil {
			t.Errorf("diagnostics collect lacks --%s", name)
		}
	}
	inspect := commandAt(t, "diagnostics inspect")
	if inspect == nil {
		t.Fatal("diagnostics inspect is missing")
	}
	if inspect.Args(inspect, nil) == nil {
		t.Error("diagnostics inspect should require a bundle file")
	}
}

func TestDiagnosticsCollectValidatesBeforeRemote(t *testing.T) {
	for name, tc := range map[string]struct {
		args   []string
		code   string
		reason string
	}{
		"missing out": {
			args:   nil,
			code:   output.CodeUsage,
			reason: "required",
		},
		"bad limit": {
			args:   []string{"--limit", "501"},
			code:   output.CodeUsage,
			reason: "out_of_range",
		},
		"bad outcome": {
			args:   []string{"--out", filepath.Join(t.TempDir(), "x.json"), "--outcome", "fine"},
			code:   output.CodeUsage,
			reason: "not_allowed",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := runDiagnosticsCollect(commandAt(t, "diagnostics collect", tc.args...), nil)
			if cliCodeOf(t, err) != tc.code || cliReasonOf(t, err) != tc.reason {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestDiagnosticsCollectRefusesBadTargetsBeforeRemote(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "existing.json")
	if err := os.WriteFile(existing, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(existing, link); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		out    string
		reason string
	}{
		"existing":       {existing, "output_exists"},
		"missing parent": {filepath.Join(dir, "missing", "incident.json"), "parent_missing"},
		"symlink":        {link, "symlink"},
	} {
		t.Run(name, func(t *testing.T) {
			err := runDiagnosticsCollect(commandAt(t, "diagnostics collect", "--out", tc.out), nil)
			if cliCodeOf(t, err) != output.CodePrecondition && cliCodeOf(t, err) != output.CodeValidation {
				t.Fatalf("err = %v", err)
			}
			if cliReasonOf(t, err) != tc.reason {
				t.Fatalf("reason = %q, want %q", cliReasonOf(t, err), tc.reason)
			}
		})
	}
	// The existing file is untouched.
	if data, err := os.ReadFile(existing); err != nil || string(data) != "keep me" { //nolint:gosec // G304: a test path.
		t.Fatalf("existing file changed: %q %v", data, err)
	}
}

func TestDiagnosticsCollectDryRunDoesNoNetworkOrWrite(t *testing.T) {
	out := filepath.Join(t.TempDir(), "incident.json")
	cmd := commandAt(t, "diagnostics collect", "--since", "2h", "--db", "app", "--out", out, "--dry-run", "--output", "json")
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := runDiagnosticsCollect(cmd, nil); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("dry run wrote a bundle")
	}
	var env struct {
		OK   bool                   `json:"ok"`
		Data diagnosticsCollectPlan `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("output: %v\n%s", err, stdout.String())
	}
	if !env.OK || env.Data.Path != out || !env.Data.DryRun {
		t.Fatalf("plan = %+v", env.Data)
	}
	if env.Data.Query.Since != "2h" || env.Data.Query.Database != "app" || env.Data.Query.Limit != activity.MaxLimit {
		t.Fatalf("query = %+v", env.Data.Query)
	}
}

func TestDiagnosticsInspectIsOfflineAndTyped(t *testing.T) {
	// An empty configuration root: inspect must never resolve a context.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "incident.json")
	want := cliTestBundle()
	want.Query = &activity.Query{Since: "24h", Database: "app", Outcome: activity.OutcomeSucceeded, Limit: 500}
	if err := diagnostics.Write(path, want); err != nil {
		t.Fatal(err)
	}
	root := NewRoot()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"diagnostics", "inspect", path, "--output", "json"})
	if _, err := root.ExecuteC(); err != nil {
		t.Fatalf("inspect: %v\n%s", err, stderr.String())
	}
	var env struct {
		OK   bool            `json:"ok"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("output: %v\n%s", err, stdout.String())
	}
	if !env.OK {
		t.Fatalf("inspect failed: %s", stdout.String())
	}
	var got diagnostics.Bundle
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatalf("data is not a typed bundle: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bundle changed:\n got %+v\nwant %+v", got, want)
	}
}

func TestDiagnosticsInspectNormalizesNullEvents(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	bundle := cliTestBundle()
	bundle.Activity.Events = nil
	bundle.Activity.Coverage = activity.Coverage{
		Node:      "db1",
		Available: false,
		Partial:   true,
	}
	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "null-events.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	root := NewRoot()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"diagnostics", "inspect", path, "--output", "json"})
	if _, err := root.ExecuteC(); err != nil {
		t.Fatalf("inspect: %v\n%s", err, stderr.String())
	}
	var env struct {
		OK   bool            `json:"ok"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("output: %v\n%s", err, stdout.String())
	}
	var got diagnostics.Bundle
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Activity.Events == nil {
		t.Fatal("offline output kept a null events array")
	}
	if !got.Activity.Coverage.Partial || got.Activity.Coverage.Available {
		t.Fatalf("missing history changed: %+v", got.Activity.Coverage)
	}
}

func TestActivityBuildVersions(t *testing.T) {
	oldVersion, oldCommit, oldChannel := buildinfo.Version, buildinfo.Commit, buildinfo.Channel
	defer func() {
		buildinfo.Version, buildinfo.Commit, buildinfo.Channel = oldVersion, oldCommit, oldChannel
	}()
	buildinfo.Version = "1.2.3"
	buildinfo.Commit = "abc123"
	buildinfo.Channel = "stable"
	if got := activityClientVersion(); got != "1.2.3+abc123" {
		t.Fatalf("client version = %q", got)
	}
	if got := activityNodeMetadataVersion(); got != "1.2.3" {
		t.Fatalf("release node version = %q, want the checked version without the operator commit", got)
	}
	buildinfo.Channel = "dev"
	if got := activityNodeMetadataVersion(); got != "1.2.3+abc123" {
		t.Fatalf("dev node version = %q, want the handshake-checked build", got)
	}
	buildinfo.Commit = "none"
	if got := activityClientVersion(); got != "1.2.3" {
		t.Fatalf("unstamped commit version = %q", got)
	}
}

func TestNewCollectBundleRecordsFilters(t *testing.T) {
	q := activity.Query{
		Since:       "24h",
		Until:       "1h",
		OperationID: "01J9ZK3Q6V3W8Y2XH5N7T1C4BD",
		Database:    "app",
		Action:      "backup.full",
		Outcome:     activity.OutcomeFailed,
		Limit:       500,
	}
	page := cliTestBundle().Activity
	b := newCollectBundle("prod", "db1", "2026-10-26T11:00:01Z", q, page)
	if b.Query == nil || *b.Query != q {
		t.Fatalf("bundle query = %+v, want %+v", b.Query, q)
	}
	path := filepath.Join(t.TempDir(), "incident.json")
	if err := diagnostics.Write(path, b); err != nil {
		t.Fatal(err)
	}
	got, err := diagnostics.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Query == nil || *got.Query != q {
		t.Fatalf("read query = %+v, want %+v", got.Query, q)
	}
}

func TestSafeActivityQueryRejectsRegisteredSecret(t *testing.T) {
	const canary = "vaire-query-secret-canary-7c1e"
	secrets.Add(canary)
	q := activity.Query{Database: canary, Limit: 500}
	safe, err := safeActivityQuery(q)
	if cliCodeOf(t, err) != output.CodeValidation || cliReasonOf(t, err) != "query_secret" {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatal("the error echoed the secret")
	}
	if strings.Contains(safe.Database, canary) {
		t.Fatal("the returned query kept the secret")
	}
	// Even if a caller bypasses the early check, the bundle validator refuses
	// the redacted filter rather than writing it.
	bundle := newCollectBundle("prod", "db1", "2026-10-26T11:00:01Z", q, cliTestBundle().Activity)
	out := filepath.Join(t.TempDir(), "incident.json")
	err = diagnostics.Write(out, bundle)
	if cliCodeOf(t, err) != output.CodeValidation {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatal("the error echoed the secret")
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatal("a secret-bearing query was written")
	}
}

func TestDiagnosticsBundleMessageShowsQuery(t *testing.T) {
	b := cliTestBundle()
	b.Query = &activity.Query{
		Since:    "24h",
		Database: "app",
		Outcome:  activity.OutcomeFailed,
		Limit:    500,
		Cursor:   "c1.abcdef",
	}
	message := diagnosticsBundleMessage("incident.json", b)
	for _, want := range []string{"query:", "since=24h", "db=app", "outcome=failed", "limit=500", "cursor=set"} {
		if !strings.Contains(message, want) {
			t.Errorf("message lacks %q:\n%s", want, message)
		}
	}
	if strings.Contains(message, "c1.abcdef") {
		t.Fatal("human output copied the opaque cursor")
	}
}

func TestDiagnosticsInspectMissingBundle(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	err := runDiagnosticsInspect(commandAt(t, "diagnostics inspect"), []string{filepath.Join(t.TempDir(), "absent.json")})
	if cliCodeOf(t, err) != output.CodeBundleNotFound {
		t.Fatalf("err = %v", err)
	}
}

func TestDiagnosticsCollectJSONDataIsTyped(t *testing.T) {
	// The collect result must stay typed rather than a map with free-form
	// fields. Build the value directly, since a successful remote read is not
	// available in a unit test.
	bundle := cliTestBundle()
	result := diagnosticsCollectResult{Path: "/tmp/incident.json", Query: activity.Query{Since: "24h", Limit: 500}, Bundle: bundle}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Path   string             `json:"path"`
		Query  activity.Query     `json:"query"`
		Bundle diagnostics.Bundle `json:"bundle"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.Bundle, bundle) || decoded.Query.Since != "24h" {
		t.Fatal("collect result lost typed fields")
	}
}

func TestCollectRedactionRejectsRegisteredSecrets(t *testing.T) {
	const canary = "vaire-test-secret-canary-8f3d2b1a"
	secrets.Add(canary)
	bundle := cliTestBundle()
	bundle.Metadata.Server = canary
	bundle.Activity.Events[0].Action = canary
	redacted := redactActivityPage(bundle.Activity)
	if strings.Contains(redacted.Events[0].Action, canary) {
		t.Fatal("activity redaction left the secret in place")
	}
	if strings.Contains(secrets.Redact(bundle.Metadata.Server), canary) {
		t.Fatal("metadata redaction left the secret in place")
	}
	bundle.Metadata.Server = secrets.Redact(bundle.Metadata.Server)
	bundle.Activity = redacted
	out := filepath.Join(t.TempDir(), "incident.json")
	err := diagnostics.Write(out, bundle)
	if cliCodeOf(t, err) != output.CodeValidation {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatal("the error echoed the secret")
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatal("a secret-bearing bundle was written")
	}
}

func cliTestBundle() diagnostics.Bundle {
	event := activity.Event{
		Schema:         activity.Schema,
		EventID:        "01J9ZK3Q6V3W8Y2XH5N7T1C4BD",
		OccurredAt:     "2026-10-26T10:00:00Z",
		Sequence:       1,
		Server:         "prod",
		Node:           "db1",
		NodeInstanceID: "3f1c2b4a-0000-4000-8000-000000000001",
		BootID:         "3f1c2b4a-0000-4000-8000-000000000002",
		OperationID:    "op-01J9ZK3Q6V3W8Y2XH5N7T1C4BD",
		RequestID:      "req-01J9ZK3Q6V3W8Y2XH5N7T1C4BE",
		Source:         activity.SourceRPC,
		Action:         "backup.full",
		Step:           "backup",
		Database:       "app",
		Outcome:        activity.OutcomeSucceeded,
		DurationMS:     1200,
		ActorKind:      activity.ActorOperator,
		ActorLabel:     "alice",
		NodeVersion:    "1.0.0",
	}
	return diagnostics.Bundle{
		Schema:    diagnostics.Schema,
		CreatedAt: "2026-10-26T11:00:01Z",
		Metadata: diagnostics.Metadata{
			Server:        "prod",
			Node:          "db1",
			NodeVersion:   "1.0.0",
			ClientVersion: "1.0.0",
			CollectedAt:   "2026-10-26T11:00:00Z",
		},
		Activity: activity.Page{
			Schema: activity.Schema,
			Events: []activity.Event{event},
			Coverage: activity.Coverage{
				Node:            "db1",
				Available:       true,
				FirstRetainedAt: "2026-10-26T09:00:00Z",
				LastRetainedAt:  "2026-10-26T11:00:00Z",
				FirstReturnedAt: "2026-10-26T10:00:00Z",
				LastReturnedAt:  "2026-10-26T10:00:00Z",
				EventsReturned:  1,
				EventsScanned:   1,
				SegmentsScanned: 1,
				BytesScanned:    128,
			},
		},
	}
}
