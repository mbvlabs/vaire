package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/cli/ui"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/node"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/secrets"
	"github.com/mbvlabs/vaire/internal/steps"
	"github.com/mbvlabs/vaire/internal/tune"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// vaire db copy copies one external PostgreSQL database into a brand-new
// vaire database. The operator supplies the source and destination; the node
// exports a consistent source snapshot, creates the ordinary destination, and
// imports with vaire ownership and access. Source roles, passwords, grants and
// cluster settings are never imported, and an existing database is never the
// target.

// copyDestination is the new database's modeled destination inputs. It is
// also the cheap validation preview the node's db.copy and db.copy.preflight
// receive.
type copyDestination struct {
	Name            string
	Size            string
	Memory          string
	Profile         string
	AllowOvercommit bool
}

func (d copyDestination) spec() config.ClusterSpec {
	return config.ClusterSpec{
		Name:             d.Name,
		Size:             d.Size,
		Memory:           d.Memory,
		Profile:          d.Profile,
		StatementTimeout: "60s",
	}
}

func (d copyDestination) params() map[string]any {
	params := map[string]any{"name": d.Name}
	// Sent only when set, so a node with older copy support still decodes
	// the ordinary request.
	if d.Size != "" {
		params["size"] = d.Size
	}
	if d.Memory != "" {
		params["memory"] = d.Memory
	}
	if d.Profile != "" {
		params["profile"] = d.Profile
	}
	if d.AllowOvercommit {
		params["allow_overcommit"] = true
	}
	return params
}

// copySourceInput is the operator's source before the node validates it. It
// holds a URL or the individual fields, never both, plus optional CA content.
type copySourceInput struct {
	URL    string
	Fields *node.CopySourceSecret
	CA     string
}

// secrets returns the exact request secret map: one source form and optional
// CA content. Values are registered for redaction as soon as they are read by
// the caller and again here, before any RPC or error can carry them.
func (s copySourceInput) secrets() (map[string]string, error) {
	out := map[string]string{}
	switch {
	case s.URL != "" && s.Fields != nil:
		return nil, output.NewError(output.CodeValidation,
			"pass either a source URL or the individual source fields, not both", output.ExitConfig, "")
	case s.URL != "":
		secrets.Add(s.URL)
		out[node.CopySourceURLSecret] = s.URL
	case s.Fields != nil:
		raw, err := json.Marshal(s.Fields) //nolint:gosec // G117: the password is the request's secret value, carried only in the encrypted secret map.
		if err != nil {
			return nil, err
		}
		secrets.Add(string(raw))
		secrets.Add(s.Fields.Password)
		out[node.CopySourceSpecSecret] = string(raw)
	default:
		return nil, output.NewError(output.CodeInputRequired,
			"the source connection details were not supplied", output.ExitInput,
			"Pass a newline-terminated postgres:// URL on stdin with --source-url-stdin.").
			WithDetails(map[string]any{"reason": "source_required"})
	}
	if s.CA != "" {
		secrets.Add(s.CA)
		out[node.CopySourceCASecret] = s.CA
	}
	return out, nil
}

// copyPreflightSource is the node's sanitized source facts from
// db.copy.preflight. It deliberately has no password field.
type copyPreflightSource struct {
	Endpoint          string                   `json:"endpoint"`
	URL               string                   `json:"url"`
	ServerVersionText string                   `json:"server_version_text"`
	Database          string                   `json:"database"`
	SizeBytes         int64                    `json:"size_bytes"`
	TLSMode           string                   `json:"tls_mode"`
	Extensions        []backup.SourceExtension `json:"extensions"`
	Warnings          []output.Warning         `json:"warnings"`
}

type copyPreflightTarget struct {
	Database  config.ClusterSpec `json:"database"`
	Resources config.Resources   `json:"resources"`
	Space     []backup.SpaceNeed `json:"space"`
	Advisory  bool               `json:"advisory"`
}

type copyPreflightData struct {
	Source  copyPreflightSource `json:"source"`
	Target  copyPreflightTarget `json:"target"`
	DryRun  bool                `json:"dry_run"`
	Checked bool                `json:"source_checked"`
}

// copyJobResult is the sanitized db-copy job result. Credentials are not
// here; they are generated on the operator and never stored in job results.
type copyJobResult struct {
	Database   config.ClusterSpec      `json:"database"`
	Source     map[string]any          `json:"source"`
	Target     map[string]any          `json:"target"`
	Extensions []backup.ExtensionMatch `json:"extensions"`
	Phase      string                  `json:"phase"`
	Imported   bool                    `json:"imported"`
	Backup     struct {
		Configured bool            `json:"configured"`
		Ready      bool            `json:"ready"`
		Warning    *output.Warning `json:"warning,omitempty"`
	} `json:"backup"`
	Warnings []output.Warning `json:"warnings"`
}

// copyRPC is the narrow remote seam for the command's two node calls. Tests
// provide recording fakes; production wraps the session's node.
type copyRPC struct {
	Preflight func(ctx context.Context, params map[string]any, secrets map[string]string) (remote.Reply, error)
	Start     func(ctx context.Context, params map[string]any, secrets map[string]string, requestID, confirm string, dryRun bool) (remote.Reply, string, bool, error)
}

func runDBCopy(cmd *cobra.Command, args []string) error {
	// A URL secret and later prompts share one buffered stdin: reading the
	// URL must never leave the next prompt with consumed or lost input.
	if flagTrue(cmd, "source-url-stdin") {
		cmd.SetIn(bufio.NewReader(cmd.InOrStdin()))
	}
	name, err := copyName(cmd, args)
	if err != nil {
		return err
	}
	// In a mode that cannot prompt, a missing source is known before any
	// connection: no destination work, source read or password generation.
	if !dryRun(cmd) && MustNotPrompt(cmd) && !flagTrue(cmd, "source-url-stdin") {
		return copyInputRequired("source", "Pass a newline-terminated PostgreSQL URL on stdin with --source-url-stdin.")
	}
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	cfg, _, err := remoteConfig(cmd, s, p)
	if err != nil {
		return err
	}
	dest, err := resolveCopyDestination(cmd, cfg, name)
	if err != nil {
		return err
	}
	var source *copySourceInput
	if !dryRun(cmd) {
		src, err := collectCopySource(cmd)
		if err != nil {
			return err
		}
		source = &src
	}
	calls := copyRPC{
		Preflight: func(ctx context.Context, params map[string]any, sec map[string]string) (remote.Reply, error) {
			return p.call(ctx, s.node, "db.copy.preflight", params, remote.CallOptions{Secrets: sec, OnEvent: p.event})
		},
		Start: func(ctx context.Context, params map[string]any, sec map[string]string, requestID, confirm string, dry bool) (remote.Reply, string, bool, error) {
			opts := remote.CallOptions{Secrets: sec, Confirm: confirm, RequestID: requestID, DryRun: dry, OnEvent: p.event}
			if dry {
				reply, err := p.call(ctx, s.node, "db.copy", params, opts)
				return reply, "", false, err
			}
			return callJob(cmd, s, p, "db.copy", params, opts)
		},
	}
	return executeDBCopy(cmd, p, s.ctx.Name, cfg, dest, source, s.warnings, calls)
}

// copyName takes the new database name from the positional argument or, only
// when prompting is allowed, asks for it. It is validated here so no password,
// source read or RPC happens for a name the node would refuse.
func copyName(cmd *cobra.Command, args []string) (string, error) {
	name := ""
	if len(args) > 0 {
		name = strings.TrimSpace(args[0])
	}
	if name == "" {
		if MustNotPrompt(cmd) {
			return "", copyInputRequired("name", "Pass the new database name: vaire db copy <name>.")
		}
		var err error
		name, err = promptLine(cmd, "Name of the new vaire database", "")
		if err != nil {
			return "", err
		}
		name = strings.TrimSpace(name)
		if name == "" {
			return "", copyInputRequired("name", "Pass the new database name: vaire db copy <name>.")
		}
	}
	if !config.DatabasePattern.MatchString(name) {
		hint := "Use lowercase letters, digits and underscores; the name is also the prefix of its roles."
		if fixed := strings.NewReplacer("-", "_", ".", "_", " ", "_").Replace(strings.ToLower(name)); config.DatabasePattern.MatchString(fixed) {
			hint = "Try " + fixed + ". Hyphens aren't allowed because the name is also the prefix of its roles."
		}
		return "", output.NewError(output.CodeValidation, "database name "+name+" must match "+config.DatabasePattern.String(), output.ExitConfig, hint).
			WithDetails(map[string]any{"name": name})
	}
	if err := config.CheckDatabaseName(name); err != nil {
		return "", output.NewError(output.CodeValidation, "database name "+name+" "+err.Error(), output.ExitConfig, "Pass a shorter name.")
	}
	return name, nil
}

func copyInputRequired(what, hint string) error {
	return output.NewError(output.CodeInputRequired, "db copy needs the "+what, output.ExitInput, hint).
		WithDetails(map[string]any{"reason": "input_required", "input": what})
}

// resolveCopyDestination applies the ordinary size flags and, when prompting
// is allowed, offers the same sizes and resource summary as db create. Size is
// memory, CPU weight and connections; it is never a storage quota.
func resolveCopyDestination(cmd *cobra.Command, cfg config.Cluster, name string) (copyDestination, error) {
	dest := copyDestination{
		Name:            name,
		Size:            stringFlag(cmd, "size"),
		Memory:          stringFlag(cmd, "memory"),
		Profile:         stringFlag(cmd, "profile"),
		AllowOvercommit: flagTrue(cmd, "allow-overcommit"),
	}
	if dest.Size == "" && dest.Memory == "" && !MustNotPrompt(cmd) {
		size, memory, err := promptCopySize(cmd, cfg)
		if err != nil {
			return copyDestination{}, err
		}
		dest.Size, dest.Memory = size, memory
	}
	if err := checkSizeFlags(cfg, dest.spec()); err != nil {
		return copyDestination{}, err
	}
	return dest, nil
}

// promptCopySize offers the configured sizes with their resolved resources,
// never a duplicate size list.
func promptCopySize(cmd *cobra.Command, cfg config.Cluster) (string, string, error) {
	options := make([][2]string, 0, len(config.SizeNames()))
	for _, name := range config.SizeNames() {
		label := name
		if name == config.SizeCustom {
			label = "custom (choose the memory)"
		} else if res, err := cfg.Resources(config.ClusterSpec{Size: name}); err == nil {
			label = fmt.Sprintf("%s (%s memory, cpu weight %d, %d connections)", name, tune.FormatMiB(res.MemoryMiB), res.CPUWeight, res.MaxConnections)
		}
		options = append(options, [2]string{label, name})
	}
	choice, err := chooseOption(cmd, "Size of the new database (memory, CPU and connections, not storage)", options)
	if err != nil {
		return "", "", err
	}
	if choice != config.SizeCustom {
		return choice, "", nil
	}
	memory, err := promptLine(cmd, "Memory for the custom size (for example 768MiB or 2GiB)", "")
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(memory) == "" {
		return "", "", copyInputRequired("custom size memory", "Pass --memory 768MiB (or another amount) with --size custom.")
	}
	return choice, strings.TrimSpace(memory), nil
}

// collectCopySource reads the source URL secret or prompts for the modeled
// individual fields. A URL is never accepted from argv.
func collectCopySource(cmd *cobra.Command) (copySourceInput, error) {
	ca, err := readCopyCAFile(cmd)
	if err != nil {
		return copySourceInput{}, err
	}
	if flagTrue(cmd, "source-url-stdin") {
		raw, err := readCopySourceURL(cmd)
		if err != nil {
			return copySourceInput{}, err
		}
		return copySourceInput{URL: raw, CA: ca}, nil
	}
	if MustNotPrompt(cmd) {
		return copySourceInput{}, copyInputRequired("source", "Pass a newline-terminated PostgreSQL URL on stdin with --source-url-stdin.")
	}
	mode, err := chooseOption(cmd, "How will you provide the source connection?", [][2]string{
		{"Full PostgreSQL URL (masked while typed)", "url"},
		{"Individual host, port, database, user and password", "fields"},
	})
	if err != nil {
		return copySourceInput{}, err
	}
	if mode == "url" {
		raw, err := readSecretTTY(cmd, "Source PostgreSQL URL")
		if err != nil {
			return copySourceInput{}, err
		}
		if strings.TrimSpace(raw) == "" {
			return copySourceInput{}, copyInputRequired("source URL", "Paste a postgres:// or postgresql:// URL.")
		}
		// Register before any validation or display can carry it.
		secrets.Add(raw)
		return copySourceInput{URL: raw, CA: ca}, nil
	}
	fields, err := promptCopySourceFields(cmd)
	if err != nil {
		return copySourceInput{}, err
	}
	return copySourceInput{Fields: fields, CA: ca}, nil
}

// promptCopySourceFields asks for the modeled fields. The password is read
// without echo, and a prompt cancellation has no provisioning or export
// effect.
func promptCopySourceFields(cmd *cobra.Command) (*node.CopySourceSecret, error) {
	host, err := promptLine(cmd, "Source host", "")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(host) == "" {
		return nil, copyInputRequired("source host", "Pass the source host name or address.")
	}
	portText, err := promptLine(cmd, "Source port", "5432")
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(strings.TrimSpace(portText))
	if err != nil || port < 1 || port > 65535 {
		return nil, output.NewError(output.CodeValidation, "the source port must be between 1 and 65535", output.ExitConfig, "")
	}
	database, err := promptLine(cmd, "Source database", "")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(database) == "" {
		return nil, copyInputRequired("source database", "Pass one source database name.")
	}
	user, err := promptLine(cmd, "Source user", "")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(user) == "" {
		return nil, copyInputRequired("source user", "Pass the source user name.")
	}
	password, err := readCopyPassword(cmd)
	if err != nil {
		return nil, err
	}
	// Register immediately: a later TLS prompt cancellation must not expose
	// the password in an error or log.
	secrets.Add(password)
	tlsMode, err := chooseOption(cmd, "Source TLS mode", [][2]string{
		{"verify-full (verify the certificate and hostname; default)", backup.SourceTLSVerifyFull},
		{"verify-ca (verify the certificate, not the hostname)", backup.SourceTLSVerifyCA},
		{"require (encrypt but do not verify; vaire warns)", backup.SourceTLSRequire},
	})
	if err != nil {
		return nil, err
	}
	return &node.CopySourceSecret{
		Host:     strings.TrimSpace(host),
		Port:     port,
		Database: strings.TrimSpace(database),
		User:     strings.TrimSpace(user),
		Password: password,
		TLSMode:  tlsMode,
	}, nil
}

// Source password input is exact-byte in both renderers: PostgreSQL
// passwords may begin or end with spaces, unlike the general passphrase
// prompts, which keep their established trimming policy.
var (
	copyPasswordRead = func() (string, error) {
		raw, err := term.ReadPassword(int(os.Stdin.Fd())) //nolint:gosec // G115: file descriptors fit in int.
		return string(raw), err
	}
	copyPasswordFromUI = ui.SecretExact
)

func readCopyPassword(cmd *cobra.Command) (string, error) {
	return readCopyPasswordWith(richOutput(cmd), cmd.ErrOrStderr(), "Source password (empty only when the source needs none)")
}

// readCopyPasswordWith is the renderer-independent exact-byte input seam.
// Rootless tests replace both readers and exercise the plain and rich paths
// without a terminal. The plain path writes its label and final newline to w
// before and after the masked reader, never the password itself.
func readCopyPasswordWith(rich bool, w io.Writer, label string) (string, error) {
	var value string
	var err error
	if rich {
		value, err = copyPasswordFromUI(label)
	} else {
		if _, werr := fmt.Fprint(w, label+": "); werr != nil {
			return "", werr
		}
		value, err = copyPasswordRead()
		_, _ = fmt.Fprintln(w)
	}
	if err != nil {
		if rich {
			return "", promptErr(err)
		}
		return "", output.WrapError(output.CodeInputRequired, err, output.ExitInput, "")
	}
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", output.NewError(output.CodeValidation,
			"the source password cannot contain a line break or NUL byte", output.ExitConfig,
			"A PostgreSQL password file cannot carry those characters; enter one password without a line break or NUL byte.").
			WithDetails(map[string]any{"reason": "invalid_source_password"})
	}
	return value, nil
}

// readCopySourceURL reads exactly one newline-terminated URL. EOF without a
// newline is an input error, not a partially accepted URL. The reader is left
// on the command so a later prompt reads after the consumed line.
func readCopySourceURL(cmd *cobra.Command) (string, error) {
	reader, ok := cmd.InOrStdin().(*bufio.Reader)
	if !ok {
		reader = bufio.NewReader(cmd.InOrStdin())
		cmd.SetIn(reader)
	}
	line, err := reader.ReadString('\n')
	raw := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if strings.TrimSpace(raw) != "" {
		// Register before the newline/EOF check can return an error.
		secrets.Add(raw)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return "", output.WrapError(output.CodeInputRequired, err, output.ExitInput, "")
	}
	if err != nil || !strings.HasSuffix(line, "\n") {
		return "", copyInputRequired("source URL on stdin", "Pipe one newline-terminated PostgreSQL URL: printf '%s\\n' \"$URL\" | vaire db copy <name> --source-url-stdin.")
	}
	if strings.TrimSpace(raw) == "" {
		return "", copyInputRequired("source URL on stdin", "The first stdin line was empty; pipe one PostgreSQL URL.")
	}
	if strings.ContainsAny(raw, "\r\n") {
		return "", output.NewError(output.CodeValidation, "the source URL must be one line", output.ExitConfig,
			"Pipe exactly one newline-terminated URL.")
	}
	return raw, nil
}

// readCopyCAFile reads certificate content, never a path to send onwards. The
// path is the operator's local choice; the node and the job receive only the
// PEM content in the copy.source_ca secret.
func readCopyCAFile(cmd *cobra.Command) (string, error) {
	path := stringFlag(cmd, "source-ca-file")
	if path == "" {
		return "", nil
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the operator chose this CA file.
	if err != nil {
		return "", output.WrapError(output.CodeUsage, err, output.ExitUsage,
			"Pass a readable PEM CA certificate with --source-ca-file.")
	}
	content := string(raw)
	if strings.TrimSpace(content) == "" {
		return "", output.NewError(output.CodeValidation, "the source CA file is empty", output.ExitConfig,
			"Pass a PEM-encoded CA certificate, or omit --source-ca-file to use the system trust store.")
	}
	secrets.Add(content)
	return content, nil
}

// executeDBCopy is the node-independent orchestration: dry run, real
// read-only preflight, confirmation, one-time credentials and the detached
// job. Tests drive it through copyRPC; production's copyRPC uses the session.
func executeDBCopy(cmd *cobra.Command, p *progress, contextName string, cfg config.Cluster, dest copyDestination, source *copySourceInput, ownWarnings []output.Warning, calls copyRPC) error {
	if dryRun(cmd) {
		reply, _, _, err := calls.Start(commandContext(cmd), dest.params(), nil, "", "", true)
		if err != nil {
			return err
		}
		var data map[string]any
		_ = reply.Decode(&data)
		summary := copyDestinationSummary(cfg, dest)
		return output.Success(cmd, output.Result{
			Data: data,
			Message: fmt.Sprintf("dry run: would copy into a new database %s (%s); no source connection was made, so source compatibility and reachability are not checked",
				dest.Name, summary),
			Warnings: append(ownWarnings, reply.Warnings...),
		})
	}
	if source == nil {
		return copyInputRequired("source", "Pass a newline-terminated PostgreSQL URL on stdin with --source-url-stdin.")
	}
	sourceSecrets, err := source.secrets()
	if err != nil {
		return err
	}
	// Refuse an impossible confirmation before touching the source; an
	// actual confirmation still comes after the preflight summary below.
	if (MustNotPrompt(cmd) || flagTrue(cmd, "yes")) && len(confirmTokens(cmd)) == 0 {
		if err := Confirm(cmd, dest.Name); err != nil {
			return err
		}
	}
	reply, err := calls.Preflight(commandContext(cmd), dest.params(), sourceSecrets)
	if err != nil {
		return err
	}
	var pre copyPreflightData
	if err := reply.Decode(&pre); err != nil {
		return err
	}
	warnings := append(append([]output.Warning{}, ownWarnings...), reply.Warnings...)
	warnings = append(warnings, pre.Source.Warnings...)
	for _, warning := range warnings {
		if warning.Message != "" {
			p.warn(warning.Message)
		}
	}
	p.panel("Copy summary", copySummaryLines(contextName, cfg, dest, pre))
	if err := Confirm(cmd, dest.Name); err != nil {
		return err
	}

	secret, roleSecrets, err := newDatabaseSecret(cfg, dest.Name, steps.DatabaseRoles)
	if err != nil {
		return err
	}
	service, creds, generated, err := serviceSecrets(contextName)
	if err != nil {
		return err
	}
	for k, v := range service {
		roleSecrets[k] = v
	}
	addRecoveryKit(roleSecrets, creds.Backup)
	for k, v := range sourceSecrets {
		roleSecrets[k] = v
	}
	warnings = append(warnings, staleWarning(generated)...)

	// The node names the db-copy job after this request id. Selecting it
	// before the call is what lets a detached or lost reply still hand over
	// the one-time credentials and the job id.
	jobID := rpc.NewRequestID()
	reply, announced, detached, err := calls.Start(commandContext(cmd), dest.params(), roleSecrets, jobID, dest.Name, false)
	if announced != "" {
		jobID = announced
	}
	if err != nil {
		return copyStartFailure(cmd, err, jobID, dest.Name, secret, warnings)
	}
	if detached {
		return copyDetachedResult(cmd, jobID, dest.Name, pre, secret, warnings)
	}
	var res copyJobResult
	jr, err := decodeJob(reply, &res)
	if err != nil {
		return copyResultFailure(cmd, jobID, dest.Name, secret, warnings)
	}
	if jr.JobID != "" {
		jobID = jr.JobID
	}
	warnings = append(warnings, reply.Warnings...)
	warnings = append(warnings, res.Warnings...)
	from := copySourceLabel(pre)
	headline := "copied " + from + " into " + dest.Name + " with its own roles"
	if res.Backup.Configured && res.Backup.Ready {
		headline += " and first backup"
	}
	message, human := credentialsMessage(cmd, headline, headline, dest.Name, secret)
	return output.Success(cmd, output.Result{
		Data: map[string]any{
			"job_id":   jobID,
			"source":   pre.Source,
			"database": secret,
			"cluster":  res.Database,
			"target":   res.Target,
			"backup":   res.Backup,
		},
		Message:  message,
		Human:    human,
		Warnings: warnings,
		Next:     []string{"vaire db list", "vaire status --db " + dest.Name},
	})
}

// copyDestinationSummary is the effective resource summary shown before
// confirmation and on a dry run.
func copyDestinationSummary(cfg config.Cluster, dest copyDestination) string {
	res, err := cfg.Resources(dest.spec())
	if err != nil {
		return dest.Size
	}
	return copyResolvedSummary(res)
}

// copyResolvedSummary renders the node's resolved resources: memory, CPU
// weight and connections, never a disk allocation.
func copyResolvedSummary(res config.Resources) string {
	out := fmt.Sprintf("%s: %s memory, cpu weight %d, %d connections", res.Size, tune.FormatMiB(res.MemoryMiB), res.CPUWeight, res.MaxConnections)
	if res.MemoryMaxMiB > 0 {
		out += fmt.Sprintf(", max %s", tune.FormatMiB(res.MemoryMaxMiB))
	}
	if res.CPUQuota != "" {
		out += ", cpu quota " + res.CPUQuota
	}
	return out
}

// copySummaryLines is the redacted confirmation summary. It names only the
// node's sanitized source facts: never a URL password, raw URI or CA content.
func copySummaryLines(contextName string, cfg config.Cluster, dest copyDestination, pre copyPreflightData) []string {
	source := pre.Source.Endpoint
	if source == "" {
		source = pre.Source.URL
	}
	if source == "" {
		source = "the external source"
	}
	if pre.Source.Database != "" {
		source += " (database " + pre.Source.Database + ")"
	}
	version := strings.TrimSpace(pre.Source.ServerVersionText)
	if version == "" {
		version = "version not reported"
	}
	size := ""
	if pre.Source.SizeBytes > 0 {
		size = ", " + humanBytes(pre.Source.SizeBytes)
	}
	tls := strings.TrimSpace(pre.Source.TLSMode)
	if tls == "" {
		tls = "TLS mode not reported"
	}
	resources := copyDestinationSummary(cfg, dest)
	if pre.Target.Resources.Size != "" {
		resources = copyResolvedSummary(pre.Target.Resources)
	}
	lines := []string{
		"Source: " + source,
		fmt.Sprintf("Source server: PostgreSQL %s%s, %s, %d extension(s)", version, size, tls, len(pre.Source.Extensions)),
		"Destination: context " + contextName + ", new database " + dest.Name + " (" + resources + ")",
		"Size means memory, CPU and connections, not a storage quota; the source data may need more disk than the estimate.",
		"Reachability: the destination node runs the export, so the source must accept connections from that server; check its provider or firewall allowlist. The operator machine is not used for the transfer.",
		"Snapshot: one consistent read-only snapshot is taken when the export starts. Later source changes are not copied, and the source is not modified apart from normal read/export load and locks.",
		"Trusted source: the dump can contain executable SQL. Copy only from a source you trust. Source roles, passwords, grants, tablespaces and cluster settings are not imported.",
		"Target: a brand-new database is created; an existing database is never an import target. The new database is unprotected unless you protect it afterwards.",
		"Credentials: new vaire role passwords are generated here and shown once. If the copy may have started, they are kept and shown even when the reply is lost or its outcome is unknown.",
	}
	return lines
}

// copySourceLabel is the sanitized source in success and detached messages.
func copySourceLabel(pre copyPreflightData) string {
	source := pre.Source.Endpoint
	if source == "" {
		source = pre.Source.URL
	}
	if source == "" {
		return "the external source"
	}
	if pre.Source.Database != "" {
		return source + " (database " + pre.Source.Database + ")"
	}
	return source
}

// copyDetachedResult prints the one-time credentials after Ctrl-C detached
// from a copy that may already run on the node.
func copyDetachedResult(cmd *cobra.Command, jobID, name string, pre copyPreflightData, secret databaseSecret, warnings []output.Warning) error {
	headline := "detached: the copy of " + copySourceLabel(pre) + " into " + name + " keeps running as job " + jobID +
		"; these credentials work once it finishes and are shown only now"
	message, human := credentialsMessage(cmd, headline, headline, name, secret)
	next := []string{"vaire job list"}
	if jobID != "" {
		next = []string{"vaire job follow " + jobID}
	}
	return output.Success(cmd, output.Result{
		Data:     map[string]any{"job_id": jobID, "detached": true, "database": secret},
		Message:  message,
		Human:    human,
		Warnings: warnings,
		Next:     next,
	})
}

// copyStartFailure keeps the generated credentials and job id whenever the
// db.copy request may have started a job that used them. A definite refusal
// before launch stays unchanged; an unknown start is never reported as a
// definite failure. A cleanup failure after an import is still a failure and
// the target is retained, with no replay or apply advice.
func copyStartFailure(cmd *cobra.Command, err error, jobID, name string, secret databaseSecret, warnings []output.Warning) error {
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) || !copyCredentialsMaybeUsed(cliErr) {
		return err
	}
	details := map[string]any{}
	if original, ok := cliErr.Details.(map[string]any); ok {
		for k, v := range original {
			details[k] = v
		}
	}
	if jobID != "" {
		if _, exists := details["job_id"]; !exists {
			details["job_id"] = jobID
		}
	}
	note := copyCredentialNote(details, name)
	details["database"] = secret
	details["credentials_note"] = note + " " + shownOnceNote
	out := *cliErr
	out.Details = details
	if jobID != "" {
		out.Hint = strings.TrimSpace(out.Hint + " Check vaire job follow " + jobID + " and vaire db list before retrying or reusing the name; another copy needs a new unused name.")
	} else {
		out.Hint = strings.TrimSpace(out.Hint + " Check vaire job list and vaire db list before retrying or reusing the name; another copy needs a new unused name.")
	}
	for _, warning := range warnings {
		if !containsWarning(out.Warnings, warning) {
			out.Warnings = append(out.Warnings, warning)
		}
	}
	printCopyCredentials(cmd, note, copyCredentialHeadline(details, name), name, secret)
	return &out
}

// copyResultFailure keeps the credentials when the node answered a started
// copy successfully but its result could not be read. The job may have
// completed, so the one-time handoff must not be discarded behind a decode
// error.
func copyResultFailure(cmd *cobra.Command, jobID, name string, secret databaseSecret, warnings []output.Warning) error {
	note := "The copy job returned a result that could not be read. It may have finished and installed these generated credentials. Check the job and database list before reusing or deciding anything about " + name + "."
	out := output.NewError(output.CodeError,
		"the copy finished but its result could not be read", output.ExitGeneric,
		"Keep these credentials and the job id; check vaire job follow "+jobID+" and vaire db list before reusing the name.").
		WithDetails(map[string]any{
			"reason":           "copy_result_unreadable",
			"job_id":           jobID,
			"database":         secret,
			"credentials_note": note + " " + shownOnceNote,
		})
	out.Warnings = warnings
	printCopyCredentials(cmd, note, "copy finished but its result could not be read; credentials for "+name, name, secret)
	return out
}

// copyCredentialsMaybeUsed reports whether the request may have installed or
// used the generated role passwords: a lost connection, an ambiguous launch,
// or any failure after a destination artefact exists. A definite refusal that
// happened before the job or target existed never hands them over.
func copyCredentialsMaybeUsed(cliErr *output.CLIError) bool {
	if cliErr.Code == output.CodeTransportFailed {
		return true
	}
	details, _ := cliErr.Details.(map[string]any)
	if used, _ := details["credentials_may_be_used"].(bool); used {
		return true
	}
	if observed, _ := details["observation_failed"].(bool); observed {
		return true
	}
	if imported, _ := details["imported"].(bool); imported {
		return true
	}
	if present, _ := details["target_present"].(bool); present {
		return true
	}
	reason, _ := details["reason"].(string)
	// A job.follow observation failure carries no credentials flag because
	// the follow RPC does not know the job type; the copy command knows the
	// job ID it started and must keep its one-time handoff. A record that
	// disappeared after the launch was confirmed is the same uncertainty.
	return reason == node.JobOutcomeUnknown || reason == node.JobObservationFailed || reason == "job_not_found"
}

// copyCredentialNote explains what the retained target may be without
// claiming a definite success or advising a replay.
func copyCredentialNote(details map[string]any, name string) string {
	imported, _ := details["imported"].(bool)
	present, _ := details["target_present"].(bool)
	switch {
	case imported:
		return "The copy failed after the import ran. The database " + name + " is retained with the data it received; vaire never replays the import into it, drops it automatically or treats a retry as permission to overwrite it. These generated credentials are the ones to try if it holds the data you expected."
	case present:
		return "The copy left a destination for " + name + " behind, so its roles and these generated credentials may exist. vaire retains it and never replays the import into it; choose a new name for another copy. These credentials are the ones to try if the database is usable."
	default:
		return "The copy's outcome is unknown: the job may have created " + name + " and installed these generated credentials, or it may have stopped before any destination existed. Check the job and database list before deciding. Never retry this import into " + name + "."
	}
}

func copyCredentialHeadline(details map[string]any, name string) string {
	imported, _ := details["imported"].(bool)
	if imported {
		return "copy failed after its import; credentials and the retained database " + name
	}
	if present, _ := details["target_present"].(bool); present {
		return "copy failed with a retained destination; credentials for " + name
	}
	return "copy outcome unknown; credentials for " + name
}

// printCopyCredentials renders the one-time credentials of a failed copy for
// human output. Structured output carries them in error.details.database.
func printCopyCredentials(cmd *cobra.Command, headline, richHeadline, title string, secret databaseSecret) {
	opts, err := output.ParseOptions(cmd)
	if err == nil && output.UsesStructuredOutput(opts) {
		return
	}
	plain, human := credentialsMessage(cmd, headline, richHeadline, title, secret)
	text := plain
	if human != "" {
		text = human
	}
	resume := ui.Suspend()
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), text)
	resume()
}

func containsWarning(warnings []output.Warning, warning output.Warning) bool {
	for _, existing := range warnings {
		if existing == warning {
			return true
		}
	}
	return false
}
