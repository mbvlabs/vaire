package cli

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/cli/ui"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/contexts"
	"github.com/mbvlabs/vaire/internal/secrets"
	"github.com/mbvlabs/vaire/internal/secretsdoc"
	"github.com/mbvlabs/vaire/internal/steps"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// ensureClusterSecrets fills the operator's cluster-wide database
// credentials, the admin and service roles, generating any password it does
// not have (D7). It returns the RPC secret map and what it generated, for
// the secrets_document_stale warning. Application database passwords are
// never generated here: only vaire db create and vaire db credentials --rotate
// make them.
func ensureClusterSecrets(creds *contexts.DatabaseCreds) (map[string]string, []string, error) {
	if creds.Admin.User == "" {
		creds.Admin.User = "vaire_admin"
	}
	var generated []string
	rpc := map[string]string{}

	if creds.Admin.Password == "" {
		pw, err := secrets.Base62(32)
		if err != nil {
			return nil, nil, err
		}
		creds.Admin.Password = pw
		generated = append(generated, "the "+creds.Admin.User+" password")
	}
	rpc[steps.SecretAdminPassword] = creds.Admin.Password

	if creds.Services == nil {
		creds.Services = map[string]contexts.Role{}
	}
	for _, role := range steps.ServiceRoles {
		r := creds.Services[role]
		if r.User == "" {
			r.User = role
		}
		if r.Password == "" {
			pw, err := secrets.Base62(32)
			if err != nil {
				return nil, nil, err
			}
			r.Password = pw
			generated = append(generated, "the "+role+" password")
		}
		creds.Services[role] = r
		rpc[steps.ServiceRoleSecret(role)] = r.Password
	}
	return rpc, generated, nil
}

// databaseSecret is one application database's roles and connection URLs.
// vaire shows it once and never stores it.
type databaseSecret struct {
	Name  string                   `json:"name"`
	Roles map[string]contexts.Role `json:"roles"`
	URLs  map[string]string        `json:"urls"`
	Notes string                   `json:"notes"`
}

// newDatabaseSecret generates passwords for roles of database name and
// returns them with the RPC secret map.
func newDatabaseSecret(cfg config.Cluster, name string, roles []string) (databaseSecret, map[string]string, error) {
	out := databaseSecret{Name: name, Roles: map[string]contexts.Role{}, Notes: secretsdoc.ConnectionNotes}
	rpc := map[string]string{}
	for _, role := range roles {
		pw, err := secrets.Base62(32)
		if err != nil {
			return databaseSecret{}, nil, err
		}
		out.Roles[role] = contexts.Role{User: name + "_" + role, Password: pw}
		rpc[steps.SecretDatabaseRole(name, role)] = pw
	}
	out.URLs = secretsdoc.DatabaseURLs(cfg.DatabaseHostname(name), cfg.TLS.Mode, name, out.Roles)
	return out, rpc, nil
}

// String renders the roles, passwords and URLs for a human.
func (d databaseSecret) String() string {
	var lines []string
	for _, role := range steps.DatabaseRoles {
		if r, ok := d.Roles[role]; ok {
			lines = append(lines, fmt.Sprintf("  %s user: %s", role, r.User), fmt.Sprintf("  %s password: %s", role, r.Password))
		}
	}
	for _, k := range sortedStrings(d.URLs) {
		lines = append(lines, "  "+k+" URL: "+d.URLs[k])
	}
	return strings.Join(lines, "\n")
}

func sortedStrings(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// staleWarning is the warning emitted when the operator generated secrets
// that are not yet in the secrets document (D7). generated names them, for
// example "a /metrics credential".
func staleWarning(generated []string) []output.Warning {
	if len(generated) == 0 {
		return nil
	}
	verb := "was"
	if len(generated) > 1 {
		verb = "were"
	}
	return []output.Warning{{
		Code:    "secrets_document_stale",
		Message: joinAnd(generated) + " " + verb + " generated; run vaire secrets export to refresh the secrets document",
	}}
}

// joinAnd joins items as "a", "a and b" or "a, b and c".
func joinAnd(items []string) string {
	if len(items) <= 1 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// serviceSecrets tops up the context's vaire_admin and service passwords (D7)
// and returns them as the RPC secret map, with the loaded credentials and
// whether anything was generated. Every command that may create a cluster
// sends them: the first cluster computes its service verifiers from them.
func serviceSecrets(contextName string) (map[string]string, contexts.Credentials, []string, error) {
	creds, err := contexts.LoadCredentials(contextName)
	if err != nil {
		return nil, creds, nil, err
	}
	if creds.Database == nil {
		creds.Database = &contexts.DatabaseCreds{}
	}
	values, generated, err := ensureClusterSecrets(creds.Database)
	if err != nil {
		return nil, creds, nil, err
	}
	// The /metrics credential tops up the same way (Phase 4): metrics-v1
	// writes its hash only where the node has none. It is saved below, so it
	// is generated once.
	metricsGenerated, err := addMetricsSecret(&creds.Metrics, values)
	if err != nil {
		return nil, creds, nil, err
	}
	if metricsGenerated {
		generated = append(generated, "a /metrics credential")
	}
	if len(generated) > 0 {
		if err := contexts.SaveCredentials(contextName, creds); err != nil {
			return nil, creds, nil, err
		}
	}
	return values, creds, generated, nil
}

// credentialsMessage renders one-time credentials as the plain result
// (headline, the indented lines and the shown-once note) and, on a terminal
// that takes color, the styled block for human output; human is empty
// otherwise. richHeadline is the line above the block, whose frame is titled
// title.
func credentialsMessage(cmd *cobra.Command, headline, richHeadline, title string, secret databaseSecret) (plain, human string) {
	plain = headline + "\n" + secret.String() + "\n" + shownOnceNote
	width, ok := styledStdout(cmd)
	if !ok {
		return plain, ""
	}
	return plain, secret.styled(richHeadline, title, shownOnceNote, width)
}

// styled renders the credentials block for a terminal width columns wide.
func (d databaseSecret) styled(headline, title, note string, width int) string {
	var rows []ui.CredentialRow
	for _, role := range steps.DatabaseRoles {
		if r, ok := d.Roles[role]; ok {
			rows = append(rows, ui.CredentialRow{Role: role, User: r.User, Password: r.Password})
		}
	}
	var urls []ui.URLRow
	for _, k := range sortedStrings(d.URLs) {
		urls = append(urls, ui.URLRow{Label: k + " URL", URL: d.URLs[k]})
	}
	return ui.Credentials(headline, title, rows, urls, note, width)
}

// styledStdout reports whether the human result may be styled, with the
// terminal's width: the terminal UI is on, stdout is that terminal and
// NO_COLOR is unset. Anything else keeps the plain lines.
func styledStdout(cmd *cobra.Command) (int, bool) {
	if os.Getenv("NO_COLOR") != "" || !richOutput(cmd) || cmd.OutOrStdout() != os.Stdout || !ui.Enabled(os.Stdout) {
		return 0, false
	}
	width := 80
	if cols, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && cols > 0 { //nolint:gosec // G115: file descriptors fit in int.
		width = cols
	}
	return width, true
}

// createInterrupted handles a db create whose call failed after the request
// went out with its generated passwords: a lost stream or a node that ended
// without a result (transport_failed). The node keeps creating the database
// with those passwords, and nothing else can show them again, so they are
// printed (human and markdown output) and carried in the error's details
// (json and agent output). A node that answered with a refusal or a failure
// is passed through unchanged.
func createInterrupted(cmd *cobra.Command, err error, name, title string, secret databaseSecret) error {
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) || cliErr.Code != output.CodeTransportFailed {
		return err
	}
	note := "The connection was lost after the request was sent, so the node may still be creating " + name +
		" with these credentials. Check with vaire db list: vaire apply finishes a database it left half made; if " + name + " is not listed, nothing was created and these are unused."
	out := *cliErr
	details := map[string]any{}
	if m, ok := cliErr.Details.(map[string]any); ok {
		for k, v := range m {
			details[k] = v
		}
	}
	details["database"] = secret
	details["credentials_note"] = note + " " + shownOnceNote
	out.Details = details
	out.Hint = strings.TrimSpace(out.Hint + " The credentials were printed above (details.database); vaire apply finishes " + name + ".")
	opts, _ := output.ParseOptions(cmd)
	if !output.UsesStructuredOutput(opts) {
		text := note + "\n" + secret.String() + "\n" + shownOnceNote
		if width, ok := styledStdout(cmd); ok {
			text = ui.Warn(note) + "\n" + secret.styled("", title, shownOnceNote, width)
		}
		resume := ui.Suspend()
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), text)
		resume()
	}
	return &out
}
