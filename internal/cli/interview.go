package cli

import (
	"strconv"
	"strings"

	"charm.land/huh/v2"
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
	Databases         string
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
	qDatabases         = "databases"
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
		qDatabases:         given("database", len(cfg.Databases) > 0),
	}
}

// runInterview asks the Phase 2 questions with huh (D13). It runs only in a
// TTY, pre-fills each field with its default, and skips the questions a flag
// or the answers file already answered.
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
		Databases:         strings.Join(databaseNames(*cfg), ", "),
		Allowlist:         strings.Join(cfg.Network.ClientAllowlist, ", "),
	}
	ask := func(key string, field huh.Field, fields *[]huh.Field) {
		if !supplied[key] {
			*fields = append(*fields, field)
		}
	}
	var server, endpoint, workload, databases []huh.Field
	ask(qName, huh.NewInput().Title("Cluster name").Value(&a.Name), &server)
	ask(qAdminUser, huh.NewInput().Title("Admin user").Value(&a.AdminUser), &server)
	ask(qTimezone, huh.NewInput().Title("Timezone").Value(&a.Timezone), &server)
	ask(qDataDirectory, huh.NewInput().Title("Data directory").Value(&a.DataDirectory), &server)
	ask(qDomain, huh.NewInput().Title("Client-facing domain").Value(&a.Domain), &endpoint)
	ask(qTLSMode, huh.NewSelect[string]().
		Title("TLS mode").
		Options(huh.NewOption("ACME (Let's Encrypt)", "acme"), huh.NewOption("Self-signed", "self-signed")).
		Value(&a.TLSMode), &endpoint)
	ask(qAllowlist, huh.NewInput().Title("Client allowlist (CIDRs, comma separated, empty for none)").Value(&a.Allowlist), &endpoint)
	ask(qProfile, huh.NewSelect[string]().
		Title("Workload profile").
		Options(huh.NewOption("OLTP (web)", "oltp"), huh.NewOption("Mixed", "mixed"), huh.NewOption("Analytics", "analytics")).
		Value(&a.Profile), &workload)
	ask(qClientConnections, huh.NewInput().Title("Expected client connections").Value(&a.ClientConnections), &workload)
	ask(qDatabases, huh.NewInput().Title("Databases (comma separated)").Value(&a.Databases), &databases)

	var groups []*huh.Group
	for _, fields := range [][]huh.Field{server, endpoint, workload, databases} {
		if len(fields) > 0 {
			groups = append(groups, huh.NewGroup(fields...))
		}
	}
	// The ACME email is asked after the TLS mode is known, and only for acme.
	if !supplied[qACMEEmail] {
		groups = append(groups, huh.NewGroup(
			huh.NewInput().Title("ACME email (certificate expiry notices)").Value(&a.ACMEEmail),
		).WithHideFunc(func() bool { return a.TLSMode != "acme" }))
	}
	if len(groups) == 0 {
		return nil
	}
	if err := huh.NewForm(groups...).Run(); err != nil {
		return err
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
	cfg.Network.ClientAllowlist = splitList(a.Allowlist)
	if names := splitList(a.Databases); len(names) > 0 {
		cfg.Databases = databasesFromNames(names, cfg.Databases)
	}
	return nil
}

// databaseNames returns the configured database names, defaulting to app.
func databaseNames(cfg config.Cluster) []string {
	if len(cfg.Databases) == 0 {
		return []string{"app"}
	}
	out := make([]string, 0, len(cfg.Databases))
	for _, db := range cfg.Databases {
		out = append(out, db.Name)
	}
	return out
}

// databasesFromNames builds [[databases]] entries, keeping an existing entry's
// statement timeout and extensions when the name is unchanged.
func databasesFromNames(names []string, existing []config.Database) []config.Database {
	byName := map[string]config.Database{}
	for _, db := range existing {
		byName[db.Name] = db
	}
	out := make([]config.Database, 0, len(names))
	for _, name := range names {
		if db, ok := byName[name]; ok {
			out = append(out, db)
			continue
		}
		out = append(out, config.Database{Name: name, StatementTimeout: "60s", Extensions: []string{"pg_stat_statements"}})
	}
	return out
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
