package cli

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"charm.land/huh/v2"
	"github.com/mbvlabs/eru/internal/cli/ui"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/spf13/cobra"
)

// interviewAnswers is the Phase 2 question set (D13).
type interviewAnswers struct {
	Name              string
	AdminUser         string
	Timezone          string
	DataDirectory     string
	Domain            string
	TLSMode           string
	ACMEEmail         string
	ClientConnections string
	Profile           string
	Allowlist         string
}

// Interview question keys, used to skip what flags or --answers supplied.
const (
	qName              = "name"
	qAdminUser         = "admin_user"
	qTimezone          = "timezone"
	qDataDirectory     = "data_directory"
	qDomain            = "domain"
	qTLSMode           = "tls_mode"
	qACMEEmail         = "acme_email"
	qAllowlist         = "allowlist"
	qProfile           = "profile"
	qClientConnections = "client_connections"
)

// suppliedAnswers reports which questions a flag or the answers file already
// answered. An answers-file value counts when it differs from the default.
func suppliedAnswers(cmd *cobra.Command, cfg config.Cluster, fromFile bool) map[string]bool {
	def := config.DefaultCluster()
	given := func(flag string, differs bool) bool {
		return flagChanged(cmd, flag) || (fromFile && differs)
	}
	return map[string]bool{
		qName:              cfg.ClusterName != "",
		qAdminUser:         given("admin-user", cfg.Server.AdminUser != def.Server.AdminUser),
		qTimezone:          given("timezone", cfg.Timezone != def.Timezone),
		qDataDirectory:     given("data-directory", cfg.Server.DataDirectory != def.Server.DataDirectory),
		qDomain:            given("domain", cfg.Domain != ""),
		qTLSMode:           given("tls-mode", cfg.TLS.Mode != def.TLS.Mode),
		qACMEEmail:         given("acme-email", cfg.ACMEEmail != "") || cfg.TLS.Mode == "self-signed",
		qAllowlist:         given("allow", len(cfg.Network.ClientAllowlist) > 0),
		qProfile:           given("profile", cfg.Postgres.Profile != def.Postgres.Profile),
		qClientConnections: given("client-connections", cfg.Postgres.ClientConnections != def.Postgres.ClientConnections),
	}
}

// runInterview asks the Phase 2 questions with huh (D13). It runs only in a
// TTY, pre-fills each field with its default, and skips the questions a flag
// or the answers file already answered. Each field is checked as it is
// answered, so the form cannot be submitted with a value Validate rejects.
func runInterview(_ *cobra.Command, cfg *config.Cluster, supplied map[string]bool) error {
	a := interviewAnswers{
		Name:              cfg.ClusterName,
		AdminUser:         cfg.Server.AdminUser,
		Timezone:          cfg.Timezone,
		DataDirectory:     cfg.Server.DataDirectory,
		Domain:            cfg.Domain,
		TLSMode:           cfg.TLS.Mode,
		ACMEEmail:         cfg.ACMEEmail,
		ClientConnections: strconv.Itoa(cfg.Postgres.ClientConnections),
		Profile:           cfg.Postgres.Profile,
		Allowlist:         strings.Join(cfg.Network.ClientAllowlist, ", "),
	}
	ask := func(key string, field huh.Field, fields *[]huh.Field) {
		if !supplied[key] {
			*fields = append(*fields, field)
		}
	}
	var server, endpoint, workload []huh.Field
	ask(qName, huh.NewInput().Title("Cluster name").Validate(validateName).Value(&a.Name), &server)
	ask(qAdminUser, huh.NewInput().Title("Admin user").Validate(optional(config.CheckAdminUser)).Value(&a.AdminUser), &server)
	ask(qTimezone, huh.NewInput().Title("Timezone").Validate(optional(config.CheckTimezone)).Value(&a.Timezone), &server)
	ask(qDataDirectory, huh.NewInput().Title("Data directory").Validate(optional(config.CheckDataDirectory)).Value(&a.DataDirectory), &server)
	ask(qDomain, huh.NewInput().Title("Client-facing domain").Validate(validateDomain).Value(&a.Domain), &endpoint)
	ask(qTLSMode, huh.NewSelect[string]().
		Title("TLS mode").
		Options(huh.NewOption("ACME (Let's Encrypt)", "acme"), huh.NewOption("Self-signed", "self-signed")).
		Value(&a.TLSMode), &endpoint)
	ask(qAllowlist, huh.NewInput().
		Title("Client allowlist (IPs or CIDRs, comma separated, empty for none)").
		Validate(validateAllowlist).
		Value(&a.Allowlist), &endpoint)
	ask(qProfile, huh.NewSelect[string]().
		Title("Workload profile").
		Options(huh.NewOption("OLTP (web)", "oltp"), huh.NewOption("Mixed", "mixed"), huh.NewOption("Analytics", "analytics")).
		Value(&a.Profile), &workload)
	ask(qClientConnections, huh.NewInput().Title("Expected client connections").Validate(validateClientConnections).Value(&a.ClientConnections), &workload)

	var groups []*huh.Group
	for _, fields := range [][]huh.Field{server, endpoint, workload} {
		if len(fields) > 0 {
			groups = append(groups, huh.NewGroup(fields...))
		}
	}
	// The ACME email is asked after the TLS mode is known, and only for acme.
	if !supplied[qACMEEmail] {
		groups = append(groups, huh.NewGroup(
			huh.NewInput().
				Title("ACME contact email (optional)").
				Description("Only the account contact for Let's Encrypt. Certificates renew automatically; leave it empty if you like.").
				Validate(optional(config.CheckACMEEmail)).
				Value(&a.ACMEEmail),
		).WithHideFunc(func() bool { return a.TLSMode != "acme" }))
	}
	if len(groups) == 0 {
		return nil
	}
	if err := ui.RunForm(groups...); err != nil {
		return promptErr(err)
	}
	return applyInterview(cfg, a)
}

// applyInterview writes the answers back into cfg.
func applyInterview(cfg *config.Cluster, a interviewAnswers) error {
	setIf := func(dst *string, v string) {
		if strings.TrimSpace(v) != "" {
			*dst = strings.TrimSpace(v)
		}
	}
	setIf(&cfg.ClusterName, a.Name)
	setIf(&cfg.Server.AdminUser, a.AdminUser)
	setIf(&cfg.Timezone, a.Timezone)
	setIf(&cfg.Server.DataDirectory, a.DataDirectory)
	setIf(&cfg.Domain, a.Domain)
	setIf(&cfg.TLS.Mode, a.TLSMode)
	setIf(&cfg.ACMEEmail, a.ACMEEmail)
	setIf(&cfg.Postgres.Profile, a.Profile)
	if n, err := strconv.Atoi(strings.TrimSpace(a.ClientConnections)); err == nil && n > 0 {
		cfg.Postgres.ClientConnections = n
	}
	cfg.Network.ClientAllowlist = allowlistPrefixes(splitList(a.Allowlist))
	return nil
}

// optional wraps a config check for a field where empty keeps the default.
func optional(check func(string) error) func(string) error {
	return func(v string) error {
		if v = strings.TrimSpace(v); v == "" {
			return nil
		}
		return check(v)
	}
}

func validateName(v string) error {
	if !config.NamePattern.MatchString(strings.TrimSpace(v)) {
		return errors.New("must match " + config.NamePattern.String())
	}
	return nil
}

func validateDomain(v string) error {
	if strings.TrimSpace(v) == "" {
		return errors.New("a domain is required")
	}
	return config.CheckDomain(strings.TrimSpace(v))
}

func validateAllowlist(v string) error {
	for _, entry := range allowlistPrefixes(splitList(v)) {
		if err := config.CheckAllowlistEntry(entry); err != nil {
			return fmt.Errorf("%s is not an IP address or CIDR", entry)
		}
	}
	return nil
}

func validateClientConnections(v string) error {
	if v = strings.TrimSpace(v); v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return errors.New("must be a number")
	}
	return config.CheckClientConnections(n)
}

// allowlistPrefixes turns a bare IP address into its single-host prefix
// (/32 or /128); anything else is kept for validation to report.
func allowlistPrefixes(entries []string) []string {
	for i, e := range entries {
		if ip, err := netip.ParseAddr(e); err == nil {
			entries[i] = netip.PrefixFrom(ip, ip.BitLen()).String()
		}
	}
	return entries
}

// splitList splits a comma-separated list, dropping empties.
func splitList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
