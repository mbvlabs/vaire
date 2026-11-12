package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/contexts"
	"github.com/mbvlabs/vaire/internal/node"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/secrets"
	"github.com/mbvlabs/vaire/internal/steps"
	"github.com/spf13/cobra"
)

// newCopyTestCommand returns the real db copy command with its output and
// stdin isolated, without running its RunE (which would connect).
func newCopyTestCommand(t *testing.T) *cobra.Command {
	t.Helper()
	root := NewRoot()
	cmd, _, err := root.Find([]string{"db", "copy"})
	if err != nil {
		t.Fatal(err)
	}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetIn(strings.NewReader(""))
	return cmd
}

func setCopyFlag(t *testing.T, cmd *cobra.Command, name, value string) {
	t.Helper()
	flag := cmd.Flags().Lookup(name)
	if flag == nil {
		flag = cmd.InheritedFlags().Lookup(name)
	}
	if flag == nil {
		t.Fatalf("flag --%s is not defined", name)
	}
	if err := flag.Value.Set(value); err != nil {
		t.Fatalf("--%s=%s: %v", name, value, err)
	}
}

func copyCLIConfig() config.Cluster {
	cfg := config.DefaultCluster()
	cfg.ClusterName, cfg.Domain = "prod", "example.com"
	return cfg
}

func TestDBCopyCommandMetadata(t *testing.T) {
	root := NewRoot()
	if report := checkCommandMetadata(root); !report.OK {
		t.Fatalf("command metadata: %v", report.Failures)
	}
	cmd, _, err := root.Find([]string{"db", "copy"})
	if err != nil {
		t.Fatal(err)
	}
	meta, ok := metaFor(cmd)
	if !ok {
		t.Fatal("db copy has no metadata")
	}
	if !meta.Mutating || meta.Destructive || !meta.SupportsDryRun || meta.ConfirmToken != "{name}" {
		t.Fatalf("copy metadata = %+v", meta)
	}
	if !meta.Prompts || len(meta.InteractiveFlags) == 0 {
		t.Fatalf("copy prompt metadata = %+v", meta)
	}
	for _, flag := range []string{"source-url-stdin", "source-ca-file", "size", "memory", "profile", "allow-overcommit", "confirm", "dry-run"} {
		if cmd.Flags().Lookup(flag) == nil {
			t.Errorf("db copy has no --%s", flag)
		}
	}
}

func TestCopyConfirmationUsesNameToken(t *testing.T) {
	cmd := newCopyTestCommand(t)
	setCopyFlag(t, cmd, "no-input", "true")
	if err := Confirm(cmd, "app"); err == nil || errorCode(t, err) != output.CodeUnsafeAction {
		t.Fatalf("missing confirmation = %v, want unsafe_action", err)
	}
	setCopyFlag(t, cmd, "yes", "true")
	if err := Confirm(cmd, "app"); err == nil || errorCode(t, err) != output.CodeUnsafeAction {
		t.Fatalf("--yes confirmed the copy: %v", err)
	}
	setCopyFlag(t, cmd, "confirm", "wrong")
	if err := Confirm(cmd, "app"); err == nil || errorCode(t, err) != output.CodeUnsafeAction {
		t.Fatalf("wrong confirmation = %v, want unsafe_action", err)
	}
	setCopyFlag(t, cmd, "confirm", "app")
	if err := Confirm(cmd, "app"); err != nil {
		t.Fatalf("--confirm app was refused: %v", err)
	}
}

func TestCopyNameNoInputAndValidation(t *testing.T) {
	cmd := newCopyTestCommand(t)
	setCopyFlag(t, cmd, "no-input", "true")
	if _, err := copyName(cmd, nil); err == nil || errorCode(t, err) != output.CodeInputRequired {
		t.Fatalf("missing name = %v, want input_required", err)
	}
	name, err := copyName(cmd, []string{"app"})
	if err != nil || name != "app" {
		t.Fatalf("name = %q, %v", name, err)
	}
	if _, err := copyName(cmd, []string{"Bad-Name"}); err == nil || errorCode(t, err) != output.CodeValidation {
		t.Fatalf("invalid name = %v, want validation_failed", err)
	}
	// JSON mode also may not prompt.
	cmd = newCopyTestCommand(t)
	setCopyFlag(t, cmd, "output", "json")
	if _, err := copyName(cmd, nil); err == nil || errorCode(t, err) != output.CodeInputRequired {
		t.Fatalf("JSON missing name = %v, want input_required", err)
	}
}

func errorCode(t *testing.T, err error) string {
	t.Helper()
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		return ""
	}
	return cliErr.Code
}

func TestCollectCopySourceNeverPrompts(t *testing.T) {
	for name, setup := range map[string]func(*cobra.Command){
		"no-input": func(cmd *cobra.Command) { setCopyFlag(t, cmd, "no-input", "true") },
		"JSON":     func(cmd *cobra.Command) { setCopyFlag(t, cmd, "output", "json") },
	} {
		cmd := newCopyTestCommand(t)
		setup(cmd)
		if _, err := collectCopySource(cmd); err == nil || errorCode(t, err) != output.CodeInputRequired {
			t.Errorf("%s: collect source = %v, want input_required", name, err)
		}
	}
}

func TestReadCopySourceURL(t *testing.T) {
	raw := "postgresql://source_user:source-password@db.example.com:5432/app?sslmode=verify-full" //nolint:gosec // G101: a test fixture, not a credential.
	cmd := newCopyTestCommand(t)
	cmd.SetIn(strings.NewReader(raw + "\nnext-line\n"))
	got, err := readCopySourceURL(cmd)
	if err != nil || got != raw {
		t.Fatalf("URL = %q, %v", got, err)
	}
	// The reader stays on the command: the URL line is consumed once and
	// the following line remains for a later prompt or input.
	next, err := readFirstLine(cmd.InOrStdin())
	if err != nil || next != "next-line" {
		t.Fatalf("next line = %q, %v", next, err)
	}
	if strings.Contains(secrets.Redact("value "+raw), "source-password") {
		t.Fatal("the raw URL password was not registered for redaction")
	}

	for name, input := range map[string]string{
		"EOF without newline": raw,
		"empty line":          "\n",
	} {
		cmd := newCopyTestCommand(t)
		cmd.SetIn(strings.NewReader(input))
		if _, err := readCopySourceURL(cmd); err == nil || errorCode(t, err) != output.CodeInputRequired {
			t.Errorf("%s: %v, want input_required", name, err)
		}
	}
	cmd = newCopyTestCommand(t)
	cmd.SetIn(strings.NewReader(raw + "\r\n"))
	if got, err := readCopySourceURL(cmd); err != nil || got != raw {
		t.Fatalf("CRLF URL = %q, %v", got, err)
	}
}

// C06-03: the copy-specific password prompt is exact-byte in both renderers.
// Leading, trailing and all-space passwords must survive and encode to the
// same URL userinfo as a URL source would carry; the general trimming helper
// is deliberately unchanged.
func TestCopyPasswordPromptPreservesExactBytes(t *testing.T) {
	oldUI, oldRead := copyPasswordFromUI, copyPasswordRead
	t.Cleanup(func() { copyPasswordFromUI, copyPasswordRead = oldUI, oldRead })

	for name, password := range map[string]string{
		"leading spaces":  "  leading",
		"trailing spaces": "trailing  ",
		"all spaces":      "   ",
	} {
		t.Run(name, func(t *testing.T) {
			var plainDiag bytes.Buffer
			copyPasswordRead = func() (string, error) {
				if got := plainDiag.String(); got != "Source password: " {
					t.Errorf("masked reader ran before the label: %q", got)
				}
				return password, nil
			}
			copyPasswordFromUI = func(string) (string, error) { return password, nil }
			plain, err := readCopyPasswordWith(false, &plainDiag, "Source password")
			if err != nil || plain != password {
				t.Fatalf("plain renderer = %q, %v; want %q", plain, err, password)
			}
			if got := plainDiag.String(); got != "Source password: \n" {
				t.Fatalf("plain prompt diagnostics = %q, want one label and newline", got)
			}
			if strings.Contains(plainDiag.String(), password) {
				t.Fatalf("plain prompt echoed the password: %q", plainDiag.String())
			}

			var richDiag bytes.Buffer
			rich, err := readCopyPasswordWith(true, &richDiag, "Source password")
			if err != nil || rich != password {
				t.Fatalf("rich renderer = %q, %v; want %q", rich, err, password)
			}
			if richDiag.Len() != 0 {
				t.Fatalf("the rich renderer wrote the plain diagnostic: %q", richDiag.String())
			}

			var encoded [2]string
			for i, value := range []string{plain, rich} {
				u := &url.URL{Scheme: "postgresql", User: url.UserPassword("source_user", value), Host: "db.example.com", Path: "/app"}
				encoded[i] = u.String()
				parsed, parseErr := url.Parse(encoded[i])
				if parseErr != nil {
					t.Fatal(parseErr)
				}
				got, ok := parsed.User.Password()
				if !ok || got != password {
					t.Fatalf("renderer %d encoded URL %q did not preserve the password: %q %v", i, encoded[i], got, ok)
				}
			}
			if encoded[0] != encoded[1] {
				t.Fatalf("renderers disagree: %q vs %q", encoded[0], encoded[1])
			}
		})
	}

	for _, bad := range []string{"bad\npassword", "bad\rpassword", "bad\x00password"} {
		for _, rich := range []bool{false, true} {
			copyPasswordRead = func() (string, error) { return bad, nil }
			copyPasswordFromUI = func(string) (string, error) { return bad, nil }
			if _, err := readCopyPasswordWith(rich, &bytes.Buffer{}, "Source password"); err == nil || errorCode(t, err) != output.CodeValidation {
				t.Errorf("rich=%v password %q = %v, want validation_failed", rich, bad, err)
			}
		}
	}
}

func TestCopySourceSecretsConverge(t *testing.T) {
	ca := "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----\n"
	urlInput := copySourceInput{URL: "postgresql://u:secret-password@db.example.com/app?sslmode=verify-full", CA: ca} //nolint:gosec // G101: a test fixture, not a credential.
	urlSecrets, err := urlInput.secrets()
	if err != nil {
		t.Fatal(err)
	}
	if urlSecrets[node.CopySourceURLSecret] != urlInput.URL || urlSecrets[node.CopySourceCASecret] != ca {
		t.Fatalf("URL secrets = %#v", urlSecrets)
	}
	if _, ok := urlSecrets[node.CopySourceSpecSecret]; ok {
		t.Fatal("URL input also carried individual fields")
	}

	fields := &node.CopySourceSecret{
		Host: "db.example.com", Port: 5433, Database: "app", User: "u",
		Password: "secret-password", TLSMode: backup.SourceTLSVerifyFull,
	}
	fieldSecrets, err := (copySourceInput{Fields: fields, CA: ca}).secrets()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fieldSecrets[node.CopySourceURLSecret]; ok {
		t.Fatal("field input also carried a URL")
	}
	var decoded node.CopySourceSecret
	if err := json.Unmarshal([]byte(fieldSecrets[node.CopySourceSpecSecret]), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Host != fields.Host || decoded.Port != fields.Port || decoded.Database != fields.Database ||
		decoded.User != fields.User || decoded.Password != fields.Password || decoded.TLSMode != fields.TLSMode {
		t.Fatalf("field source changed: %+v", decoded)
	}
	if fieldSecrets[node.CopySourceCASecret] != ca {
		t.Fatal("the CA content was not sent as the separate CA secret")
	}
	if strings.Contains(fieldSecrets[node.CopySourceCASecret], "secret-password") {
		t.Fatal("the CA secret carries the password")
	}

	if _, err := (copySourceInput{URL: "postgresql://u:p@db/app"}).secrets(); err != nil { //nolint:gosec // G101: a test fixture, not a credential.
		t.Fatalf("URL without a CA: %v", err)
	}
	if _, err := (copySourceInput{URL: "postgresql://u:p@db/app", Fields: fields}).secrets(); err == nil { //nolint:gosec // G101: a test fixture, not a credential.
		t.Fatal("both source forms were accepted")
	}
	if _, err := (copySourceInput{}).secrets(); err == nil || errorCode(t, err) != output.CodeInputRequired {
		t.Fatalf("empty source = %v, want input_required", err)
	}
}

func TestReadCopyCAFileSendsContentNotPath(t *testing.T) {
	content := "-----BEGIN CERTIFICATE-----\nsource-ca\n-----END CERTIFICATE-----\n"
	path := filepath.Join(t.TempDir(), "provider-ca.pem")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newCopyTestCommand(t)
	setCopyFlag(t, cmd, "source-ca-file", path)
	got, err := readCopyCAFile(cmd)
	if err != nil || got != content {
		t.Fatalf("CA = %q, %v", got, err)
	}
	if strings.Contains(got, path) {
		t.Fatal("the CA secret carries the local path")
	}
	if strings.Contains(secrets.Redact("value "+content), "source-ca") {
		t.Fatal("the CA content was not registered for redaction")
	}

	empty := filepath.Join(t.TempDir(), "empty.pem")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd = newCopyTestCommand(t)
	setCopyFlag(t, cmd, "source-ca-file", empty)
	if _, err := readCopyCAFile(cmd); err == nil || errorCode(t, err) != output.CodeValidation {
		t.Fatalf("empty CA = %v, want validation_failed", err)
	}
	cmd = newCopyTestCommand(t)
	setCopyFlag(t, cmd, "source-ca-file", filepath.Join(t.TempDir(), "missing.pem"))
	if _, err := readCopyCAFile(cmd); err == nil || errorCode(t, err) != output.CodeUsage {
		t.Fatalf("missing CA = %v, want usage_error", err)
	}
}

func TestCopyDestinationSizesOvercommitAndParams(t *testing.T) {
	cfg := copyCLIConfig()
	cmd := newCopyTestCommand(t)
	setCopyFlag(t, cmd, "no-input", "true")
	setCopyFlag(t, cmd, "size", "custom")
	setCopyFlag(t, cmd, "memory", "768MiB")
	setCopyFlag(t, cmd, "profile", "mixed")
	setCopyFlag(t, cmd, "allow-overcommit", "true")
	dest, err := resolveCopyDestination(cmd, cfg, "app")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	params := dest.params()
	if params["size"] != "custom" || params["memory"] != "768MiB" || params["profile"] != "mixed" || params["allow_overcommit"] != true {
		t.Fatalf("params = %#v", params)
	}
	if !strings.Contains(copyDestinationSummary(cfg, dest), "768 MiB") ||
		!strings.Contains(copyDestinationSummary(cfg, dest), "connections") {
		t.Fatalf("summary = %q", copyDestinationSummary(cfg, dest))
	}

	cmd = newCopyTestCommand(t)
	setCopyFlag(t, cmd, "no-input", "true")
	setCopyFlag(t, cmd, "size", "huge")
	if _, err := resolveCopyDestination(cmd, cfg, "app"); err == nil || errorCode(t, err) != output.CodeValidation {
		t.Fatalf("invalid size = %v, want validation_failed", err)
	}
	cmd = newCopyTestCommand(t)
	setCopyFlag(t, cmd, "no-input", "true")
	setCopyFlag(t, cmd, "size", "custom")
	if _, err := resolveCopyDestination(cmd, cfg, "app"); err == nil {
		t.Fatal("custom size without memory was accepted")
	}
	cmd = newCopyTestCommand(t)
	setCopyFlag(t, cmd, "no-input", "true")
	dest, err = resolveCopyDestination(cmd, cfg, "app")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := dest.params()["allow_overcommit"]; ok {
		t.Fatal("overcommit was implicit")
	}
}

func TestCopySummaryIsRedactedAndExplainsLimits(t *testing.T) {
	cfg := copyCLIConfig()
	dest := copyDestination{Name: "app", Size: "small"}
	pre := copyPreflightData{
		Source: copyPreflightSource{
			Endpoint:          "db.example.com:5432/app",
			URL:               "postgresql://source_user@db.example.com:5432/app?sslmode=verify-full",
			ServerVersionText: "16.4",
			Database:          "app",
			SizeBytes:         64 << 20,
			TLSMode:           backup.SourceTLSVerifyFull,
			Extensions:        []backup.SourceExtension{{Name: "plpgsql", Version: "1.0", Schema: "pg_catalog"}},
			Warnings:          []output.Warning{{Code: "source_tls_unverified", Message: "certificate is not verified"}},
		},
	}
	lines := strings.Join(copySummaryLines("prod", cfg, dest, pre), "\n")
	for _, want := range []string{
		"db.example.com:5432/app", "PostgreSQL 16.4", "not a storage quota",
		"Reachability", "destination node", "Snapshot", "Trusted source",
		"brand-new database", "verify-full",
	} {
		if !strings.Contains(strings.ToLower(lines), strings.ToLower(want)) {
			t.Errorf("summary is missing %q:\n%s", want, lines)
		}
	}
	for _, forbidden := range []string{"source-password", "sslmode=require"} {
		if strings.Contains(lines, forbidden) {
			t.Errorf("summary carries %q:\n%s", forbidden, lines)
		}
	}
}

func TestCopyStartFailureCredentialsOnlyWhenPossiblyUsed(t *testing.T) {
	secret, _, err := newDatabaseSecret(copyCLIConfig(), "app", steps.DatabaseRoles)
	if err != nil {
		t.Fatal(err)
	}
	const jobID = "01J0000000000000000000TEST"
	handovers := map[string]error{
		"lost reply": output.NewError(output.CodeTransportFailed, "the connection was lost", output.ExitTransport, ""),
		"job outcome unknown": output.NewError(output.CodeError, "job 01J ended without a result", output.ExitGeneric, "").
			WithDetails(map[string]any{"reason": node.JobOutcomeUnknown}),
		"node initial observation": output.NewError(output.CodeError, "the copy job was started but its progress could not be observed", output.ExitGeneric, "").
			WithDetails(map[string]any{"reason": node.JobOutcomeUnknown, "job_id": jobID, "observation_failed": true,
				"credentials_may_be_used": true, "phase": "unknown", "start": "confirmed", "import_outcome": "unknown"}),
		"node poll observation": output.NewError(output.CodeError, "the copy job was started but its progress could not be observed", output.ExitGeneric, "").
			WithDetails(map[string]any{"reason": node.JobOutcomeUnknown, "job_id": jobID, "observation_failed": true,
				"credentials_may_be_used": true, "phase": "unknown", "start": "confirmed", "import_outcome": "unknown", "target_present": true}),
		"node says may be used": output.NewError(output.CodeError, "the copy failed", output.ExitGeneric, "").
			WithDetails(map[string]any{"credentials_may_be_used": true}),
		"started job observation failed": output.NewError(output.CodeError, "the job's state could not be read", output.ExitGeneric, "").
			WithDetails(map[string]any{"reason": node.JobObservationFailed, "job_id": jobID, "observation_failed": true}),
		"started job record disappeared": output.NewError(output.CodePrecondition, "there is no job "+jobID, output.ExitPrecondition, "").
			WithDetails(map[string]any{"reason": "job_not_found", "job_id": jobID}),
		"import ran then cleanup failed": output.NewError(output.CodeError, "cleanup failed", output.ExitGeneric, "").
			WithDetails(map[string]any{"reason": "copy_cleanup_failed", "imported": true, "target_present": true}),
		"destination retained": output.NewError(output.CodeError, "provisioning failed", output.ExitGeneric, "").
			WithDetails(map[string]any{"reason": "copy_provision_failed", "target_present": true}),
	}
	for name, original := range handovers {
		cmd := newCopyTestCommand(t)
		out := cmd.OutOrStdout().(*bytes.Buffer)
		got := copyStartFailure(cmd, original, jobID, "app", secret, nil)
		var cliErr *output.CLIError
		if !errors.As(got, &cliErr) {
			t.Fatalf("%s: not a CLI error: %v", name, got)
		}
		details, _ := cliErr.Details.(map[string]any)
		shown, _ := details["database"].(databaseSecret)
		if shown.Roles["app"].Password != secret.Roles["app"].Password {
			t.Fatalf("%s: no credential handover", name)
		}
		if details["job_id"] != jobID {
			t.Fatalf("%s: job_id = %v, want %s", name, details["job_id"], jobID)
		}
		if note, _ := details["credentials_note"].(string); note == "" {
			t.Fatalf("%s: no credentials note", name)
		}
		if strings.Contains(cliErr.Error(), secret.Roles["app"].Password) {
			t.Fatalf("%s: the error message carries a password", name)
		}
		if strings.Contains(cliErr.Hint, "vaire apply") {
			t.Fatalf("%s: the copy hint reused db create's apply advice: %q", name, cliErr.Hint)
		}
		if !strings.Contains(out.String(), secret.Roles["app"].Password) {
			t.Fatalf("%s: the human output did not print the credentials", name)
		}
	}

	// A definite refusal before the job or target existed hands over
	// nothing and keeps its exact error.
	definite := output.NewError(output.CodePrecondition, "a cluster named app already exists", output.ExitPrecondition, "").
		WithDetails(map[string]any{"reason": "target_cluster_exists"})
	cmd := newCopyTestCommand(t)
	out := cmd.OutOrStdout().(*bytes.Buffer)
	got := copyStartFailure(cmd, definite, jobID, "app", secret, nil)
	if !errors.Is(got, definite) {
		t.Fatalf("a definite refusal changed: %v", got)
	}
	if out.Len() != 0 {
		t.Fatalf("a definite refusal printed credentials: %s", out.String())
	}

	// Structured output carries the one-time credentials only in details.
	cmd = newCopyTestCommand(t)
	setCopyFlag(t, cmd, "output", "json")
	out = cmd.OutOrStdout().(*bytes.Buffer)
	lost := output.NewError(output.CodeTransportFailed, "the connection was lost", output.ExitTransport, "")
	got = copyStartFailure(cmd, lost, jobID, "app", secret, nil)
	if out.Len() != 0 {
		t.Fatalf("structured output printed credentials: %s", out.String())
	}
	var cliErr *output.CLIError
	if !errors.As(got, &cliErr) {
		t.Fatal(got)
	}
	details, _ := cliErr.Details.(map[string]any)
	if _, ok := details["database"].(databaseSecret); !ok {
		t.Fatal("structured failure details lack database credentials")
	}

	cmd = newCopyTestCommand(t)
	setCopyFlag(t, cmd, "output", "json")
	out = cmd.OutOrStdout().(*bytes.Buffer)
	observation := output.NewError(output.CodeError, "the job's state could not be read", output.ExitGeneric, "").
		WithDetails(map[string]any{"reason": node.JobObservationFailed, "job_id": jobID, "observation_failed": true})
	got = copyStartFailure(cmd, observation, jobID, "app", secret, nil)
	if out.Len() != 0 {
		t.Fatalf("structured observation output printed credentials: %s", out.String())
	}
	if !errors.As(got, &cliErr) {
		t.Fatal(got)
	}
	details, _ = cliErr.Details.(map[string]any)
	if _, ok := details["database"].(databaseSecret); !ok {
		t.Fatal("structured observation failure details lack database credentials")
	}
}

// C06-02: a reattach whose job.follow cannot observe the started job returns
// an observation error rather than the original lost-stream error. The copy
// command still knows the job ID and must keep the generated credentials.
func TestCopyCredentialHandoffAfterReattachObservationFailure(t *testing.T) {
	secret, _, err := newDatabaseSecret(copyCLIConfig(), "app", steps.DatabaseRoles)
	if err != nil {
		t.Fatal(err)
	}
	id := rpc.NewRequestID()
	lost := output.NewError(output.CodeTransportFailed, "the connection to the node was lost", output.ExitTransport, "").
		WithDetails(map[string]any{"reason": "stream_lost"})
	followErr := output.NewError(output.CodeError, "the job's state could not be read", output.ExitGeneric, "").
		WithDetails(map[string]any{"reason": node.JobObservationFailed, "job_id": id, "observation_failed": true})
	seen := 0
	_, observed := reattachJob(context.Background(), id, lost, &seen,
		func(context.Context) error { return nil },
		func(context.Context, func(output.Event)) (remote.Reply, error) {
			return remote.Reply{}, followErr
		},
		func(output.Event) {}, func(string) {}, []time.Duration{0})
	var followCLI *output.CLIError
	if !errors.As(observed, &followCLI) {
		t.Fatalf("reattach did not return the observation failure: %v", observed)
	}

	cmd := newCopyTestCommand(t)
	out := cmd.OutOrStdout().(*bytes.Buffer)
	got := copyStartFailure(cmd, observed, id, "app", secret, nil)
	var cliErr *output.CLIError
	if !errors.As(got, &cliErr) {
		t.Fatalf("observation handoff = %v", got)
	}
	details, _ := cliErr.Details.(map[string]any)
	if details["job_id"] != id {
		t.Fatalf("job_id = %v, want %s", details["job_id"], id)
	}
	if shown, _ := details["database"].(databaseSecret); shown.Roles["app"].Password == "" {
		t.Fatal("the reattach observation failure lost the credentials")
	}
	if note, _ := details["credentials_note"].(string); !strings.Contains(note, "outcome is unknown") {
		t.Fatalf("credentials note = %q", note)
	}
	if !strings.Contains(out.String(), secret.Roles["app"].Password) {
		t.Fatal("human output did not print the credentials")
	}
}

// saveCopyTestCredentials stores an ordinary context's service and recovery
// secrets so executeDBCopy can top them up without touching the real home.
func saveCopyTestCredentials(t *testing.T, name string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	creds := contexts.Credentials{
		Database: &contexts.DatabaseCreds{
			Admin:    contexts.Role{User: "vaire_admin", Password: "admin-password"},
			Services: map[string]contexts.Role{},
		},
		Backup: &contexts.BackupCreds{
			AccessKeyID: "backup-key", SecretAccessKey: "backup-secret", Bucket: "bucket",
		},
	}
	creds.Backup.PgBackRest.CipherPass = "cipher-pass"
	for _, role := range steps.ServiceRoles {
		creds.Database.Services[role] = contexts.Role{User: role, Password: role + "-password"}
	}
	if err := contexts.SaveCredentials(name, creds); err != nil {
		t.Fatal(err)
	}
}

func copyTestPreflightReply() remote.Reply {
	data, _ := json.Marshal(map[string]any{
		"source": map[string]any{
			"endpoint":            "db.example.com:5432/app",
			"url":                 "postgresql://source_user@db.example.com:5432/app?sslmode=verify-full",
			"server_version_text": "16.4",
			"database":            "app",
			"size_bytes":          1024,
			"tls_mode":            backup.SourceTLSVerifyFull,
			"extensions":          []backup.SourceExtension{{Name: "plpgsql", Version: "1.0", Schema: "pg_catalog"}},
		},
		"target": map[string]any{
			"database":  map[string]any{"Name": "app", "Size": "small", "StatementTimeout": "60s"},
			"resources": map[string]any{"Size": "small", "MemoryMiB": 512, "CPUWeight": 50, "MaxConnections": 30},
			"advisory":  true,
		},
		"source_checked": true,
	})
	return remote.Reply{Data: data}
}

func TestExecuteDBCopyDryRunAsksOnlyForDestinationValidation(t *testing.T) {
	cmd := newCopyTestCommand(t)
	setCopyFlag(t, cmd, "no-input", "true")
	setCopyFlag(t, cmd, "dry-run", "true")
	dest := copyDestination{Name: "app", Size: "small"}
	var preflightCalls, startCalls int
	var startedSecrets map[string]string
	var startedRequestID string
	var startedDry bool
	calls := copyRPC{
		Preflight: func(context.Context, map[string]any, map[string]string) (remote.Reply, error) {
			preflightCalls++
			return remote.Reply{}, errors.New("dry run must not preflight the source")
		},
		Start: func(_ context.Context, params map[string]any, sec map[string]string, requestID, confirm string, dry bool) (remote.Reply, string, bool, error) {
			startCalls++
			startedSecrets, startedRequestID, startedDry = sec, requestID, dry
			if params["name"] != "app" || params["size"] != "small" {
				t.Fatalf("dry-run params = %#v", params)
			}
			if confirm != "" {
				t.Fatalf("dry run asked to confirm %q", confirm)
			}
			return remote.Reply{Data: []byte(`{"dry_run":true,"source_checked":false}`)}, "", false, nil
		},
	}
	p := newProgress(cmd)
	if err := executeDBCopy(cmd, p, "prod", copyCLIConfig(), dest, nil, nil, calls); err != nil {
		t.Fatal(err)
	}
	if preflightCalls != 0 || startCalls != 1 {
		t.Fatalf("calls: preflight %d, start %d", preflightCalls, startCalls)
	}
	if startedSecrets != nil || startedRequestID != "" || !startedDry {
		t.Fatalf("dry run started a real request: secrets %v, id %q, dry %v", startedSecrets, startedRequestID, startedDry)
	}
	if text := cmd.OutOrStdout().(*bytes.Buffer).String(); !strings.Contains(text, "no source connection") {
		t.Fatalf("dry-run message = %q", text)
	}
}

func TestExecuteDBCopyPreflightFailureProvisionsNothing(t *testing.T) {
	cmd := newCopyTestCommand(t)
	setCopyFlag(t, cmd, "no-input", "true")
	setCopyFlag(t, cmd, "confirm", "app")
	source := &copySourceInput{URL: "postgresql://source_user:source-password@db.example.com:5432/app?sslmode=verify-full"} //nolint:gosec // G101: a test fixture, not a credential.
	var startCalls int
	calls := copyRPC{
		Preflight: func(context.Context, map[string]any, map[string]string) (remote.Reply, error) {
			return remote.Reply{}, output.NewError(output.CodePrecondition, "the source refused the connection", output.ExitPrecondition, "")
		},
		Start: func(context.Context, map[string]any, map[string]string, string, string, bool) (remote.Reply, string, bool, error) {
			startCalls++
			return remote.Reply{}, "", false, nil
		},
	}
	p := newProgress(cmd)
	err := executeDBCopy(cmd, p, "prod", copyCLIConfig(), copyDestination{Name: "app"}, source, nil, calls)
	if err == nil || errorCode(t, err) != output.CodePrecondition {
		t.Fatalf("preflight failure = %v", err)
	}
	if startCalls != 0 {
		t.Fatal("a failed preflight still started the copy")
	}
}

func TestExecuteDBCopySuccessCarriesCredentialsAndServiceSecrets(t *testing.T) {
	saveCopyTestCredentials(t, "prod")
	cmd := newCopyTestCommand(t)
	setCopyFlag(t, cmd, "no-input", "true")
	setCopyFlag(t, cmd, "confirm", "app")
	setCopyFlag(t, cmd, "size", "small")
	setCopyFlag(t, cmd, "output", "json")
	source := &copySourceInput{URL: "postgresql://source_user:source-password@db.example.com:5432/app?sslmode=verify-full"} //nolint:gosec // G101: a test fixture, not a credential.
	var order []string
	var jobID string
	var jobSecrets map[string]string
	calls := copyRPC{
		Preflight: func(_ context.Context, params map[string]any, sec map[string]string) (remote.Reply, error) {
			order = append(order, "preflight")
			if params["name"] != "app" {
				t.Fatalf("preflight params = %#v", params)
			}
			if sec[node.CopySourceURLSecret] == "" || sec[node.CopySourceSpecSecret] != "" {
				t.Fatalf("preflight source secrets = %#v", sec)
			}
			return copyTestPreflightReply(), nil
		},
		Start: func(_ context.Context, params map[string]any, sec map[string]string, requestID, confirm string, dry bool) (remote.Reply, string, bool, error) {
			order = append(order, "start")
			jobID, jobSecrets = requestID, sec
			if dry {
				t.Fatal("success path was marked dry")
			}
			if confirm != "app" {
				t.Fatalf("confirm = %q", confirm)
			}
			if len(requestID) != 26 {
				t.Fatalf("preselected request id = %q", requestID)
			}
			result := map[string]any{
				"database": map[string]any{"Name": "app", "Size": "small", "StatementTimeout": "60s"},
				"target":   map[string]any{"in_config": true, "known": true},
				"phase":    "succeeded",
				"imported": true,
				"backup":   map[string]any{"configured": true, "ready": true},
			}
			raw, _ := json.Marshal(result)
			envelope, _ := json.Marshal(map[string]any{
				"job_id": requestID, "job": "db-copy", "state": "succeeded", "result": json.RawMessage(raw),
			})
			return remote.Reply{Data: envelope}, requestID, false, nil
		},
	}
	p := newProgress(cmd)
	if err := executeDBCopy(cmd, p, "prod", copyCLIConfig(), copyDestination{Name: "app", Size: "small"}, source, nil, calls); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "preflight,start" {
		t.Fatalf("call order = %v", order)
	}
	for _, key := range []string{
		steps.SecretDatabaseRole("app", "migrate"), steps.SecretDatabaseRole("app", "app"), steps.SecretDatabaseRole("app", "ro"),
		steps.SecretAdminPassword, steps.SecretBackupKeyID, steps.SecretBackupKeySecret, steps.SecretBackupCipherPass,
		node.CopySourceURLSecret,
	} {
		if jobSecrets[key] == "" {
			t.Errorf("job secrets are missing %s", key)
		}
	}
	if jobSecrets[node.CopySourceSpecSecret] != "" {
		t.Error("the job carried both source forms")
	}

	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(cmd.OutOrStdout().(*bytes.Buffer).Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data["job_id"] != jobID {
		t.Fatalf("job id = %v, want %s", env.Data["job_id"], jobID)
	}
	secret, ok := env.Data["database"].(map[string]any)
	if !ok {
		t.Fatalf("database credentials = %T", env.Data["database"])
	}
	roles, _ := secret["roles"].(map[string]any)
	for _, role := range steps.DatabaseRoles {
		entry, _ := roles[role].(map[string]any)
		if entry == nil || entry["password"] == "" {
			t.Errorf("role %s has no password in the handoff", role)
		}
	}
	if env.Data["backup"] == nil {
		t.Error("the successful copy did not report its backup state")
	}
}

func TestExecuteDBCopyDetachedKeepsJobIDAndCredentials(t *testing.T) {
	saveCopyTestCredentials(t, "prod")
	cmd := newCopyTestCommand(t)
	setCopyFlag(t, cmd, "no-input", "true")
	setCopyFlag(t, cmd, "confirm", "app")
	setCopyFlag(t, cmd, "output", "json")
	source := &copySourceInput{URL: "postgresql://source_user:source-password@db.example.com:5432/app?sslmode=verify-full"} //nolint:gosec // G101: a test fixture, not a credential.
	var jobID string
	calls := copyRPC{
		Preflight: func(context.Context, map[string]any, map[string]string) (remote.Reply, error) {
			return copyTestPreflightReply(), nil
		},
		Start: func(_ context.Context, _ map[string]any, _ map[string]string, requestID, _ string, _ bool) (remote.Reply, string, bool, error) {
			jobID = requestID
			return remote.Reply{}, requestID, true, nil
		},
	}
	p := newProgress(cmd)
	if err := executeDBCopy(cmd, p, "prod", copyCLIConfig(), copyDestination{Name: "app"}, source, nil, calls); err != nil {
		t.Fatal(err)
	}
	if len(jobID) != 26 {
		t.Fatalf("job id = %q", jobID)
	}
	var env struct {
		Data    map[string]any `json:"data"`
		Message string         `json:"message"`
		Next    []string       `json:"next"`
	}
	if err := json.Unmarshal(cmd.OutOrStdout().(*bytes.Buffer).Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data["job_id"] != jobID || env.Data["detached"] != true {
		t.Fatalf("detached data = %#v", env.Data)
	}
	secret, ok := env.Data["database"].(map[string]any)
	if !ok {
		t.Fatal("detached output lost the credentials")
	}
	roles, _ := secret["roles"].(map[string]any)
	if roles["app"] == nil {
		t.Fatal("detached output lost a role")
	}
	if env.Message != "" {
		t.Fatalf("JSON detached output unexpectedly carried a human message: %q", env.Message)
	}
	if len(env.Next) == 0 || env.Next[0] != "vaire job follow "+jobID {
		t.Fatalf("detached next = %v", env.Next)
	}
}

func TestExecuteDBCopyLostStartReplyKeepsCredentials(t *testing.T) {
	saveCopyTestCredentials(t, "prod")
	cmd := newCopyTestCommand(t)
	setCopyFlag(t, cmd, "no-input", "true")
	setCopyFlag(t, cmd, "confirm", "app")
	setCopyFlag(t, cmd, "output", "json")
	source := &copySourceInput{URL: "postgresql://source_user:source-password@db.example.com:5432/app?sslmode=verify-full"} //nolint:gosec // G101: a test fixture, not a credential.
	var jobID string
	calls := copyRPC{
		Preflight: func(context.Context, map[string]any, map[string]string) (remote.Reply, error) {
			return copyTestPreflightReply(), nil
		},
		Start: func(_ context.Context, _ map[string]any, _ map[string]string, requestID, _ string, _ bool) (remote.Reply, string, bool, error) {
			jobID = requestID
			return remote.Reply{}, "", false, output.NewError(output.CodeTransportFailed,
				"the connection to the node was lost", output.ExitTransport, "check the job list")
		},
	}
	p := newProgress(cmd)
	err := executeDBCopy(cmd, p, "prod", copyCLIConfig(), copyDestination{Name: "app"}, source, nil, calls)
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("lost reply = %v", err)
	}
	details, _ := cliErr.Details.(map[string]any)
	if details["job_id"] != jobID {
		t.Fatalf("lost reply job_id = %v, want %s", details["job_id"], jobID)
	}
	secret, ok := details["database"].(databaseSecret)
	if !ok || secret.Roles["app"].Password == "" {
		t.Fatal("lost reply lost the generated credentials")
	}
	if strings.Contains(cliErr.Error(), secret.Roles["app"].Password) {
		t.Fatal("lost reply error message carries a password")
	}
}

func TestExecuteDBCopyUnknownOutcomeCarriesJobID(t *testing.T) {
	saveCopyTestCredentials(t, "prod")
	cmd := newCopyTestCommand(t)
	setCopyFlag(t, cmd, "no-input", "true")
	setCopyFlag(t, cmd, "confirm", "app")
	setCopyFlag(t, cmd, "output", "json")
	source := &copySourceInput{URL: "postgresql://source_user:source-password@db.example.com:5432/app?sslmode=verify-full"} //nolint:gosec // G101: a test fixture, not a credential.
	var jobID string
	calls := copyRPC{
		Preflight: func(context.Context, map[string]any, map[string]string) (remote.Reply, error) {
			return copyTestPreflightReply(), nil
		},
		Start: func(_ context.Context, _ map[string]any, _ map[string]string, requestID, _ string, _ bool) (remote.Reply, string, bool, error) {
			jobID = requestID
			return remote.Reply{}, "", false, output.NewError(output.CodeError,
				"the copy job's start could not be confirmed", output.ExitGeneric,
				"Keep the generated credentials and the job id").
				WithDetails(map[string]any{
					"reason": node.JobOutcomeUnknown, "job_id": requestID,
					"credentials_may_be_used": true, "import_outcome": "unknown",
					"observation_failed": true, "start": "confirmed",
				})
		},
	}
	p := newProgress(cmd)
	err := executeDBCopy(cmd, p, "prod", copyCLIConfig(), copyDestination{Name: "app"}, source, nil, calls)
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("unknown outcome = %v", err)
	}
	details, _ := cliErr.Details.(map[string]any)
	if details["job_id"] != jobID {
		t.Fatalf("job_id = %v, want %s", details["job_id"], jobID)
	}
	if note, _ := details["credentials_note"].(string); !strings.Contains(note, "outcome is unknown") {
		t.Fatalf("credentials note = %q", note)
	}
	if _, ok := details["database"].(databaseSecret); !ok {
		t.Fatal("unknown outcome lost the credentials")
	}
}

func TestExecuteDBCopyUnreadableSuccessKeepsCredentials(t *testing.T) {
	saveCopyTestCredentials(t, "prod")
	cmd := newCopyTestCommand(t)
	setCopyFlag(t, cmd, "no-input", "true")
	setCopyFlag(t, cmd, "confirm", "app")
	setCopyFlag(t, cmd, "output", "json")
	source := &copySourceInput{URL: "postgresql://source_user:source-password@db.example.com:5432/app?sslmode=verify-full"} //nolint:gosec // G101: a test fixture, not a credential.
	var jobID string
	calls := copyRPC{
		Preflight: func(context.Context, map[string]any, map[string]string) (remote.Reply, error) {
			return copyTestPreflightReply(), nil
		},
		Start: func(_ context.Context, _ map[string]any, _ map[string]string, requestID, _ string, _ bool) (remote.Reply, string, bool, error) {
			jobID = requestID
			return remote.Reply{Data: []byte("not a job envelope")}, requestID, false, nil
		},
	}
	p := newProgress(cmd)
	err := executeDBCopy(cmd, p, "prod", copyCLIConfig(), copyDestination{Name: "app"}, source, nil, calls)
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("unreadable result = %v", err)
	}
	details, _ := cliErr.Details.(map[string]any)
	if details["job_id"] != jobID || details["reason"] != "copy_result_unreadable" {
		t.Fatalf("details = %#v", details)
	}
	if secret, _ := details["database"].(databaseSecret); secret.Roles["app"].Password == "" {
		t.Fatal("an unreadable successful result lost the credentials")
	}
}

func TestExecuteDBCopyHumanSuccessHandsOffCredentials(t *testing.T) {
	saveCopyTestCredentials(t, "prod")
	cmd := newCopyTestCommand(t)
	setCopyFlag(t, cmd, "no-input", "true")
	setCopyFlag(t, cmd, "confirm", "app")
	source := &copySourceInput{URL: "postgresql://source_user:source-password@db.example.com:5432/app?sslmode=verify-full"} //nolint:gosec // G101: a test fixture, not a credential.
	calls := copyRPC{
		Preflight: func(context.Context, map[string]any, map[string]string) (remote.Reply, error) {
			return copyTestPreflightReply(), nil
		},
		Start: func(_ context.Context, _ map[string]any, _ map[string]string, requestID, _ string, _ bool) (remote.Reply, string, bool, error) {
			raw, _ := json.Marshal(map[string]any{
				"job_id": requestID, "job": "db-copy", "state": "succeeded",
				"result": map[string]any{"database": map[string]any{"Name": "app"}, "phase": "succeeded", "imported": true},
			})
			return remote.Reply{Data: raw}, requestID, false, nil
		},
	}
	p := newProgress(cmd)
	if err := executeDBCopy(cmd, p, "prod", copyCLIConfig(), copyDestination{Name: "app"}, source, nil, calls); err != nil {
		t.Fatal(err)
	}
	text := cmd.OutOrStdout().(*bytes.Buffer).String()
	if !strings.Contains(text, "app password:") {
		t.Fatalf("human output has no role password:\n%s", text)
	}
	if !strings.Contains(text, "Store these now") {
		t.Fatalf("human output has no one-time note:\n%s", text)
	}
	if strings.Contains(text, "source-password") {
		t.Fatal("human output leaked the source password")
	}
}
