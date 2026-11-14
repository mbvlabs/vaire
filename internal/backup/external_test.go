package backup

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/pg"
	"github.com/mbvlabs/vaire/internal/secrets"
)

func newTestWorkspace(t *testing.T) *CopyWorkspace {
	t.Helper()
	root := t.TempDir()
	ws, err := NewCopyWorkspace(root, "job-1", CurrentWorkspaceOwner(), func(string, int, int) error { return nil })
	if err != nil {
		t.Fatalf("NewCopyWorkspace: %v", err)
	}
	t.Cleanup(func() { _ = ws.Remove() })
	return ws
}

func testSource(t *testing.T, password string) Source {
	t.Helper()
	src, err := NewSourceWithRegistry(secrets.NewRegistry(), SourceSpec{
		Host:     "db.example.com",
		Port:     5432,
		Database: "app",
		User:     "source_user",
		Password: password,
		TLSMode:  SourceTLSVerifyFull,
	})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	return src
}

func errorReason(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		return ""
	}
	details, ok := cliErr.Details.(map[string]any)
	if !ok {
		return ""
	}
	reason, _ := details["reason"].(string)
	return reason
}

func errorDetails(t *testing.T, err error) map[string]any {
	t.Helper()
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error is not a *output.CLIError: %v", err)
	}
	details, _ := cliErr.Details.(map[string]any)
	return details
}

type recordedCall struct {
	step   string
	cmd    engine.Command
	stdin  []byte
	stdout io.Writer
}

type fakeExecutor struct {
	calls  []*recordedCall
	handle func(*recordedCall) error
	logs   []string
}

func (f *fakeExecutor) Exec(_ context.Context, stepID string, c engine.Command, report engine.Reporter) error {
	if strings.HasSuffix(stepID, "-workspace-probe") {
		_, _ = io.WriteString(c.Stdout, "readable")
		return nil
	}
	call := &recordedCall{step: stepID, cmd: c, stdout: c.Stdout}
	if c.Stdin != nil {
		call.stdin, _ = io.ReadAll(c.Stdin)
	}
	f.calls = append(f.calls, call)
	if report != nil {
		for _, line := range f.logs {
			report(output.Event{Type: "log", Step: stepID, Line: line})
		}
	}
	if f.handle != nil {
		return f.handle(call)
	}
	return nil
}

type eventRecorder struct {
	events []output.Event
}

func (r *eventRecorder) report(ev output.Event) {
	r.events = append(r.events, ev)
}

func (r *eventRecorder) logLines() []string {
	var out []string
	for _, ev := range r.events {
		if ev.Type == "log" {
			out = append(out, ev.Line)
		}
	}
	return out
}

func (f *fakeExecutor) callContaining(t *testing.T, needle string) *recordedCall {
	t.Helper()
	for _, call := range f.calls {
		if strings.Contains(call.cmd.Name+" "+strings.Join(call.cmd.Args, " "), needle) {
			return call
		}
	}
	t.Fatalf("no command contains %q; calls: %v", needle, f.describe())
	return nil
}

func (f *fakeExecutor) describe() []string {
	var out []string
	for _, call := range f.calls {
		out = append(out, call.cmd.Name+" "+strings.Join(call.cmd.Args, " "))
	}
	return out
}

func envArg(cmd engine.Command, key string) string {
	for _, arg := range cmd.Args {
		if strings.HasPrefix(arg, key+"=") {
			return strings.TrimPrefix(arg, key+"=")
		}
	}
	return ""
}

func writeFakeTOC(c *recordedCall, database string) {
	_, _ = io.WriteString(c.stdout, ";\n;     dbname: "+database+"\n;     Format: CUSTOM\n;\n1; 1262 0 DATABASE - "+database+" owner\n2; 1259 16390 TABLE public users owner\n")
}

func TestParseSourceURLNormalizesAndRedacts(t *testing.T) {
	reg := secrets.NewRegistry()
	raw := "postgresql://source%5Fuser:p%40ss%3Aword@db.example.com:5433/app?sslmode=verify-full"
	src, err := ParseSourceURLWithRegistry(reg, raw)
	if err != nil {
		t.Fatalf("ParseSourceURL: %v", err)
	}
	if src.Host != "db.example.com" || src.Port != 5433 || src.Database != "app" || src.User != "source_user" {
		t.Fatalf("parsed source fields: %+v", src)
	}
	if src.password != "p@ss:word" {
		t.Fatalf("decoded password %q", src.password)
	}
	if src.TLSMode != SourceTLSVerifyFull {
		t.Fatalf("tls mode %q", src.TLSMode)
	}
	if got := src.passfileLine(); got != "db.example.com:5433:app:source_user:p@ss\\:word\n" {
		t.Fatalf("passfile line %q", got)
	}
	if strings.Contains(reg.Redact(raw), "p%40ss") || strings.Contains(reg.Redact("the password is p@ss:word"), "p@ss:word") {
		t.Fatalf("raw or decoded password was not redacted")
	}
	if redacted := fmt.Sprintf("%v %+v %#v %s", src, src, src, src); strings.Contains(redacted, "p@ss") {
		t.Fatalf("fmt leaked the password: %s", redacted)
	}
	if strings.Contains(src.RedactedURL(), "p@ss") || !strings.Contains(src.RedactedURL(), "source_user") {
		t.Fatalf("redacted URL %q", src.RedactedURL())
	}
}

func TestParseSourceURLEscapingAndDefaults(t *testing.T) {
	reg := secrets.NewRegistry()
	src, err := ParseSourceURLWithRegistry(reg, "postgres://u:a%5Cb@[::1]/app%3Aprod")
	if err != nil {
		t.Fatalf("ParseSourceURL: %v", err)
	}
	if src.Host != "::1" || src.Port != 5432 || src.Database != "app:prod" || src.password != `a\b` {
		t.Fatalf("parsed source: %+v", src)
	}
	want := `\:\:1:5432:app\:prod:u:a\\b` + "\n"
	if got := src.passfileLine(); got != want {
		t.Fatalf("passfile line %q, want %q", got, want)
	}
	if src.TLSMode != SourceTLSVerifyFull {
		t.Fatalf("default TLS mode %q", src.TLSMode)
	}
}

func TestParseSourceURLAllowsMissingPassword(t *testing.T) {
	src, err := ParseSourceURLWithRegistry(secrets.NewRegistry(), "postgresql://u@db.example.com/app")
	if err != nil {
		t.Fatalf("ParseSourceURL: %v", err)
	}
	if src.password != "" {
		t.Fatalf("password %q, want empty", src.password)
	}
	if got := src.passfileLine(); got != "db.example.com:5432:app:u:\n" {
		t.Fatalf("passfile line %q", got)
	}
}

func TestParseSourceURLRejectsUnsafeInput(t *testing.T) {
	cases := []string{
		"mysql://u:password@host/db",
		"postgresql:///db",
		"postgresql://u:password@host",
		"postgresql://u:password@host/db/extra",
		"postgresql://u:password@host/db?options=-c%20x",
		"postgresql://u:password@host/db?sslmode=disable",
		"postgresql://u:password@host/db?sslmode=allow",
		"postgresql://u:password@host/db?sslmode=prefer",
		"postgresql://u:password@host/db?sslmode=require&sslmode=verify-full",
		"postgresql://u:password@host/db?passfile=%2Ftmp%2Fx",
		"postgresql://u:password@host,other/db",
		"postgresql://u:password@%2Fvar%2Frun/db",
		"postgresql://u:password@host:0/db",
		"postgresql://u:password@host:65536/db",
		"postgresql://u:password@host/db#fragment",
		"postgresql://u:password@host/db%00",
		"postgresql://u:password@host/db?sslmode=verify-ca&sslmode=require",
		"postgresql://short:secret@host:abc/db",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			_, err := ParseSourceURLWithRegistry(secrets.NewRegistry(), raw)
			if err == nil {
				t.Fatalf("expected error for %q", raw)
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "password@") {
				t.Fatalf("error leaked URL credentials: %v", err)
			}
		})
	}
}

func TestSourceFieldsValidation(t *testing.T) {
	base := SourceSpec{Host: "db.example.com", Database: "app", User: "u", TLSMode: SourceTLSVerifyFull}
	cases := []SourceSpec{
		{Host: "db,other", Database: "app", User: "u"},
		{Host: "db/host", Database: "app", User: "u"},
		{Host: "db host", Database: "app", User: "u"},
		{Host: "-bad.example.com", Database: "app", User: "u"},
		{Host: "db.example.com", Database: "app=evil", User: "u"},
		{Host: "db.example.com", Database: "*", User: "u"},
		{Host: "db.example.com", Database: "app", User: "*"},
		{Host: "*", Database: "app", User: "u"},
		{Host: "db.example.com", Database: "postgresql://evil", User: "u"},
		{Host: "db.example.com", Database: " app", User: "u"},
		{Host: "db.example.com", Database: "", User: "u"},
		{Host: "db.example.com", Database: "app", User: ""},
		{Host: "db.example.com", Database: "app", User: "u\x00"},
		{Host: "db.example.com", Database: "app", User: "u", Password: "a\nb"},
		{Host: "db.example.com", Port: 70000, Database: "app", User: "u"},
	}
	for i, spec := range cases {
		if _, err := NewSourceWithRegistry(secrets.NewRegistry(), spec); err == nil {
			t.Errorf("case %d (%+v) was accepted", i, spec)
		}
	}
	if _, err := NewSourceWithRegistry(secrets.NewRegistry(), base); err != nil {
		t.Fatalf("base source rejected: %v", err)
	}
}

func TestSourceTLSModesWarningsAndCA(t *testing.T) {
	ca := testCertificatePEM(t)
	for _, tc := range []struct {
		mode    string
		ca      []byte
		warning string
	}{
		{mode: SourceTLSVerifyFull},
		{mode: SourceTLSVerifyCA, warning: "source_tls_hostname_unverified"},
		{mode: SourceTLSRequire, warning: "source_tls_unverified"},
		{mode: SourceTLSVerifyFull, ca: ca},
		{mode: SourceTLSVerifyCA, ca: ca, warning: "source_tls_hostname_unverified"},
	} {
		src, err := NewSourceWithRegistry(secrets.NewRegistry(), SourceSpec{
			Host: "db.example.com", Database: "app", User: "u", Password: "secret-password", TLSMode: tc.mode, CACert: tc.ca,
		})
		if err != nil {
			t.Fatalf("%s: %v", tc.mode, err)
		}
		warning, ok := src.TLSWarning()
		if tc.warning == "" {
			if ok {
				t.Fatalf("%s: unexpected warning %+v", tc.mode, warning)
			}
		} else if !ok || warning.Code != tc.warning {
			t.Fatalf("%s: warning %+v, want %s", tc.mode, warning, tc.warning)
		}
	}
	if _, err := NewSourceWithRegistry(secrets.NewRegistry(), SourceSpec{
		Host: "db.example.com", Database: "app", User: "u", TLSMode: SourceTLSRequire, CACert: ca,
	}); err == nil {
		t.Fatal("CA with sslmode=require was accepted")
	}
	if _, err := NewSourceWithRegistry(secrets.NewRegistry(), SourceSpec{
		Host: "db.example.com", Database: "app", User: "u", TLSMode: SourceTLSVerifyFull, CACert: []byte("not a certificate"),
	}); err == nil {
		t.Fatal("invalid CA was accepted")
	}
}

// A URL source carries no CA in its own form; the CLI's separately supplied
// CA secret attaches it through one validated helper. The CA content must be
// validated in the source's declared TLS mode, and a second certificate must
// not silently replace the first.
func TestSourceWithCACert(t *testing.T) {
	ca := testCertificatePEM(t)
	base, err := NewSourceWithRegistry(secrets.NewRegistry(), SourceSpec{
		Host: "db.example.com", Database: "app", User: "u", Password: "secret-password", TLSMode: SourceTLSVerifyFull,
	})
	if err != nil {
		t.Fatal(err)
	}
	withCA, err := base.WithCACert(ca)
	if err != nil {
		t.Fatalf("WithCACert: %v", err)
	}
	if string(withCA.CACert()) != string(ca) {
		t.Fatal("the CA was not attached")
	}
	if _, err := withCA.WithCACert(ca); err == nil {
		t.Fatal("a second CA replaced the first")
	}
	require, err := NewSourceWithRegistry(secrets.NewRegistry(), SourceSpec{
		Host: "db.example.com", Database: "app", User: "u", TLSMode: SourceTLSRequire,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := require.WithCACert(ca); err == nil {
		t.Fatal("a CA with sslmode=require was accepted")
	}
	invalid, err := base.WithCACert([]byte("not a certificate"))
	if err == nil || invalid.CACert() != nil {
		t.Fatalf("an invalid CA was accepted: %v", err)
	}
}

func TestSourceCommandUsesIsolatedEnvironment(t *testing.T) {
	t.Setenv("PGPASSFILE", "/tmp/evil-pass")
	t.Setenv("PGSSLROOTCERT", "/tmp/evil-root")
	t.Setenv("PGSERVICE", "evil")
	t.Setenv("PGPASSWORD", "evil")
	ws := newTestWorkspace(t)
	src := testSource(t, "secret-password")
	access, err := prepareSourceAccess(ws, src)
	if err != nil {
		t.Fatalf("prepareSourceAccess: %v", err)
	}
	cmd := sourceCommand(access, sourceTool("pg_dump"), sourceDumpArgs(src))
	if cmd.Env != nil {
		t.Fatalf("command env map = %v, want nil", cmd.Env)
	}
	joined := strings.Join(cmd.Args, " ")
	for _, forbidden := range []string{"/tmp/evil-pass", "/tmp/evil-root", "evil", "secret-password", "PGPASSWORD"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("command args contain %q: %s", forbidden, joined)
		}
	}
	env := access.envValues()
	wantKeys := map[string]bool{
		"HOME": true, "PGPASSFILE": true, "PGCONNECT_TIMEOUT": true, "PGGSSENCMODE": true,
		"PGSSLMODE": true, "PGSSLROOTCERT": true, "PGAPPNAME": true, "PGCLIENTENCODING": true,
	}
	for key := range env {
		if !wantKeys[key] {
			t.Fatalf("unexpected source env key %q", key)
		}
	}
	if env["PGGSSENCMODE"] != "disable" || env["PGSSLMODE"] != SourceTLSVerifyFull {
		t.Fatalf("TLS environment: %v", env)
	}
	if env["HOME"] == "/root" || env["HOME"] == "" {
		t.Fatalf("HOME = %q, want a private directory", env["HOME"])
	}
	if env["PGSSLROOTCERT"] != "system" {
		t.Fatalf("PGSSLROOTCERT = %q, want system", env["PGSSLROOTCERT"])
	}
	passInfo, err := os.Stat(env["PGPASSFILE"])
	if err != nil {
		t.Fatalf("passfile stat: %v", err)
	}
	if passInfo.Mode().Perm() != 0o600 {
		t.Fatalf("passfile mode %o", passInfo.Mode().Perm())
	}
	pass, err := os.ReadFile(env["PGPASSFILE"]) //nolint:gosec // the test just created the private passfile.
	if err != nil {
		t.Fatalf("read passfile: %v", err)
	}
	if string(pass) != "db.example.com:5432:app:source_user:secret-password\n" {
		t.Fatalf("passfile content %q", pass)
	}
	if err := access.remove(); err != nil {
		t.Fatalf("remove source access: %v", err)
	}
	if _, err := os.Stat(env["PGPASSFILE"]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("passfile survived cleanup: %v", err)
	}
}

func TestSourceAccessTLSFiles(t *testing.T) {
	ws := newTestWorkspace(t)
	ca := testCertificatePEM(t)
	src, err := NewSourceWithRegistry(secrets.NewRegistry(), SourceSpec{
		Host: "db.example.com", Database: "app", User: "u", Password: "secret-password",
		TLSMode: SourceTLSVerifyFull, CACert: ca,
	})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	access, err := prepareSourceAccess(ws, src)
	if err != nil {
		t.Fatalf("prepareSourceAccess: %v", err)
	}
	env := access.envValues()
	caPath := env["PGSSLROOTCERT"]
	if caPath == "" || caPath == "system" {
		t.Fatalf("CA path %q", caPath)
	}
	info, err := os.Stat(caPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("CA file stat %v mode %v", err, info)
	}
	got, err := os.ReadFile(caPath) //nolint:gosec // the test just created the private CA file.
	if err != nil || !bytes.Equal(got, ca) {
		t.Fatalf("CA content mismatch: %v", err)
	}
	if err := access.remove(); err != nil {
		t.Fatalf("remove source access: %v", err)
	}
}

type accessProbeExecutor struct {
	denied  string
	calls   []string
	failure error
	result  string
}

func (e *accessProbeExecutor) Exec(_ context.Context, step string, cmd engine.Command, _ engine.Reporter) error {
	if !strings.HasSuffix(step, "-workspace-probe") {
		e.calls = append(e.calls, "source")
		return errors.New("unexpected source callback")
	}
	if cmd.Name != "runuser" || len(cmd.Args) != 8 || cmd.Args[5] != sourceAccessProbeScript {
		return errors.New("invalid probe")
	}
	label := filepath.Base(cmd.Args[7])
	if cmd.Args[7] == systemCABundlePath {
		label = copySystemCALabel
	}
	e.calls = append(e.calls, label)
	if e.failure != nil {
		return e.failure
	}
	if label == e.denied {
		_, _ = io.WriteString(cmd.Stdout, "denied")
	} else if e.result != "" {
		_, _ = io.WriteString(cmd.Stdout, e.result)
	} else {
		_, _ = io.WriteString(cmd.Stdout, "readable")
	}
	return nil
}

func TestSourceAccessProbeRefusesBeforeSource(t *testing.T) {
	for _, operation := range []string{"preflight", "export"} {
		for _, denied := range []string{copyPassfileName, copyCAName} {
			t.Run(operation+"/"+denied, func(t *testing.T) {
				ws := newTestWorkspace(t)
				src, err := testSource(t, "sensitive-password").WithCACert(testCertificatePEM(t))
				if err != nil {
					t.Fatal(err)
				}
				exec := &accessProbeExecutor{denied: denied}
				if operation == "preflight" {
					_, err = PreflightSource(context.Background(), exec, "copy", nil, src, ws)
				} else {
					_, err = ExportSource(context.Background(), exec, "copy", nil, src, ws)
				}
				if errorReason(t, err) != "copy_workspace_access_denied" {
					t.Fatalf("unexpected failure: %v", err)
				}
				details := errorDetails(t, err)
				if details["stage"] != "source-access" || details["file"] != denied {
					t.Fatalf("unexpected details: %v", details)
				}
				if strings.Contains(fmt.Sprint(err, details), "sensitive-password") || strings.Contains(fmt.Sprint(err, details), ws.Dir) {
					t.Fatal("private information escaped")
				}
				for _, call := range exec.calls {
					if call == "source" {
						t.Fatal("source callback ran")
					}
				}
				for _, name := range []string{copyPassfileName, copyCAName, copyArchiveName} {
					if _, err := ws.Stat(name); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("%s survived: %v", name, err)
					}
				}
			})
		}
	}
}

func TestLocalSourceFileClassification(t *testing.T) {
	for _, tc := range []struct {
		line, reason string
	}{
		{`psql: error: connection to server at "db.example.com", port 5432 failed: could not open root certificate file "/var/lib/vaire-copies/job-1/source-ca.pem": Permission denied`, "copy_workspace_access_denied"},
		{`pg_dump: warning: could not open password file "/var/lib/vaire-copies/job-1/source.pgpass": Permission denied`, "copy_workspace_access_denied"},
		{`psql: error: could not read root certificate file "/etc/ssl/certs/ca-certificates.crt": Permission denied`, "copy_workspace_access_denied"},
		{`SSL error: certificate verify failed`, "source_tls_failed"},
		{`password authentication failed for user`, "source_auth_failed"},
		{`ERROR: permission denied for table source_ca`, "source_preflight_failed"},
		{`pg_dump: error: query failed: ERROR: permission denied for table "could not open root certificate file source-ca.pem"`, "source_preflight_failed"},
		{`psql:<stdin>:2: ERROR: permission denied for table "could not open root certificate file source-ca.pem"`, "source_preflight_failed"},
		{`pg_dump: error: query failed: ERROR: permission denied for table "could not open root certificate file ""/var/lib/vaire-copies/job-1/source-ca.pem"": Permission denied"`, "source_preflight_failed"},
	} {
		for _, classifier := range []struct {
			classify func(error) error
			fallback string
		}{{sourcePreflightError, "source_preflight_failed"}, {sourceExportError, "source_export_failed"}} {
			want := tc.reason
			if want == "source_preflight_failed" {
				want = classifier.fallback
			}
			err := classifier.classify(&engine.ScriptError{Status: 2, Tail: []string{tc.line}})
			if got := errorReason(t, err); got != want {
				t.Errorf("%q: got %q, want %q", tc.line, got, want)
			}
			if strings.Contains(err.Error(), "/var/lib/") || strings.Contains(err.Error(), "/etc/ssl/") {
				t.Fatal("raw diagnostic leaked")
			}
		}
	}
}

func TestSourceAccessProbeSystemCAAndFailures(t *testing.T) {
	oldBundle := systemCABundlePath
	systemCABundlePath = filepath.Join(t.TempDir(), "system-ca.crt")
	if err := os.WriteFile(systemCABundlePath, []byte("not read by this test"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { systemCABundlePath = oldBundle })
	for _, operation := range []string{"preflight", "export"} {
		for _, tc := range []struct {
			name        string
			exec        accessProbeExecutor
			reason      string
			interrupted bool
		}{
			{"system-ca-denied", accessProbeExecutor{denied: copySystemCALabel}, "copy_workspace_access_denied", false},
			{"runuser-start", accessProbeExecutor{failure: errors.New("private start failure")}, "copy_workspace_probe_failed", false},
			{"wrapper-failure", accessProbeExecutor{failure: &engine.ScriptError{Status: 125, Tail: []string{"private lease failure"}}}, "copy_workspace_probe_failed", false},
			{"runuser-exit-one", accessProbeExecutor{failure: &engine.ScriptError{Status: 1}}, "copy_workspace_probe_failed", false},
			{"typed-lease", accessProbeExecutor{failure: output.NewError(output.CodePrecondition, "lease failed", output.ExitPrecondition, "").WithDetails(map[string]any{"reason": "copy_workspace_lease_failed"})}, "copy_workspace_lease_failed", false},
			{"invalid-marker", accessProbeExecutor{result: "unexpected"}, "copy_workspace_probe_failed", false},
			{"interrupted", accessProbeExecutor{failure: engine.ErrInterrupted}, "", true},
		} {
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				ws := newTestWorkspace(t)
				src, err := NewSource(SourceSpec{Host: "db.example.com", Database: "app", User: "u", TLSMode: SourceTLSVerifyCA})
				if err != nil {
					t.Fatal(err)
				}
				if operation == "preflight" {
					_, err = PreflightSource(context.Background(), &tc.exec, "copy", nil, src, ws)
				} else {
					_, err = ExportSource(context.Background(), &tc.exec, "copy", nil, src, ws)
				}
				if tc.interrupted {
					if !errors.Is(err, engine.ErrInterrupted) {
						t.Fatalf("interruption lost: %v", err)
					}
				} else if errorReason(t, err) != tc.reason {
					t.Fatalf("unexpected error: %v", err)
				}
				if tc.reason == "copy_workspace_probe_failed" {
					var cliErr *output.CLIError
					if !errors.As(err, &cliErr) || cliErr.Code != output.CodeExternalCommand {
						t.Fatalf("probe failure was not external: %v", err)
					}
				}
				if tc.name == "system-ca-denied" && errorDetails(t, err)["file"] != copySystemCALabel {
					t.Fatalf("system CA label missing: %v", err)
				}
				if strings.Contains(fmt.Sprint(err), "private") || strings.Contains(fmt.Sprint(err), systemCABundlePath) {
					t.Fatal("probe diagnostic leaked")
				}
				for _, call := range tc.exec.calls {
					if call == "source" {
						t.Fatal("source callback ran")
					}
				}
			})
		}
	}
}

func TestCopyAncestorRequiresServiceTraversal(t *testing.T) {
	parent := t.TempDir()
	originalOwner, originalUID, originalGID := copyWorkspaceOwnerOf, copyEffectiveUID, copyEffectiveGID
	copyWorkspaceOwnerOf = func(fs.FileInfo) (int, int, bool) { return 0, 0, true }
	copyEffectiveUID = func() int { return 0 }
	copyEffectiveGID = func() int { return 0 }
	t.Cleanup(func() {
		copyWorkspaceOwnerOf, copyEffectiveUID, copyEffectiveGID = originalOwner, originalUID, originalGID
	})
	err := verifyCopyAncestors(filepath.Join(parent, "copies"), WorkspaceOwner{UID: 12345, GID: 12345})
	if errorReason(t, err) != "copy_workspace_access_denied" {
		t.Fatalf("root-only ancestor accepted: %v", err)
	}
}

func TestSourceAccessVerifyCAUsesSystemBundle(t *testing.T) {
	ws := newTestWorkspace(t)
	bundle := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(bundle, testCertificatePEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	old := systemCABundlePath
	systemCABundlePath = bundle
	defer func() { systemCABundlePath = old }()
	src, err := NewSourceWithRegistry(secrets.NewRegistry(), SourceSpec{
		Host: "db.example.com", Database: "app", User: "u", Password: "secret-password", TLSMode: SourceTLSVerifyCA,
	})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	access, err := prepareSourceAccess(ws, src)
	if err != nil {
		t.Fatalf("prepareSourceAccess: %v", err)
	}
	if got := access.envValues()["PGSSLROOTCERT"]; got != bundle {
		t.Fatalf("PGSSLROOTCERT = %q, want %q", got, bundle)
	}
	if err := access.remove(); err != nil {
		t.Fatal(err)
	}

	systemCABundlePath = filepath.Join(t.TempDir(), "missing.pem")
	_, err = prepareSourceAccess(ws, src)
	if err == nil || errorReason(t, err) != "source_tls_ca_unavailable" {
		t.Fatalf("missing system bundle: %v", err)
	}
}

func TestSourceAccessRequireHasNoRootCert(t *testing.T) {
	ws := newTestWorkspace(t)
	src, err := NewSourceWithRegistry(secrets.NewRegistry(), SourceSpec{
		Host: "db.example.com", Database: "app", User: "u", Password: "secret-password", TLSMode: SourceTLSRequire,
	})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	access, err := prepareSourceAccess(ws, src)
	if err != nil {
		t.Fatalf("prepareSourceAccess: %v", err)
	}
	if got, ok := access.envValues()["PGSSLROOTCERT"]; ok {
		t.Fatalf("PGSSLROOTCERT = %q with sslmode=require", got)
	}
	if err := access.remove(); err != nil {
		t.Fatal(err)
	}
}

func testCertificatePEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "vaire test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

const preflightJSON = `180006,18.6,app,123456,t,t
pg_trgm,1.6,public
plpgsql,1.0,pg_catalog
`

func psqlHandler(json string) func(*recordedCall) error {
	return func(c *recordedCall) error {
		if !strings.Contains(strings.Join(c.cmd.Args, " "), "psql") {
			return nil
		}
		_, _ = io.WriteString(c.stdout, json)
		return nil
	}
}

func TestPreflightSource(t *testing.T) {
	ws := newTestWorkspace(t)
	src := testSource(t, "secret-password")
	fake := &fakeExecutor{handle: psqlHandler(preflightJSON)}
	pre, err := PreflightSource(context.Background(), fake, "db.copy.preflight", nil, src, ws)
	if err != nil {
		t.Fatalf("PreflightSource: %v", err)
	}
	if pre.ServerVersion != 180006 || pre.Database != "app" || pre.SizeBytes != 123456 {
		t.Fatalf("preflight: %+v", pre)
	}
	if len(pre.Extensions) != 2 || pre.Extensions[0].Name != "pg_trgm" || pre.Extensions[1].Name != "plpgsql" {
		t.Fatalf("extensions: %+v", pre.Extensions)
	}
	psql := fake.callContaining(t, "psql")
	joined := strings.Join(psql.cmd.Args, " ")
	if strings.Contains(joined, "secret-password") || strings.Contains(joined, "PGPASSWORD") {
		t.Fatalf("psql argv leaked a password: %s", joined)
	}
	if !strings.Contains(joined, "PGPASSFILE=") || !strings.Contains(joined, "PGGSSENCMODE=disable") {
		t.Fatalf("psql argv lacks the modeled environment: %s", joined)
	}
	if _, err := os.Stat(ws.FilePath(copyPassfileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("passfile survived preflight: %v", err)
	}
	if !strings.Contains(string(psql.stdin), "default_transaction_read_only") {
		t.Fatalf("preflight SQL is not read-only: %s", psql.stdin)
	}
}

func TestPreflightSourceVersionRefusal(t *testing.T) {
	ws := newTestWorkspace(t)
	src := testSource(t, "secret-password")
	raw := `190000,19.0,app,1,t,t
`
	fake := &fakeExecutor{handle: psqlHandler(raw)}
	_, err := PreflightSource(context.Background(), fake, "db.copy.preflight", nil, src, ws)
	if err == nil || errorReason(t, err) != "source_version_unsupported" {
		t.Fatalf("PG19 refusal: %v", err)
	}
}

func TestPreflightSourceFailures(t *testing.T) {
	ws := newTestWorkspace(t)
	src := testSource(t, "secret-password")
	tail := []string{`psql: error: connection to server at "db.example.com" failed: password authentication failed for user "source_user"`}
	fake := &fakeExecutor{handle: func(c *recordedCall) error {
		return &engine.ScriptError{Step: c.step, Status: 2, Tail: tail}
	}}
	_, err := PreflightSource(context.Background(), fake, "db.copy.preflight", nil, src, ws)
	if err == nil || errorReason(t, err) != "source_auth_failed" {
		t.Fatalf("auth failure: %v", err)
	}
	if strings.Contains(err.Error(), "secret-password") {
		t.Fatalf("auth error leaked password: %v", err)
	}
	if got := errorDetails(t, err)["exit_status"]; got != 2 {
		t.Fatalf("exit status detail %v", got)
	}

	fake = &fakeExecutor{handle: psqlHandler("not json")}
	_, err = PreflightSource(context.Background(), fake, "db.copy.preflight", nil, src, ws)
	if err == nil || errorReason(t, err) != "source_preflight_failed" {
		t.Fatalf("malformed preflight: %v", err)
	}

	fake = &fakeExecutor{handle: func(c *recordedCall) error {
		return &engine.ScriptError{Step: c.step, Status: 1, Tail: []string{
			"ERROR:  permission denied for function pg_database_size",
			"LINE 1: COPY ( SELECT current_setting('server_version_num'),",
		}}
	}}
	_, err = PreflightSource(context.Background(), fake, "db.copy.preflight", nil, src, ws)
	if err == nil || errorReason(t, err) != "source_preflight_failed" {
		t.Fatalf("permission preflight: %v", err)
	}
	if _, ok := errorDetails(t, err)["source_error"]; ok {
		t.Fatalf("preflight returned arbitrary source text: %v", errorDetails(t, err))
	}
	if strings.Contains(err.Error(), "permission denied") || strings.Contains(err.Error(), "COPY") {
		t.Fatalf("preflight error leaked server text: %v", err)
	}
}

func TestPreflightWarnsAboutWeakerTLS(t *testing.T) {
	ws := newTestWorkspace(t)
	src, err := NewSourceWithRegistry(secrets.NewRegistry(), SourceSpec{
		Host: "db.example.com", Database: "app", User: "u", Password: "secret-password", TLSMode: SourceTLSRequire,
	})
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeExecutor{handle: psqlHandler(preflightJSON)}
	pre, err := PreflightSource(context.Background(), fake, "db.copy.preflight", nil, src, ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(pre.Warnings) != 1 || pre.Warnings[0].Code != "source_tls_unverified" {
		t.Fatalf("warnings: %+v", pre.Warnings)
	}
}

func TestExportSource(t *testing.T) {
	ws := newTestWorkspace(t)
	src := testSource(t, "secret-password")
	var passfile string
	fake := &fakeExecutor{handle: func(c *recordedCall) error {
		joined := c.cmd.Name + " " + strings.Join(c.cmd.Args, " ")
		switch {
		case strings.Contains(joined, "pg_dump"):
			passfile = envArg(c.cmd, "PGPASSFILE")
			if passfile == "" {
				t.Fatal("pg_dump has no PGPASSFILE")
			}
			_, _ = io.WriteString(c.stdout, "fake custom archive")
			return nil
		case strings.Contains(joined, "pg_restore") && strings.Contains(joined, "--list"):
			writeFakeTOC(c, "app")
			return nil
		default:
			t.Fatalf("unexpected command: %s", joined)
			return nil
		}
	}}
	archive, err := ExportSource(context.Background(), fake, "db.copy.export", nil, src, ws)
	if err != nil {
		t.Fatalf("ExportSource: %v", err)
	}
	if archive.Size != int64(len("fake custom archive")) || archive.TOC.Database != "app" {
		t.Fatalf("archive: %+v", archive)
	}
	dump := fake.callContaining(t, "pg_dump")
	dumpArgs := strings.Join(dump.cmd.Args, " ")
	for _, want := range []string{"-Fc", "--quote-all-identifiers", "--no-password"} {
		if !strings.Contains(dumpArgs, want) {
			t.Fatalf("pg_dump args missing %q: %s", want, dumpArgs)
		}
	}
	for _, forbidden := range []string{"--globals-only", "--role", "secret-password", "PGPASSWORD"} {
		if strings.Contains(dumpArgs, forbidden) {
			t.Fatalf("pg_dump args contain %q: %s", forbidden, dumpArgs)
		}
	}
	if passfile == "" {
		t.Fatal("pg_dump passfile was not observed")
	}
	if _, err := os.Stat(passfile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("passfile survived export: %v", err)
	}
	info, err := os.Stat(ws.FilePath(copyArchiveName))
	if err != nil {
		t.Fatalf("archive stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("archive mode %o", info.Mode().Perm())
	}
}

func TestExportSourceFailureRemovesArchive(t *testing.T) {
	ws := newTestWorkspace(t)
	src := testSource(t, "secret-password")
	fake := &fakeExecutor{handle: func(c *recordedCall) error {
		if strings.Contains(strings.Join(c.cmd.Args, " "), "pg_dump") {
			_, _ = io.WriteString(c.stdout, "partial")
			return &engine.ScriptError{Step: c.step, Status: 1, Tail: []string{"pg_dump: error: permission denied for table secret"}}
		}
		t.Fatalf("unexpected call after failed dump: %v", c.cmd.Args)
		return nil
	}}
	_, err := ExportSource(context.Background(), fake, "db.copy.export", nil, src, ws)
	if err == nil || errorReason(t, err) != "source_export_failed" {
		t.Fatalf("export failure: %v", err)
	}
	if got := errorDetails(t, err)["exit_status"]; got != 1 {
		t.Fatalf("exit status detail %v", got)
	}
	if _, err := os.Stat(ws.FilePath(copyArchiveName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial archive remains: %v", err)
	}
}

func TestExportSourceAuthFailureClassification(t *testing.T) {
	ws := newTestWorkspace(t)
	src := testSource(t, "secret-password")
	fake := &fakeExecutor{handle: func(c *recordedCall) error {
		return &engine.ScriptError{Step: c.step, Status: 2, Tail: []string{"pg_dump: error: connection failed: password authentication failed"}}
	}}
	_, err := ExportSource(context.Background(), fake, "db.copy.export", nil, src, ws)
	if err == nil || errorReason(t, err) != "source_auth_failed" {
		t.Fatalf("auth export failure: %v", err)
	}
	if strings.Contains(err.Error(), "secret-password") {
		t.Fatalf("error leaked password: %v", err)
	}
}

func TestExportSourceRejectsUnreadableArchive(t *testing.T) {
	ws := newTestWorkspace(t)
	src := testSource(t, "secret-password")
	fake := &fakeExecutor{handle: func(c *recordedCall) error {
		joined := c.cmd.Name + " " + strings.Join(c.cmd.Args, " ")
		if strings.Contains(joined, "pg_dump") {
			_, _ = io.WriteString(c.stdout, "not a dump")
			return nil
		}
		_, _ = io.WriteString(c.stdout, ";\n;     dbname: app\n;     Format: DIRECTORY\n;\n1; 1262 0 DATABASE - app owner\n")
		return nil
	}}
	_, err := ExportSource(context.Background(), fake, "db.copy.export", nil, src, ws)
	if err == nil || errorReason(t, err) != "archive_invalid" {
		t.Fatalf("unreadable archive: %v", err)
	}
	if _, err := os.Stat(ws.FilePath(copyArchiveName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid archive remains: %v", err)
	}
}

func TestExportSourceDatabaseMismatch(t *testing.T) {
	ws := newTestWorkspace(t)
	src := testSource(t, "secret-password")
	fake := &fakeExecutor{handle: func(c *recordedCall) error {
		joined := c.cmd.Name + " " + strings.Join(c.cmd.Args, " ")
		if strings.Contains(joined, "pg_dump") {
			_, _ = io.WriteString(c.stdout, "fake")
			return nil
		}
		writeFakeTOC(c, "other")
		return nil
	}}
	_, err := ExportSource(context.Background(), fake, "db.copy.export", nil, src, ws)
	if err == nil || errorReason(t, err) != "archive_database_mismatch" {
		t.Fatalf("database mismatch: %v", err)
	}
}

func TestParseArchiveTOCList(t *testing.T) {
	toc, err := parseArchiveTOCList(";\n;     dbname: app\n;     Format: CUSTOM\n;\n1; 1262 0 DATABASE - app owner\n2; 1259 16390 TABLE public users owner\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if toc.Database != "app" || len(toc.Entries) != 2 || toc.Entries[1].ID != 2 || toc.Entries[1].TableOID != 1259 || toc.Entries[1].OID != 16390 {
		t.Fatalf("toc: %+v", toc)
	}
	for name, raw := range map[string]string{
		"missing format":   ";\n;     dbname: app\n;\n1; 1262 0 DATABASE - app owner\n",
		"missing database": ";\n;     Format: CUSTOM\n;\n1; 1262 0 DATABASE - app owner\n",
		"duplicate id":     ";\n;     dbname: app\n;     Format: CUSTOM\n;\n1; 1262 0 DATABASE - app owner\n1; 1259 1 TABLE t o\n",
		"bad line":         ";\n;     dbname: app\n;     Format: CUSTOM\n;\nnot-a-toc-line\n",
		"bad catalog id":   ";\n;     dbname: app\n;     Format: CUSTOM\n;\n1; x 0 DATABASE - app owner\n",
	} {
		if _, err := parseArchiveTOCList(raw); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestTOCSelectionWritesOnlyIDs(t *testing.T) {
	sel := &TOCSelection{Lines: []string{"7;", "8;"}}
	var b strings.Builder
	if err := sel.WriteList(&b); err != nil {
		t.Fatal(err)
	}
	if b.String() != "7;\n8;\n" {
		t.Fatalf("list %q", b.String())
	}
}

func selectionFixture(t *testing.T, tocText string, sql map[int]string) (*CopyWorkspace, *fakeExecutor, SourceArchive) {
	t.Helper()
	ws := newTestWorkspace(t)
	if err := ws.WriteFile(copyArchiveName, []byte("fake archive")); err != nil {
		t.Fatal(err)
	}
	toc, err := parseArchiveTOCList(tocText)
	if err != nil {
		t.Fatalf("parse fixture TOC: %v", err)
	}
	fake := &fakeExecutor{handle: func(c *recordedCall) error {
		joined := c.cmd.Name + " " + strings.Join(c.cmd.Args, " ")
		if !strings.Contains(joined, "pg_restore") || !strings.Contains(joined, "--use-list=") {
			t.Fatalf("unexpected inspection command: %s", joined)
		}
		var listPath string
		for _, arg := range c.cmd.Args {
			if strings.HasPrefix(arg, "--use-list=") {
				listPath = strings.TrimPrefix(arg, "--use-list=")
			}
		}
		data, err := os.ReadFile(listPath) //nolint:gosec // the test just created the private list.
		if err != nil {
			t.Fatalf("read inspection list: %v", err)
		}
		idText := strings.TrimSuffix(strings.TrimSpace(string(data)), ";")
		id, err := strconv.Atoi(idText)
		if err != nil {
			t.Fatalf("inspection id %q: %v", idText, err)
		}
		stmt, ok := sql[id]
		if !ok {
			t.Fatalf("no SQL fixture for inspection id %d", id)
		}
		_, _ = io.WriteString(c.stdout, stmt+"\n")
		return nil
	}}
	return ws, fake, SourceArchive{TOC: toc}
}

func TestSelectArchiveTOC(t *testing.T) {
	tocText := ";\n;     dbname: app\n;     Format: CUSTOM\n;\n" +
		"1; 1262 0 DATABASE - app owner\n" +
		"2; 0 0 DATABASE PROPERTIES - app owner\n" +
		"3; 3079 0 EXTENSION - pg_trgm \n" +
		"4; 0 0 COMMENT - \"pg_trgm\" \n" +
		"5; 0 0 COMMENT - DATABASE \"app\" owner\n" +
		"6; 3079 0 EXTENSION - plpgsql \n" +
		"7; 1259 16390 TABLE public users owner\n" +
		"8; 0 0 TABLE DATA public users owner\n" +
		"9; 0 0 TABLE DATA public trgm_config owner\n" +
		"10; 0 0 COMMENT public users owner\n" +
		"11; 0 0 COMMENT - pg_trgm owner\n" +
		"12; 0 0 SECURITY LABEL - DATABASE \"app\" owner\n" +
		"13; 0 0 COMMENT - some_fdw owner\n"
	sql := map[int]string{
		1:  `CREATE DATABASE "app" WITH TEMPLATE = template0;`,
		2:  `ALTER DATABASE "app" CONNECTION LIMIT = -1;`,
		3:  `CREATE EXTENSION IF NOT EXISTS "pg_trgm" WITH SCHEMA "public";`,
		4:  `COMMENT ON EXTENSION "pg_trgm" IS 'trgm';`,
		5:  `COMMENT ON DATABASE "app" IS 'db';`,
		6:  `CREATE EXTENSION IF NOT EXISTS "plpgsql" WITH SCHEMA "pg_catalog";`,
		11: `COMMENT ON SCHEMA "pg_trgm" IS 'schema';`,
		12: `SECURITY LABEL FOR selinux ON DATABASE "app" IS 'label';`,
		13: `COMMENT ON FOREIGN DATA WRAPPER "some_fdw" IS 'fdw';`,
	}
	ws, fake, archive := selectionFixture(t, tocText, sql)
	sel, err := SelectArchiveTOC(context.Background(), fake, "db.copy.select", nil, ws, archive, SelectionOptions{
		SourceDatabase: "app",
		Extensions: []SourceExtension{
			{Name: "plpgsql", Version: "1.0", Schema: "pg_catalog"},
			{Name: "pg_trgm", Version: "1.6", Schema: "public"},
		},
	})
	if err != nil {
		t.Fatalf("SelectArchiveTOC: %v", err)
	}
	if got, want := strings.Join(sel.Lines, " "), "7; 8; 9; 10; 11; 13;"; got != want {
		t.Fatalf("selected lines %q, want %q", got, want)
	}
	var omittedIDs []int
	for _, o := range sel.Omitted {
		omittedIDs = append(omittedIDs, o.ID)
	}
	if fmt.Sprint(omittedIDs) != "[1 2 3 4 5 6 12]" {
		t.Fatalf("omitted ids %v", omittedIDs)
	}
	for _, call := range fake.calls {
		if !strings.Contains(strings.Join(call.cmd.Args, " "), "--create") {
			t.Fatalf("inspection did not use --create: %v", call.cmd.Args)
		}
	}
}

// Modern pg_dump omits plpgsql's definition even though the source catalog
// always reports the installed extension. Ordinary objects must still be
// selected, while every non-built-in installed extension keeps the strict
// definition requirement.
func TestSelectArchiveTOCAllowsInstalledPlpgsqlWithoutDefinition(t *testing.T) {
	tocText := ";\n;     dbname: app\n;     Format: CUSTOM\n;\n" +
		"1; 1262 0 DATABASE - app owner\n" +
		"2; 1259 16390 TABLE public users owner\n" +
		"3; 0 0 TABLE DATA public users owner\n" +
		"4; 1259 16391 SEQUENCE public users_id_seq owner\n" +
		"5; 0 0 SEQUENCE SET public users_id_seq owner\n"
	sql := map[int]string{
		1: `CREATE DATABASE "app" WITH TEMPLATE = template0;`,
	}
	ws, fake, archive := selectionFixture(t, tocText, sql)
	sel, err := SelectArchiveTOC(context.Background(), fake, "db.copy.select", nil, ws, archive, SelectionOptions{
		SourceDatabase: "app",
		Extensions: []SourceExtension{
			{Name: "plpgsql", Version: "1.0", Schema: "pg_catalog"},
		},
	})
	if err != nil {
		t.Fatalf("SelectArchiveTOC: %v", err)
	}
	if got, want := strings.Join(sel.Lines, " "), "2; 3; 4; 5;"; got != want {
		t.Fatalf("selected lines %q, want %q", got, want)
	}
	if len(sel.Omitted) != 1 || sel.Omitted[0].ID != 1 || sel.Omitted[0].Kind != omittedDatabase {
		t.Fatalf("omitted = %+v", sel.Omitted)
	}
	for _, call := range fake.calls {
		if strings.Contains(strings.Join(call.cmd.Args, " "), "EXTENSION") {
			t.Fatalf("an absent plpgsql definition was inspected: %v", call.cmd.Args)
		}
	}
}

func TestSelectArchiveTOCFailClosed(t *testing.T) {
	base := ";\n;     dbname: app\n;     Format: CUSTOM\n;\n1; 1262 0 DATABASE - app owner\n"
	sql := map[int]string{1: `CREATE DATABASE "app" WITH TEMPLATE = template0;`}
	t.Run("unexpected extension", func(t *testing.T) {
		tocText := base + "2; 3079 0 EXTENSION - evil \n"
		ws, fake, archive := selectionFixture(t, tocText, sql)
		_, err := SelectArchiveTOC(context.Background(), fake, "select", nil, ws, archive, SelectionOptions{SourceDatabase: "app"})
		if err == nil || errorReason(t, err) != "archive_unexpected_extension" {
			t.Fatalf("unexpected extension: %v", err)
		}
	})
	t.Run("missing non-built-in extension definition", func(t *testing.T) {
		ws, fake, archive := selectionFixture(t, base, sql)
		_, err := SelectArchiveTOC(context.Background(), fake, "select", nil, ws, archive, SelectionOptions{
			SourceDatabase: "app",
			Extensions: []SourceExtension{
				{Name: "plpgsql", Version: "1.0", Schema: "pg_catalog"},
				{Name: "pg_trgm", Version: "1.6", Schema: "public"},
			},
		})
		if err == nil || errorReason(t, err) != "archive_invalid" {
			t.Fatalf("missing extension: %v", err)
		}
	})
	t.Run("database comment for another database", func(t *testing.T) {
		tocText := base + "2; 0 0 COMMENT - DATABASE \"other\" owner\n"
		sql2 := map[int]string{1: sql[1], 2: `COMMENT ON DATABASE "other" IS 'x';`}
		ws, fake, archive := selectionFixture(t, tocText, sql2)
		_, err := SelectArchiveTOC(context.Background(), fake, "select", nil, ws, archive, SelectionOptions{SourceDatabase: "app"})
		if err == nil || errorReason(t, err) != "archive_ambiguous" {
			t.Fatalf("database comment: %v", err)
		}
	})
	t.Run("malformed comment", func(t *testing.T) {
		tocText := base + "2; 0 0 COMMENT - something \n"
		sql2 := map[int]string{1: sql[1], 2: "DROP TABLE x;"}
		ws, fake, archive := selectionFixture(t, tocText, sql2)
		_, err := SelectArchiveTOC(context.Background(), fake, "select", nil, ws, archive, SelectionOptions{SourceDatabase: "app"})
		if err == nil || errorReason(t, err) != "archive_invalid" {
			t.Fatalf("malformed comment: %v", err)
		}
	})
	t.Run("control character", func(t *testing.T) {
		tocText := base + "2; 3079 0 EXTENSION - pg\t_trgm \n"
		ws, fake, archive := selectionFixture(t, tocText, sql)
		_, err := SelectArchiveTOC(context.Background(), fake, "select", nil, ws, archive, SelectionOptions{
			SourceDatabase: "app",
			Extensions:     []SourceExtension{{Name: "pg_trgm", Version: "1.6", Schema: "public"}},
		})
		if err == nil || errorReason(t, err) != "archive_invalid" {
			t.Fatalf("control character: %v", err)
		}
	})
	t.Run("missing database definition", func(t *testing.T) {
		tocText := ";\n;     dbname: app\n;     Format: CUSTOM\n;\n2; 3079 0 EXTENSION - pg_trgm \n"
		sql2 := map[int]string{2: `CREATE EXTENSION IF NOT EXISTS "pg_trgm" WITH SCHEMA "public";`}
		ws, fake, archive := selectionFixture(t, tocText, sql2)
		_, err := SelectArchiveTOC(context.Background(), fake, "select", nil, ws, archive, SelectionOptions{
			SourceDatabase: "app",
			Extensions:     []SourceExtension{{Name: "pg_trgm", Version: "1.6", Schema: "public"}},
		})
		if err == nil || errorReason(t, err) != "archive_invalid" {
			t.Fatalf("missing database: %v", err)
		}
	})
}

func TestMatchSourceExtensions(t *testing.T) {
	source := []SourceExtension{
		{Name: "pg_trgm", Version: "1.6", Schema: "public"},
		{Name: "plpgsql", Version: "1.0", Schema: "pg_catalog"},
	}
	destination := append([]SourceExtension(nil), source...)
	destination = append(destination, SourceExtension{Name: "pg_stat_statements", Version: "1.11", Schema: "public"})
	matches, err := MatchSourceExtensions(source, destination)
	if err != nil || len(matches) != 2 {
		t.Fatalf("match: %v %+v", err, matches)
	}
	for name, tc := range map[string]struct {
		source []SourceExtension
		dest   []SourceExtension
		reason string
	}{
		"missing": {
			source: source,
			dest:   destination[1:],
			reason: "extension_missing",
		},
		"version": {
			source: source,
			dest:   []SourceExtension{source[0], {Name: "plpgsql", Version: "1.1", Schema: "pg_catalog"}},
			reason: "extension_version_incompatible",
		},
		"schema": {
			source: source,
			dest:   []SourceExtension{{Name: "pg_trgm", Version: "1.6", Schema: "ext"}, source[1]},
			reason: "extension_schema_incompatible",
		},
		"duplicate destination": {
			source: source,
			dest:   []SourceExtension{source[0], source[0], source[1]},
			reason: "extension_incompatible",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := MatchSourceExtensions(tc.source, tc.dest)
			if err == nil || errorReason(t, err) != tc.reason {
				t.Fatalf("reason %v, want %s", err, tc.reason)
			}
		})
	}
}

func TestRestoreStrictArgumentsAndStdin(t *testing.T) {
	ws := newTestWorkspace(t)
	if err := ws.WriteFile(copyArchiveName, []byte("archive-body")); err != nil {
		t.Fatal(err)
	}
	var listPath, listContent string
	fake := &fakeExecutor{handle: func(c *recordedCall) error {
		joined := c.cmd.Name + " " + strings.Join(c.cmd.Args, " ")
		if !strings.Contains(joined, "pg_restore") {
			t.Fatalf("unexpected command: %s", joined)
		}
		for _, arg := range c.cmd.Args {
			if strings.HasPrefix(arg, "--use-list=") {
				listPath = strings.TrimPrefix(arg, "--use-list=")
			}
		}
		data, err := os.ReadFile(listPath) //nolint:gosec // the test just created the private list.
		if err != nil {
			t.Fatalf("read use-list: %v", err)
		}
		listContent = string(data)
		return nil
	}}
	sel := &TOCSelection{Lines: []string{"7;", "8;"}}
	err := RestoreStrict(context.Background(), fake, "db.copy.import", nil, ws, StrictRestoreOptions{
		Local:     pg.Local{Host: "/var/run/postgresql", Port: 5499},
		Target:    "newdb",
		Owner:     "newdb_owner",
		Selection: sel,
	})
	if err != nil {
		t.Fatalf("RestoreStrict: %v", err)
	}
	call := fake.calls[0]
	args := strings.Join(call.cmd.Args, " ")
	for _, want := range []string{
		"--no-owner", "--no-privileges", "--no-tablespaces", "--exit-on-error", "--no-password",
		"--role=newdb_owner", "--use-list=", "-h /var/run/postgresql", "-p 5499", "-d newdb",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("restore args missing %q: %s", want, args)
		}
	}
	for _, forbidden := range []string{"--clean", "--create", "--superuser", "--disable-triggers", "--if-exists"} {
		if strings.Contains(args, forbidden) {
			t.Fatalf("restore args contain %q: %s", forbidden, args)
		}
	}
	if string(call.stdin) != "archive-body" {
		t.Fatalf("restore stdin %q", call.stdin)
	}
	if listContent != "7;\n8;\n" {
		t.Fatalf("use-list content %q", listContent)
	}
	if _, err := os.Stat(listPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("use-list survived restore: %v", err)
	}
}

func TestRestoreStrictFailsOnFirstError(t *testing.T) {
	ws := newTestWorkspace(t)
	if err := ws.WriteFile(copyArchiveName, []byte("archive")); err != nil {
		t.Fatal(err)
	}
	fake := &fakeExecutor{handle: func(c *recordedCall) error {
		return &engine.ScriptError{Step: c.step, Status: 1, Tail: []string{
			"pg_restore: error: could not execute query: ERROR:  relation \"users\" already exists",
			"DETAIL:  Key (id)=(1) already exists.",
			"Command was: CREATE TABLE public.users (id integer, secret text);",
			"pg_restore: warning: errors ignored on restore: 1",
		}}
	}}
	err := RestoreStrict(context.Background(), fake, "db.copy.import", nil, ws, StrictRestoreOptions{
		Target: "newdb", Owner: "newdb_owner", Selection: &TOCSelection{Lines: []string{"7;"}},
	})
	if err == nil || errorReason(t, err) != "restore_conflict" {
		t.Fatalf("restore failure: %v", err)
	}
	if got := errorDetails(t, err)["exit_status"]; got != 1 {
		t.Fatalf("exit status detail %v", got)
	}
	if _, ok := errorDetails(t, err)["output_tail"]; ok {
		t.Fatalf("restore returned arbitrary output: %v", errorDetails(t, err))
	}
	if strings.Contains(err.Error(), "CREATE TABLE") || strings.Contains(err.Error(), "secret text") || strings.Contains(err.Error(), "Key (id)") {
		t.Fatalf("restore error leaked source SQL or data: %v", err)
	}
}

func TestRestoreStrictRejectsUnsafeOptionsAndAllowsEmptySelection(t *testing.T) {
	ws := newTestWorkspace(t)
	if err := ws.WriteFile(copyArchiveName, []byte("archive")); err != nil {
		t.Fatal(err)
	}
	fake := &fakeExecutor{}
	for name, opts := range map[string]StrictRestoreOptions{
		"superuser owner":   {Target: "newdb", Owner: "postgres", Selection: &TOCSelection{}},
		"bad target":        {Target: "Bad-Name", Owner: "bad_name_owner", Selection: &TOCSelection{}},
		"as root":           {Local: pg.Local{AsRoot: true}, Target: "newdb", Owner: "newdb_owner", Selection: &TOCSelection{}},
		"missing selection": {Target: "newdb", Owner: "newdb_owner"},
		"unsafe use-list":   {Target: "newdb", Owner: "newdb_owner", Selection: &TOCSelection{Lines: []string{"7; -- evil"}}},
	} {
		err := RestoreStrict(context.Background(), fake, "db.copy.import", nil, ws, opts)
		if err == nil || errorReason(t, err) != "restore_failed" {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(fake.calls) != 0 {
		t.Fatalf("unsafe options reached the executor: %d calls", len(fake.calls))
	}

	err := RestoreStrict(context.Background(), fake, "db.copy.import", nil, ws, StrictRestoreOptions{
		Target: "newdb", Owner: "newdb_owner", Selection: &TOCSelection{},
	})
	if err != nil {
		t.Fatalf("empty selection: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("empty selection calls %d", len(fake.calls))
	}
}

func TestCopyWorkspacePrivateFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "copies")
	ws, err := NewCopyWorkspace(root, "job_1", CurrentWorkspaceOwner(), func(string, int, int) error { return nil })
	if err != nil {
		t.Fatalf("NewCopyWorkspace: %v", err)
	}
	defer func() { _ = ws.Remove() }()
	info, err := os.Stat(ws.Dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("workspace mode: %v %v", err, info)
	}
	home, err := os.Stat(ws.FilePath(copyHomeName))
	if err != nil || !home.IsDir() || home.Mode().Perm() != 0o700 {
		t.Fatalf("home mode: %v %v", err, home)
	}
	f, err := ws.Create(copyPassfileName)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	fileInfo, err := os.Stat(ws.FilePath(copyPassfileName))
	if err != nil || fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("file mode: %v %v", err, fileInfo)
	}
	if _, err := ws.Create(copyPassfileName); err == nil {
		t.Fatal("second exclusive create succeeded")
	}
	if err := ws.WriteFile("../outside", []byte("x")); err == nil {
		t.Fatal("path traversal was accepted")
	}
	if err := ws.WriteFile("nested/file", []byte("x")); err == nil {
		t.Fatal("nested path was accepted")
	}
	if _, err := os.Stat(filepath.Join(root, "outside")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("path traversal wrote outside: %v", err)
	}
	if err := ws.WriteFile(copyArchiveName, []byte("archive")); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteFile(copyUseListName, []byte("7;\n")); err != nil {
		t.Fatal(err)
	}
	if err := ws.Cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	for _, name := range []string{copyArchiveName, copyUseListName, copyPassfileName, copyCAName, copyHomeName} {
		if _, err := os.Stat(ws.FilePath(name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived cleanup: %v", name, err)
		}
	}
}

func TestCopyWorkspaceRefusesReuseAndSymlink(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "job-1")
	if err := os.Mkdir(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := NewCopyWorkspace(root, "job-1", CurrentWorkspaceOwner(), func(string, int, int) error { return nil })
	if err == nil || errorReason(t, err) != "copy_workspace_exists" {
		t.Fatalf("reuse: %v", err)
	}
	if _, err := NewCopyWorkspace(root, "../escape", CurrentWorkspaceOwner(), func(string, int, int) error { return nil }); err == nil {
		t.Fatal("unsafe job id was accepted")
	}
	if _, err := NewCopyWorkspace("relative", "job-2", CurrentWorkspaceOwner(), func(string, int, int) error { return nil }); err == nil {
		t.Fatal("relative root was accepted")
	}

	ws := newTestWorkspace(t)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, ws.FilePath("link")); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteFile("link", []byte("overwritten")); err == nil {
		t.Fatal("symlink target was accepted for exclusive creation")
	}
	got, err := os.ReadFile(outside) //nolint:gosec // the test created this file.
	if err != nil || string(got) != "original" {
		t.Fatalf("outside file changed: %q %v", got, err)
	}
}

func TestCheckCopySpaceUsesBothFilesystems(t *testing.T) {
	ws := newTestWorkspace(t)
	old := externalCheckSpace
	defer func() { externalCheckSpace = old }()
	sentinel := errors.New("space short")
	var got []SpaceNeed
	externalCheckSpace = func(needs []SpaceNeed) ([]SpaceNeed, error) {
		got = append([]SpaceNeed(nil), needs...)
		return needs, sentinel
	}
	_, err := CheckCopySpace(ws, "/var/lib/vaire/data/app", 1000)
	if !errors.Is(err, sentinel) {
		t.Fatalf("space error: %v", err)
	}
	if len(got) != 2 || got[0].Path != ws.Dir || got[1].Path != "/var/lib/vaire/data/app" {
		t.Fatalf("space needs: %+v", got)
	}
	want := CopyNeed(1000)
	if got[0].Need != want || got[1].Need != want {
		t.Fatalf("space need %d, want %d", got[0].Need, want)
	}
}

func TestCheckPG18ToolsAt(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"pg_dump", "pg_restore", "psql"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // G306: fake tool binaries must be executable.
			t.Fatal(err)
		}
	}
	if err := CheckPG18ToolsAt(dir); err != nil {
		t.Fatalf("tools: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "pg_restore")); err != nil {
		t.Fatal(err)
	}
	err := CheckPG18ToolsAt(dir)
	if err == nil || errorReason(t, err) != "missing_pg18_tool" {
		t.Fatalf("missing tool: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pg_restore"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckPG18ToolsAt(dir); err == nil {
		t.Fatal("non-executable tool was accepted")
	}
}

func TestSourceValidatedRejectsDirectConstruction(t *testing.T) {
	if _, err := (Source{Host: "db.example.com", Database: "app=evil", User: "u"}).validated(); err == nil {
		t.Fatal("unsafe directly constructed source was accepted")
	}
	if _, err := (Source{Host: "db.example.com", Database: "app", User: "u", Port: 0, TLSMode: ""}).validated(); err != nil {
		t.Fatalf("defaults: %v", err)
	}
}

func TestSelectArchiveTOCRejectsDuplicateExtensionComment(t *testing.T) {
	tocText := ";\n;     dbname: app\n;     Format: CUSTOM\n;\n" +
		"1; 1262 0 DATABASE - app owner\n" +
		"2; 3079 0 EXTENSION - pg_trgm \n" +
		"3; 0 0 COMMENT - pg_trgm \n" +
		"4; 0 0 COMMENT - pg_trgm \n"
	sql := map[int]string{
		1: `CREATE DATABASE "app" WITH TEMPLATE = template0;`,
		2: `CREATE EXTENSION IF NOT EXISTS "pg_trgm" WITH SCHEMA "public";`,
		3: `COMMENT ON EXTENSION "pg_trgm" IS 'one';`,
		4: `COMMENT ON EXTENSION "pg_trgm" IS 'two';`,
	}
	ws, fake, archive := selectionFixture(t, tocText, sql)
	_, err := SelectArchiveTOC(context.Background(), fake, "select", nil, ws, archive, SelectionOptions{
		SourceDatabase: "app",
		Extensions:     []SourceExtension{{Name: "pg_trgm", Version: "1.6", Schema: "public"}},
	})
	if err == nil || errorReason(t, err) != "archive_ambiguous" {
		t.Fatalf("duplicate extension comment: %v", err)
	}
}

func TestPreflightCSVQuotingAndCatalogWarning(t *testing.T) {
	ws := newTestWorkspace(t)
	src := testSource(t, "secret-password")
	raw := "180006,18.6,app,10,t,f\n\"weird,ext\",1.0,public\n"
	fake := &fakeExecutor{handle: psqlHandler(raw)}
	pre, err := PreflightSource(context.Background(), fake, "db.copy.preflight", nil, src, ws)
	if err != nil {
		t.Fatalf("PreflightSource: %v", err)
	}
	if len(pre.Extensions) != 1 || pre.Extensions[0].Name != "weird,ext" {
		t.Fatalf("extensions: %+v", pre.Extensions)
	}
	if len(pre.Warnings) != 1 || pre.Warnings[0].Code != "source_permissions_unverified" {
		t.Fatalf("warnings: %+v", pre.Warnings)
	}
}

func TestSelectArchiveTOCRejectsExtensionSchemaMismatch(t *testing.T) {
	tocText := ";\n;     dbname: app\n;     Format: CUSTOM\n;\n1; 1262 0 DATABASE - app owner\n2; 3079 0 EXTENSION - pg_trgm \n"
	sql := map[int]string{
		1: `CREATE DATABASE "app" WITH TEMPLATE = template0;`,
		2: `CREATE EXTENSION IF NOT EXISTS "pg_trgm" WITH SCHEMA "other";`,
	}
	ws, fake, archive := selectionFixture(t, tocText, sql)
	_, err := SelectArchiveTOC(context.Background(), fake, "select", nil, ws, archive, SelectionOptions{
		SourceDatabase: "app",
		Extensions:     []SourceExtension{{Name: "pg_trgm", Version: "1.6", Schema: "public"}},
	})
	if err == nil || errorReason(t, err) != "archive_invalid" {
		t.Fatalf("extension schema mismatch: %v", err)
	}
}

func TestCopyDiagnosticsNeverForwardRawSubprocessText(t *testing.T) {
	ws := newTestWorkspace(t)
	src := testSource(t, "secret-password")
	sensitive := []string{
		"ERROR:  INSERT INTO private_rows VALUES ('alice', 'top-secret-row')",
		"DETAIL:  Key (email)=(alice@example.com) already exists.",
		"STATEMENT:  INSERT INTO private_rows VALUES ('alice', 'top-secret-row')",
		"CONTEXT:  COPY private_rows, line 1: \"alice\ttop-secret-row\"",
	}
	assertClean := func(label string, err error, recorded *eventRecorder) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: expected an error", label)
		}
		if lines := recorded.logLines(); len(lines) != 0 {
			t.Fatalf("%s: forwarded raw log lines: %q", label, lines)
		}
		for _, secret := range []string{"alice", "top-secret-row", "INSERT INTO", "STATEMENT:", "CONTEXT:"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("%s: error leaked %q: %v", label, secret, err)
			}
		}
		details := errorDetails(t, err)
		if _, ok := details["output_tail"]; ok {
			t.Fatalf("%s: error returned output_tail: %v", label, details)
		}
		if _, ok := details["source_error"]; ok {
			t.Fatalf("%s: error returned source_error: %v", label, details)
		}
	}

	preEvents := &eventRecorder{}
	preFake := &fakeExecutor{logs: sensitive, handle: func(c *recordedCall) error {
		return &engine.ScriptError{Step: c.step, Status: 1, Tail: sensitive}
	}}
	_, err := PreflightSource(context.Background(), preFake, "preflight", preEvents.report, src, ws)
	assertClean("preflight", err, preEvents)

	exportEvents := &eventRecorder{}
	exportFake := &fakeExecutor{logs: sensitive, handle: func(c *recordedCall) error {
		return &engine.ScriptError{Step: c.step, Status: 1, Tail: sensitive}
	}}
	_, err = ExportSource(context.Background(), exportFake, "export", exportEvents.report, src, ws)
	assertClean("export", err, exportEvents)

	archiveEvents := &eventRecorder{}
	archiveFake := &fakeExecutor{logs: sensitive, handle: func(c *recordedCall) error {
		return &engine.ScriptError{Step: c.step, Status: 1, Tail: sensitive}
	}}
	_, err = ReadArchiveTOC(context.Background(), archiveFake, "list", archiveEvents.report, strings.NewReader("archive"))
	assertClean("archive list", err, archiveEvents)

	restoreEvents := &eventRecorder{}
	restoreFake := &fakeExecutor{logs: sensitive, handle: func(c *recordedCall) error {
		return &engine.ScriptError{Step: c.step, Status: 1, Tail: sensitive}
	}}
	if err := ws.WriteFile(copyArchiveName, []byte("archive")); err != nil {
		t.Fatal(err)
	}
	err = RestoreStrict(context.Background(), restoreFake, "restore", restoreEvents.report, ws, StrictRestoreOptions{
		Target: "newdb", Owner: "newdb_owner", Selection: &TOCSelection{Lines: []string{"7;"}},
	})
	assertClean("restore", err, restoreEvents)

	toc, err := parseArchiveTOCList(";\n;     dbname: app\n;     Format: CUSTOM\n;\n1; 1262 0 DATABASE - app owner\n")
	if err != nil {
		t.Fatal(err)
	}
	inspectEvents := &eventRecorder{}
	inspectFake := &fakeExecutor{logs: sensitive, handle: func(c *recordedCall) error {
		return &engine.ScriptError{Step: c.step, Status: 1, Tail: sensitive}
	}}
	_, err = SelectArchiveTOC(context.Background(), inspectFake, "select", inspectEvents.report, ws,
		SourceArchive{TOC: toc}, SelectionOptions{SourceDatabase: "app"})
	assertClean("inspect", err, inspectEvents)
}

func TestCopyDiagnosticsBoundOversizedOutput(t *testing.T) {
	oldPreflight, oldTOC, oldInspect := copyPreflightLimitBytes, copyTOCLimitBytes, copyInspectLimitBytes
	copyPreflightLimitBytes, copyTOCLimitBytes, copyInspectLimitBytes = 32, 32, 32
	defer func() {
		copyPreflightLimitBytes, copyTOCLimitBytes, copyInspectLimitBytes = oldPreflight, oldTOC, oldInspect
	}()
	ws := newTestWorkspace(t)
	src := testSource(t, "secret-password")
	big := strings.Repeat("Z", 128)
	assertFixed := func(label string, err error, reason string) {
		t.Helper()
		if err == nil || errorReason(t, err) != reason {
			t.Fatalf("%s: err %v, want reason %s", label, err, reason)
		}
		if strings.Contains(err.Error(), "ZZZZ") {
			t.Fatalf("%s: error echoed oversized output: %v", label, err)
		}
	}

	preEvents := &eventRecorder{}
	preFake := &fakeExecutor{handle: func(c *recordedCall) error {
		_, _ = io.WriteString(c.stdout, big)
		return nil
	}}
	_, err := PreflightSource(context.Background(), preFake, "preflight", preEvents.report, src, ws)
	assertFixed("preflight", err, "source_preflight_failed")
	if lines := preEvents.logLines(); len(lines) != 0 {
		t.Fatalf("preflight forwarded logs: %q", lines)
	}
	preTailFake := &fakeExecutor{handle: func(c *recordedCall) error {
		return &engine.ScriptError{Step: c.step, Status: 1, Tail: []string{strings.Repeat("P", 100000)}}
	}}
	_, err = PreflightSource(context.Background(), preTailFake, "preflight", nil, src, ws)
	assertFixed("preflight tail", err, "source_preflight_failed")
	if strings.Contains(err.Error(), "PPPP") {
		t.Fatalf("preflight tail echoed: %v", err)
	}

	listEvents := &eventRecorder{}
	listFake := &fakeExecutor{handle: func(c *recordedCall) error {
		_, _ = io.WriteString(c.stdout, big)
		return nil
	}}
	_, err = ReadArchiveTOC(context.Background(), listFake, "list", listEvents.report, strings.NewReader("archive"))
	assertFixed("toc list", err, "archive_invalid")
	if lines := listEvents.logLines(); len(lines) != 0 {
		t.Fatalf("toc list forwarded logs: %q", lines)
	}
	listTailFake := &fakeExecutor{handle: func(c *recordedCall) error {
		return &engine.ScriptError{Step: c.step, Status: 1, Tail: []string{strings.Repeat("R", 100000)}}
	}}
	_, err = ReadArchiveTOC(context.Background(), listTailFake, "list", nil, strings.NewReader("archive"))
	assertFixed("toc list tail", err, "archive_unreadable")
	if strings.Contains(err.Error(), "RRRR") {
		t.Fatalf("toc list tail echoed: %v", err)
	}

	if err := ws.WriteFile(copyArchiveName, []byte("archive")); err != nil {
		t.Fatal(err)
	}
	toc, err := parseArchiveTOCList(";\n;     dbname: app\n;     Format: CUSTOM\n;\n1; 1262 0 DATABASE - app owner\n")
	if err != nil {
		t.Fatal(err)
	}
	inspectEvents := &eventRecorder{}
	inspectFake := &fakeExecutor{handle: func(c *recordedCall) error {
		_, _ = io.WriteString(c.stdout, big)
		return nil
	}}
	_, err = SelectArchiveTOC(context.Background(), inspectFake, "select", inspectEvents.report, ws,
		SourceArchive{TOC: toc}, SelectionOptions{SourceDatabase: "app"})
	assertFixed("inspect", err, "archive_invalid")
	if lines := inspectEvents.logLines(); len(lines) != 0 {
		t.Fatalf("inspect forwarded logs: %q", lines)
	}
	inspectTailFake := &fakeExecutor{handle: func(c *recordedCall) error {
		return &engine.ScriptError{Step: c.step, Status: 1, Tail: []string{strings.Repeat("S", 100000)}}
	}}
	_, err = SelectArchiveTOC(context.Background(), inspectTailFake, "select", nil, ws,
		SourceArchive{TOC: toc}, SelectionOptions{SourceDatabase: "app"})
	assertFixed("inspect tail", err, "archive_unreadable")
	if strings.Contains(err.Error(), "SSSS") {
		t.Fatalf("inspect tail echoed: %v", err)
	}

	exportFake := &fakeExecutor{handle: func(c *recordedCall) error {
		return &engine.ScriptError{Step: c.step, Status: 1, Tail: []string{strings.Repeat("Q", 100000)}}
	}}
	exportWS := newTestWorkspace(t)
	_, err = ExportSource(context.Background(), exportFake, "export", nil, src, exportWS)
	assertFixed("export", err, "source_export_failed")
}

func TestNewCopyWorkspaceRejectsUnsafeRoot(t *testing.T) {
	owner := CurrentWorkspaceOwner()
	noChown := func(string, int, int) error { return nil }

	t.Run("writable root", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Chmod(root, 0o777); err != nil { //nolint:gosec // G302: deliberately unsafe fixture.
			t.Fatal(err)
		}
		_, err := NewCopyWorkspace(root, "job-1", owner, noChown)
		if err == nil || errorReason(t, err) != "copy_workspace_unsafe" {
			t.Fatalf("writable root: %v", err)
		}
	})

	t.Run("symlink root", func(t *testing.T) {
		target := t.TempDir()
		root := filepath.Join(t.TempDir(), "copies")
		if err := os.Symlink(target, root); err != nil {
			t.Fatal(err)
		}
		_, err := NewCopyWorkspace(root, "job-1", owner, noChown)
		if err == nil || errorReason(t, err) != "copy_workspace_unsafe" {
			t.Fatalf("symlink root: %v", err)
		}
	})

	t.Run("symlink ancestor", func(t *testing.T) {
		base := t.TempDir()
		real := filepath.Join(base, "real")
		if err := os.Mkdir(real, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(base, "link")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		_, err := NewCopyWorkspace(filepath.Join(link, "copies"), "job-1", owner, noChown)
		if err == nil || errorReason(t, err) != "copy_workspace_unsafe" {
			t.Fatalf("symlink ancestor: %v", err)
		}
	})

	t.Run("other-writable ancestor", func(t *testing.T) {
		base := t.TempDir()
		if err := os.Chmod(base, 0o777); err != nil { //nolint:gosec // G302: deliberately unsafe fixture.
			t.Fatal(err)
		}
		_, err := NewCopyWorkspace(filepath.Join(base, "copies"), "job-1", owner, noChown)
		if err == nil || errorReason(t, err) != "copy_workspace_unsafe" {
			t.Fatalf("other-writable ancestor: %v", err)
		}
	})

	t.Run("foreign ancestor owner", func(t *testing.T) {
		base := t.TempDir()
		realOwner := copyWorkspaceOwnerOf
		copyWorkspaceOwnerOf = func(info fs.FileInfo) (int, int, bool) {
			if info.Name() == filepath.Base(base) {
				return os.Geteuid() + 1, os.Getegid(), true
			}
			return realOwner(info)
		}
		defer func() { copyWorkspaceOwnerOf = realOwner }()
		_, err := NewCopyWorkspace(filepath.Join(base, "copies"), "job-1", owner, noChown)
		if err == nil || errorReason(t, err) != "copy_workspace_unsafe" {
			t.Fatalf("foreign ancestor owner: %v", err)
		}
	})

	t.Run("foreign root owner", func(t *testing.T) {
		realOwner := copyWorkspaceOwnerOf
		copyWorkspaceOwnerOf = func(info fs.FileInfo) (int, int, bool) {
			if info.Name() == "." {
				return os.Geteuid() + 1, os.Getegid(), true
			}
			return realOwner(info)
		}
		defer func() { copyWorkspaceOwnerOf = realOwner }()
		root := filepath.Join(t.TempDir(), "copies")
		_, err := NewCopyWorkspace(root, "job-1", owner, noChown)
		if err == nil || errorReason(t, err) != "copy_workspace_unsafe" {
			t.Fatalf("foreign root owner: %v", err)
		}
	})
}

// E02: the production process runs as root, so an ownership or group-writable
// check may not be skipped just because the effective uid is 0. A foreign
// ancestor can replace the copy root entry whether or not the directory is
// sticky; only a root- or process-owned sticky directory is trusted.
func TestNewCopyWorkspaceRejectsUntrustedAncestorAsRoot(t *testing.T) {
	owner := CurrentWorkspaceOwner()
	noChown := func(string, int, int) error { return nil }
	realUID, realGID := copyEffectiveUID, copyEffectiveGID
	realGIDValue := copyEffectiveGID()
	copyEffectiveUID = func() int { return 0 }
	copyEffectiveGID = func() int { return 0 }
	t.Cleanup(func() {
		copyEffectiveUID, copyEffectiveGID = realUID, realGID
	})

	for name, mode := range map[string]os.FileMode{
		"foreign-owned non-sticky":     0o700,
		"foreign-owned group-writable": 0o770,
		"foreign-owned sticky":         0o777 | os.ModeSticky,
	} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			if err := os.Chmod(base, mode); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(base, "copies")
			_, err := NewCopyWorkspace(root, "job-1", owner, noChown)
			if err == nil || errorReason(t, err) != "copy_workspace_unsafe" {
				t.Fatalf("%s: %v", name, err)
			}
			if _, statErr := os.Lstat(root); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("%s: an untrusted ancestor still created %s: %v", name, root, statErr)
			}
		})
	}

	// A trusted owner is not enough for a non-sticky directory writable by
	// another group: those group members could replace the entry.
	groupWritable := t.TempDir()
	if err := os.Chmod(groupWritable, 0o770); err != nil { //nolint:gosec // G302: deliberately group-writable fixture.
		t.Fatal(err)
	}
	realOwner := copyWorkspaceOwnerOf
	copyWorkspaceOwnerOf = func(info fs.FileInfo) (int, int, bool) {
		if info.Name() == filepath.Base(groupWritable) {
			return 0, realGIDValue + 1, true
		}
		return realOwner(info)
	}
	root := filepath.Join(groupWritable, "copies")
	_, err := NewCopyWorkspace(root, "job-1", owner, noChown)
	copyWorkspaceOwnerOf = realOwner
	if err == nil || errorReason(t, err) != "copy_workspace_unsafe" {
		t.Fatalf("trusted owner with a foreign group-writable ancestor: %v", err)
	}
	if _, statErr := os.Lstat(root); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a foreign-group-writable ancestor still created %s: %v", root, statErr)
	}

	// A root-owned sticky directory such as /tmp is the one shared ancestor
	// a root process may traverse: other users can only create their own
	// entries there.
	base := t.TempDir()
	sticky := filepath.Join(base, "shared")
	if err := os.Mkdir(sticky, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sticky, 0o777|os.ModeSticky); err != nil { //nolint:gosec // G302: deliberately sticky and world-writable.
		t.Fatal(err)
	}
	stickyOwner := copyWorkspaceOwnerOf
	copyWorkspaceOwnerOf = func(info fs.FileInfo) (int, int, bool) {
		if info.IsDir() {
			return 0, 0, true
		}
		return stickyOwner(info)
	}
	t.Cleanup(func() { copyWorkspaceOwnerOf = stickyOwner })
	if err := verifyCopyAncestors(filepath.Join(sticky, "copies"), CurrentWorkspaceOwner()); err != nil {
		t.Fatalf("a root-owned sticky ancestor was refused: %v", err)
	}
}

func TestNewCopyWorkspaceCreationReplacementLeavesOutsideAlone(t *testing.T) {
	owner := CurrentWorkspaceOwner()
	noChown := func(string, int, int) error { return nil }

	t.Run("directory replaced after mkdir", func(t *testing.T) {
		root := t.TempDir()
		var outside, original string
		old := copyAfterJobMkdir
		copyAfterJobMkdir = func(dir string) {
			original = dir + ".original"
			if err := os.Rename(dir, original); err != nil {
				t.Fatal(err)
			}
			outside = t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "sentinel"), []byte("outside"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "replacement"), []byte("replacement"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		defer func() { copyAfterJobMkdir = old }()
		_, err := NewCopyWorkspace(root, "job-1", owner, noChown)
		if err == nil || errorReason(t, err) != "copy_workspace_unsafe" {
			t.Fatalf("replacement after mkdir: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(outside, "sentinel")) //nolint:gosec // G304: the test reads its own fixture.
		if err != nil || string(got) != "outside" {
			t.Fatalf("outside file changed: %q %v", got, err)
		}
		if _, err := os.Stat(original); err != nil {
			t.Fatalf("original created directory was removed: %v", err)
		}
		got, err = os.ReadFile(filepath.Join(root, "job-1", "replacement")) //nolint:gosec // G304: the test reads its own fixture.
		if err != nil || string(got) != "replacement" {
			t.Fatalf("replacement directory was removed or changed: %q %v", got, err)
		}
	})

	t.Run("symlink replaced after mkdir", func(t *testing.T) {
		root := t.TempDir()
		var outside, original string
		old := copyAfterJobMkdir
		copyAfterJobMkdir = func(dir string) {
			original = dir + ".original"
			if err := os.Rename(dir, original); err != nil {
				t.Fatal(err)
			}
			outside = t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "sentinel"), []byte("outside"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, dir); err != nil {
				t.Fatal(err)
			}
		}
		defer func() { copyAfterJobMkdir = old }()
		_, err := NewCopyWorkspace(root, "job-1", owner, noChown)
		if err == nil || errorReason(t, err) != "copy_workspace_unsafe" {
			t.Fatalf("symlink replacement after mkdir: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(outside, "sentinel")) //nolint:gosec // G304: the test reads its own fixture.
		if err != nil || string(got) != "outside" {
			t.Fatalf("outside file changed: %q %v", got, err)
		}
		if _, err := os.Stat(original); err != nil {
			t.Fatalf("original created directory was removed: %v", err)
		}
	})

	t.Run("replaced after root open", func(t *testing.T) {
		root := t.TempDir()
		var outside, original string
		old := copyAfterJobRootOpen
		copyAfterJobRootOpen = func(dir string) {
			original = dir + ".original"
			if err := os.Rename(dir, original); err != nil {
				t.Fatal(err)
			}
			outside = t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "sentinel"), []byte("outside"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "replacement"), []byte("replacement"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		defer func() { copyAfterJobRootOpen = old }()
		_, err := NewCopyWorkspace(root, "job-1", owner, noChown)
		if err == nil || errorReason(t, err) != "copy_workspace_unsafe" {
			t.Fatalf("replacement after root open: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(outside, "sentinel")) //nolint:gosec // G304: the test reads its own fixture.
		if err != nil || string(got) != "outside" {
			t.Fatalf("outside file changed: %q %v", got, err)
		}
		got, err = os.ReadFile(filepath.Join(root, "job-1", "replacement")) //nolint:gosec // G304: the test reads its own fixture.
		if err != nil || string(got) != "replacement" {
			t.Fatalf("replacement directory was removed or changed: %q %v", got, err)
		}
		if _, err := os.Stat(original); err != nil {
			t.Fatalf("original created directory was removed: %v", err)
		}
	})
}

func TestCopyWorkspaceRemoveRefusesReplacement(t *testing.T) {
	t.Run("directory replacement", func(t *testing.T) {
		ws := newTestWorkspace(t)
		saved := ws.Dir + ".saved"
		if err := os.Rename(ws.Dir, saved); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(ws.Dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws.Dir, "sentinel"), []byte("replacement"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := ws.Remove()
		if err == nil || errorReason(t, err) != "copy_workspace_unsafe" {
			t.Fatalf("directory replacement removal: %v", err)
		}
		if got, err := os.ReadFile(filepath.Join(ws.Dir, "sentinel")); err != nil || string(got) != "replacement" {
			t.Fatalf("replacement directory was removed: %q %v", got, err)
		}
		if _, err := os.Stat(saved); err != nil {
			t.Fatalf("original workspace was removed: %v", err)
		}
	})
	t.Run("symlink replacement", func(t *testing.T) {
		ws := newTestWorkspace(t)
		saved := ws.Dir + ".saved"
		if err := os.Rename(ws.Dir, saved); err != nil {
			t.Fatal(err)
		}
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "sentinel"), []byte("outside"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, ws.Dir); err != nil {
			t.Fatal(err)
		}
		err := ws.Remove()
		if err == nil || errorReason(t, err) != "copy_workspace_unsafe" {
			t.Fatalf("symlink replacement removal: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(outside, "sentinel")) //nolint:gosec // G304: the test reads its own fixture.
		if err != nil || string(got) != "outside" {
			t.Fatalf("outside file changed or was removed: %q %v", got, err)
		}
		if _, err := os.Lstat(ws.Dir); err != nil {
			t.Fatalf("replacement symlink was removed: %v", err)
		}
		if _, err := os.Stat(saved); err != nil {
			t.Fatalf("original workspace was removed: %v", err)
		}
	})
}

func TestSelectArchiveTOCPropagatesInspectCleanupFailure(t *testing.T) {
	ws := newTestWorkspace(t)
	if err := ws.WriteFile(copyArchiveName, []byte("archive")); err != nil {
		t.Fatal(err)
	}
	toc, err := parseArchiveTOCList(";\n;     dbname: app\n;     Format: CUSTOM\n;\n1; 1262 0 DATABASE - app owner\n")
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeExecutor{handle: func(c *recordedCall) error {
		_, _ = io.WriteString(c.stdout, `CREATE DATABASE "app" WITH TEMPLATE = template0;`+"\n")
		var listPath string
		for _, arg := range c.cmd.Args {
			if strings.HasPrefix(arg, "--use-list=") {
				listPath = strings.TrimPrefix(arg, "--use-list=")
			}
		}
		if listPath == "" {
			t.Fatal("inspection had no use-list path")
		}
		if err := os.Remove(listPath); err != nil {
			t.Fatalf("remove inspection list: %v", err)
		}
		if err := os.Mkdir(listPath, 0o700); err != nil {
			t.Fatalf("replace inspection list with directory: %v", err)
		}
		if err := os.WriteFile(filepath.Join(listPath, "block"), []byte("block"), 0o600); err != nil {
			t.Fatalf("block inspection list: %v", err)
		}
		return nil
	}}
	_, err = SelectArchiveTOC(context.Background(), fake, "select", nil, ws, SourceArchive{TOC: toc}, SelectionOptions{SourceDatabase: "app"})
	if err == nil || errorReason(t, err) != "workspace_cleanup_failed" {
		t.Fatalf("inspection cleanup failure: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("selection continued after cleanup failure: %d calls", len(fake.calls))
	}
}

func TestPrepareSourceAccessCleansPassfileWhenCAWriteFails(t *testing.T) {
	ws := newTestWorkspace(t)
	if err := ws.WriteFile(copyCAName, []byte("pre-existing CA file")); err != nil {
		t.Fatal(err)
	}
	src := testSource(t, "secret-password")
	ca := testCertificatePEM(t)
	src.caPEM = ca
	_, err := prepareSourceAccess(ws, src)
	if err == nil || errorReason(t, err) != "copy_workspace_failed" {
		t.Fatalf("CA write failure: %v", err)
	}
	if _, statErr := os.Stat(ws.FilePath(copyPassfileName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("passfile was not cleaned after CA failure: %v", statErr)
	}
}

func TestPrepareSourceAccessPropagatesPassfileCleanupFailure(t *testing.T) {
	ws := newTestWorkspace(t)
	if err := ws.WriteFile(copyCAName, []byte("pre-existing CA file")); err != nil {
		t.Fatal(err)
	}
	old := copyAfterSourcePassfile
	copyAfterSourcePassfile = func(ws *CopyWorkspace) {
		passfile := ws.FilePath(copyPassfileName)
		if err := os.Rename(passfile, passfile+".saved"); err != nil {
			t.Fatalf("rename passfile: %v", err)
		}
		if err := os.Mkdir(passfile, 0o700); err != nil {
			t.Fatalf("replace passfile with directory: %v", err)
		}
		if err := os.WriteFile(filepath.Join(passfile, "block"), []byte("block"), 0o600); err != nil {
			t.Fatalf("block passfile: %v", err)
		}
	}
	defer func() { copyAfterSourcePassfile = old }()
	src := testSource(t, "secret-password")
	src.caPEM = testCertificatePEM(t)
	_, err := prepareSourceAccess(ws, src)
	if err == nil {
		t.Fatal("expected a cleanup error")
	}
	if !strings.Contains(err.Error(), "temporary copy file could not be removed") {
		t.Fatalf("cleanup error was not propagated: %v", err)
	}
	if strings.Contains(err.Error(), "secret-password") {
		t.Fatalf("cleanup error leaked a password: %v", err)
	}
}

func TestRestoreDiagnosticsBoundOversizedTail(t *testing.T) {
	ws := newTestWorkspace(t)
	if err := ws.WriteFile(copyArchiveName, []byte("archive")); err != nil {
		t.Fatal(err)
	}
	fake := &fakeExecutor{handle: func(c *recordedCall) error {
		return &engine.ScriptError{Step: c.step, Status: 1, Tail: []string{strings.Repeat("W", 100000)}}
	}}
	err := RestoreStrict(context.Background(), fake, "restore", nil, ws, StrictRestoreOptions{
		Target: "newdb", Owner: "newdb_owner", Selection: &TOCSelection{},
	})
	if err == nil || errorReason(t, err) != "restore_failed" {
		t.Fatalf("restore oversized tail: %v", err)
	}
	if strings.Contains(err.Error(), "WWWW") {
		t.Fatalf("restore error echoed oversized tail: %v", err)
	}
}

func TestNewCopyWorkspaceOwnershipFailureLeavesNoWorkspace(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a root test process can chown to a foreign account")
	}
	root := t.TempDir()
	owner := CurrentWorkspaceOwner()
	owner.GID = 1 << 30
	_, err := NewCopyWorkspace(root, "job-1", owner, nil)
	if err == nil || errorReason(t, err) != "copy_workspace_failed" {
		t.Fatalf("foreign ownership: %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(root, "job-1")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed workspace was left behind: %v", statErr)
	}
}

func TestNewCopyWorkspaceAllowsStickySharedAncestor(t *testing.T) {
	base := t.TempDir()
	sticky := filepath.Join(base, "shared")
	if err := os.Mkdir(sticky, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sticky, 0o777|os.ModeSticky); err != nil { //nolint:gosec // G302: deliberately world-writable but sticky.
		t.Fatal(err)
	}
	ws, err := NewCopyWorkspace(filepath.Join(sticky, "copies"), "job-1", CurrentWorkspaceOwner(), nil)
	if err != nil {
		t.Fatalf("sticky ancestor: %v", err)
	}
	if err := ws.Remove(); err != nil {
		t.Fatalf("remove sticky workspace: %v", err)
	}
}
