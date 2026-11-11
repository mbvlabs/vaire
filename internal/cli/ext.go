package cli

import (
	"fmt"
	"slices"
	"strings"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/node"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/spf13/cobra"
)

// ext (Phase 7): list, add and remove one database's extensions. The node
// validates the target, descriptor, tokens and protection; this side only
// canonicalises names, asks for the tokens it can predict, and renders the
// result.

type extAddResult struct {
	Database              string   `json:"database"`
	Extension             string   `json:"extension"`
	ExtName               string   `json:"ext_name,omitempty"`
	Source                string   `json:"source,omitempty"`
	Package               string   `json:"package,omitempty"`
	Preload               string   `json:"preload,omitempty"`
	Schema                string   `json:"schema,omitempty"`
	CopySafety            string   `json:"copy_safety,omitempty"`
	StandbySafe           bool     `json:"standby_safe,omitempty"`
	DescriptorSaved       bool     `json:"descriptor_saved,omitempty"`
	AlreadyEnabled        bool     `json:"already_enabled,omitempty"`
	Created               bool     `json:"created,omitempty"`
	RestartRequired       bool     `json:"restart_required"`
	Restarted             bool     `json:"restarted,omitempty"`
	PendingRestart        bool     `json:"pending_restart,omitempty"`
	PreloadMissing        bool     `json:"preload_missing,omitempty"`
	Version               string   `json:"version,omitempty"`
	PreviousVersion       string   `json:"previous_version,omitempty"`
	DefaultVersion        string   `json:"default_version,omitempty"`
	UpdateAvailable       bool     `json:"update_available,omitempty"`
	Downgrade             bool     `json:"downgrade,omitempty"`
	DirectionUnknown      bool     `json:"direction_unknown,omitempty"`
	Updated               bool     `json:"updated,omitempty"`
	PackageKept           string   `json:"package_kept,omitempty"`
	RequiredConfirmTokens []string `json:"required_confirm_tokens,omitempty"`
	DryRun                bool     `json:"dry_run,omitempty"`
	SQL                   string   `json:"sql,omitempty"`
}

type extRemoveResult struct {
	Database              string   `json:"database"`
	Extension             string   `json:"extension"`
	ExtName               string   `json:"ext_name,omitempty"`
	Enabled               bool     `json:"enabled"`
	Installed             bool     `json:"installed"`
	Dependents            []string `json:"dependents,omitempty"`
	DependentsTotal       int      `json:"dependents_total,omitempty"`
	DependentsTruncated   bool     `json:"dependents_truncated,omitempty"`
	OwnTables             []string `json:"own_tables,omitempty"`
	OwnTablesTotal        int      `json:"own_tables_total,omitempty"`
	OwnTablesTruncated    bool     `json:"own_tables_truncated,omitempty"`
	Cascade               bool     `json:"cascade,omitempty"`
	RestartRequired       bool     `json:"restart_required"`
	Restarted             bool     `json:"restarted,omitempty"`
	PackageKept           string   `json:"package_kept,omitempty"`
	RequiredConfirmTokens []string `json:"required_confirm_tokens,omitempty"`
	DryRun                bool     `json:"dry_run,omitempty"`
}

type extListEntry struct {
	Name             string             `json:"name"`
	ExtName          string             `json:"ext_name,omitempty"`
	Aliases          []string           `json:"aliases,omitempty"`
	Source           string             `json:"source"`
	Package          string             `json:"package,omitempty"`
	Preload          string             `json:"preload,omitempty"`
	Restart          bool               `json:"restart"`
	Default          bool               `json:"default,omitempty"`
	Note             string             `json:"note,omitempty"`
	BuiltIn          bool               `json:"built_in,omitempty"`
	CopySafety       string             `json:"copy_safety,omitempty"`
	StandbySafe      bool               `json:"standby_safe,omitempty"`
	PackageInstalled bool               `json:"package_installed"`
	PackageKnown     bool               `json:"package_known"`
	PackageError     string             `json:"package_error,omitempty"`
	Available        bool               `json:"available"`
	AvailableKnown   bool               `json:"available_known"`
	AvailableError   string             `json:"available_error,omitempty"`
	DefaultVersion   string             `json:"default_version,omitempty"`
	Unused           bool               `json:"unused,omitempty"`
	Databases        []extDatabaseState `json:"databases"`
}

type extDatabaseState struct {
	DB              string `json:"db"`
	Available       bool   `json:"available"`
	State           string `json:"state,omitempty"`
	Error           string `json:"error,omitempty"`
	Enabled         bool   `json:"enabled"`
	Installed       bool   `json:"installed"`
	Version         string `json:"version,omitempty"`
	DefaultVersion  string `json:"default_version,omitempty"`
	UpdateAvailable bool   `json:"update_available,omitempty"`
	PendingRestart  bool   `json:"pending_restart,omitempty"`
	PreloadKnown    bool   `json:"preload_known"`
}

type extListDatabase struct {
	DB           string                  `json:"db"`
	State        string                  `json:"state"`
	Available    bool                    `json:"available"`
	Error        string                  `json:"error,omitempty"`
	PreloadKnown bool                    `json:"preload_known"`
	Unmanaged    []extInstalledExtension `json:"unmanaged,omitempty"`
}

type extInstalledExtension struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Default string `json:"default_version,omitempty"`
}

type extListData struct {
	Extensions []extListEntry    `json:"extensions"`
	Databases  []extListDatabase `json:"databases"`
}

func runExtList(cmd *cobra.Command, _ []string) error {
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	params := map[string]any{}
	if db := stringFlag(cmd, "db"); db != "" {
		params["db"] = db
	}
	reply, err := p.call(commandContext(cmd), s.node, "ext.list", params, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return err
	}
	var data extListData
	if err := reply.Decode(&data); err != nil {
		return err
	}
	return output.Success(cmd, output.Result{
		Data: data, Message: strings.Join(extListLines(data), "\n"),
		Warnings: append(s.warnings, reply.Warnings...),
	})
}

func runExtAdd(cmd *cobra.Command, args []string) error {
	name := strings.TrimSpace(args[0])
	if name == "" {
		return output.NewError(output.CodeUsage, "ext add needs an extension name", output.ExitUsage, "")
	}
	settings, err := extFlagMap(cmd, "setting")
	if err != nil {
		return err
	}
	copySettings, err := extFlagMap(cmd, "copy-setting")
	if err != nil {
		return err
	}
	params := map[string]any{"name": name}
	if db := stringFlag(cmd, "db"); db != "" {
		params["db"] = db
	}
	if pkg := stringFlag(cmd, "package"); pkg != "" {
		params["package"] = pkg
	}
	if flagTrue(cmd, "preinstalled") {
		params["preinstalled"] = true
	}
	if preload := stringFlag(cmd, "preload"); preload != "" {
		params["preload"] = preload
	}
	if len(settings) > 0 {
		params["settings"] = settings
	}
	if schema := stringFlag(cmd, "schema"); schema != "" {
		params["schema"] = schema
	}
	if copySafety := stringFlag(cmd, "copy-safety"); copySafety != "" {
		params["copy_safety"] = copySafety
	}
	if len(copySettings) > 0 {
		params["copy_settings"] = copySettings
	}
	if value, supplied := extStandbySafeParam(cmd); supplied {
		params["standby_safe"] = value
	}
	if flagTrue(cmd, "preinstalled") && stringFlag(cmd, "package") != "" {
		return output.NewError(output.CodeUsage, "--package and --preinstalled are mutually exclusive", output.ExitUsage, "")
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
	db, err := extDBForAdd(cfg, stringFlag(cmd, "db"))
	if err != nil {
		return err
	}
	params["db"] = db
	if err := node.RefuseFollowerMethod(cfg, "ext.add", map[string]any{"db": db}); err != nil {
		return err
	}
	tokens := confirmTokens(cmd)
	if !dryRun(cmd) && extAddLikelyNeedsRestart(cfg, db, name, stringFlag(cmd, "preload"), params) {
		if tokens, err = ensureConfirmToken(cmd, tokens, db+"/restart"); err != nil {
			return err
		}
	}
	confirm, confirms := splitConfirmTokens(tokens)
	reply, err := p.call(commandContext(cmd), s.node, "ext.add", params,
		remote.CallOptions{DryRun: dryRun(cmd), Confirm: confirm, Confirms: confirms, OnEvent: p.event})
	if err != nil {
		return err
	}
	var res extAddResult
	if err := reply.Decode(&res); err != nil {
		return err
	}
	warnings := append(s.warnings, reply.Warnings...)
	if dryRun(cmd) {
		if res.AlreadyEnabled && !res.DescriptorSaved && !res.UpdateAvailable && !res.Downgrade && !res.RestartRequired && !res.PreloadMissing && res.Version != "" {
			return output.Success(cmd, output.Result{
				Data: res, Message: "dry run: " + res.Extension + " " + res.Version + " is already enabled in " + res.Database + "; no change",
				Warnings: warnings,
			})
		}
		msg := "dry run: would add " + res.Extension + " to " + res.Database
		if res.Package != "" {
			msg += ", installing " + res.Package
		}
		if res.DescriptorSaved {
			msg += ", and save its [extensions] definition"
		}
		if res.RestartRequired {
			msg += "; its configuration change needs a restart token (--confirm " + res.Database + "/restart)"
		}
		if res.Version != "" {
			msg += "; installed version " + res.Version
		}
		if res.UpdateAvailable {
			msg += "; the package's default is " + res.DefaultVersion + ", so the change would check a supported update path"
		}
		if res.Downgrade {
			msg += "; the package's default " + res.DefaultVersion + " is older than the installed " + res.Version + ", so no downgrade would be performed"
		}
		if res.DirectionUnknown {
			msg += "; vaire cannot establish a non-downgrade ordering from " + res.Version + " to " + res.DefaultVersion + ", so no automatic update would run"
		}
		if res.PreloadMissing {
			msg += "; its preload library is not loaded and no restart is pending"
		}
		if len(res.RequiredConfirmTokens) > 0 {
			msg += "; confirmation: --confirm " + strings.Join(res.RequiredConfirmTokens, " --confirm ")
		}
		if res.SQL != "" {
			msg += "\n" + strings.TrimRight(res.SQL, "\n")
		}
		return output.Success(cmd, output.Result{Data: res, Message: msg, Warnings: warnings})
	}
	msg := extAddMessage(res)
	return output.Success(cmd, output.Result{
		Data: res, Message: msg, Warnings: warnings,
		Next: []string{"vaire ext list --db " + res.Database},
	})
}

func runExtRemove(cmd *cobra.Command, args []string) error {
	name := strings.TrimSpace(args[0])
	if name == "" {
		return output.NewError(output.CodeUsage, "ext remove needs an extension name", output.ExitUsage, "")
	}
	db := stringFlag(cmd, "db")
	if db == "" {
		return output.NewError(output.CodeInputRequired, "ext remove needs --db", output.ExitInput,
			"Pass --db with the database whose extension to remove.").WithDetails(map[string]any{"flag": "--db"})
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
	if _, ok := cfg.ClusterByName(db); !ok {
		return output.NewError(output.CodePrecondition, "database "+db+" does not exist", output.ExitPrecondition,
			"Run vaire db list to see the databases.").WithDetails(map[string]any{"reason": "database_not_found", "database": db})
	}
	if err := node.RefuseFollowerMethod(cfg, "ext.remove", map[string]any{"db": db}); err != nil {
		return err
	}
	if err := node.CheckNotProtected(cfg, db, "ext remove"); err != nil {
		return err
	}
	params := map[string]any{"name": name, "db": db}
	if flagTrue(cmd, "cascade") {
		params["cascade"] = true
	}
	tokens := confirmTokens(cmd)
	restart, err := extRemoveLikelyNeedsRestart(cfg, db, name)
	if err != nil {
		return err
	}
	if !dryRun(cmd) {
		if tokens, err = ensureConfirmToken(cmd, tokens, db); err != nil {
			return err
		}
		if restart {
			if tokens, err = ensureConfirmToken(cmd, tokens, db+"/restart"); err != nil {
				return err
			}
		}
	}
	confirm, confirms := splitConfirmTokens(tokens)
	reply, err := p.call(commandContext(cmd), s.node, "ext.remove", params,
		remote.CallOptions{DryRun: dryRun(cmd), Confirm: confirm, Confirms: confirms, OnEvent: p.event})
	if err != nil {
		return err
	}
	var res extRemoveResult
	if err := reply.Decode(&res); err != nil {
		return err
	}
	warnings := append(s.warnings, reply.Warnings...)
	if dryRun(cmd) {
		lines := []string{"dry run: would remove " + res.Extension + " from " + res.Database}
		if res.DependentsTotal > 0 {
			lines = append(lines, "  used by "+extCountText(res.DependentsTotal, "object")+" (--cascade drops them): "+extPreviewText(res.Dependents, res.DependentsTruncated))
		}
		if res.OwnTablesTotal > 0 {
			lines = append(lines, "  its own tables go with it: "+extPreviewText(res.OwnTables, res.OwnTablesTruncated))
		}
		if res.RestartRequired {
			lines = append(lines, "  its removal changes preload or postmaster settings, so a restart token is required (--confirm "+res.Database+"/restart)")
		}
		if len(res.RequiredConfirmTokens) > 0 {
			lines = append(lines, "  confirmation: --confirm "+strings.Join(res.RequiredConfirmTokens, " --confirm "))
		}
		return output.Success(cmd, output.Result{Data: res, Message: strings.Join(lines, "\n"), Warnings: warnings})
	}
	return output.Success(cmd, output.Result{
		Data: res, Message: extRemoveMessage(res), Warnings: warnings,
		Next: []string{"vaire ext list --db " + res.Database},
	})
}

func extDBForAdd(cfg config.Cluster, db string) (string, error) {
	if db != "" {
		if _, ok := cfg.ClusterByName(db); !ok {
			return "", output.NewError(output.CodePrecondition, "database "+db+" does not exist", output.ExitPrecondition,
				"Run vaire db list to see the databases.").WithDetails(map[string]any{"reason": "database_not_found", "database": db})
		}
		return db, nil
	}
	var candidates []string
	for _, spec := range cfg.Clusters {
		if spec.IsFollower() || spec.FollowGeneration != "" {
			continue
		}
		candidates = append(candidates, spec.Name)
	}
	switch len(candidates) {
	case 0:
		return "", output.NewError(output.CodePrecondition, "there is no database that can take an extension change", output.ExitPrecondition,
			"Create an independent database first: vaire db create <name>.").WithDetails(map[string]any{"reason": "database_not_found"})
	case 1:
		return candidates[0], nil
	}
	return "", output.NewError(output.CodeInputRequired,
		fmt.Sprintf("ext add needs --db: the server has %d databases", len(candidates)), output.ExitInput,
		"Pass --db with one of "+strings.Join(candidates, ", ")+".").
		WithDetails(map[string]any{"flag": "--db", "databases": candidates})
}

func extAddLikelyNeedsRestart(cfg config.Cluster, db, name, directPreload string, params map[string]any) bool {
	if directPreload != "" {
		return true
	}
	if settings, _ := params["settings"].(map[string]string); len(settings) > 0 {
		return true
	}
	spec, ok := cfg.ClusterByName(db)
	if !ok {
		return false
	}
	already := extensionEnabledCLI(cfg, spec, name)
	ext, err := cfg.ExtensionResolver().Resolve(name)
	if err != nil {
		return false
	}
	direct := extHasDirectFlags(params)
	if ext.Preload != "" {
		return !already || !ext.Default || direct
	}
	return (!already || direct) && len(ext.Settings) > 0
}

func extHasDirectFlags(params map[string]any) bool {
	for _, key := range []string{"package", "preinstalled", "preload", "settings", "schema", "copy_safety", "copy_settings", "standby_safe"} {
		if _, ok := params[key]; ok {
			return true
		}
	}
	return false
}

func extensionEnabledCLI(cfg config.Cluster, spec config.ClusterSpec, name string) bool {
	canonical, _ := cfg.ExtensionResolver().Canonical(name)
	effective, err := cfg.ExtensionResolver().EffectiveNames(spec.Extensions)
	if err != nil {
		return false
	}
	return slices.Contains(effective, canonical)
}

func extRemoveLikelyNeedsRestart(cfg config.Cluster, db, name string) (bool, error) {
	spec, ok := cfg.ClusterByName(db)
	if !ok {
		return false, nil
	}
	if ext, err := cfg.ExtensionResolver().Resolve(name); err == nil && extensionEnabledCLI(cfg, spec, name) && len(ext.Settings) > 0 {
		return true, nil
	}
	before, err := cfg.ExtensionResolver().Preloads(spec.Extensions)
	if err != nil {
		return false, nil
	}
	canonical, _ := cfg.ExtensionResolver().Canonical(name)
	var afterNames []string
	for _, n := range spec.Extensions {
		c, _ := cfg.ExtensionResolver().Canonical(n)
		if c != canonical {
			afterNames = append(afterNames, n)
		}
	}
	after, err := cfg.ExtensionResolver().Preloads(afterNames)
	if err != nil {
		return false, nil
	}
	return !slices.Equal(before, after), nil
}

// extStandbySafeParam reports the flag value only when the operator supplied
// it. An explicit --standby-safe=false is a supplied safety declaration and
// must not collapse into "absent".
func extStandbySafeParam(cmd *cobra.Command) (bool, bool) {
	flag := cmd.Flags().Lookup("standby-safe")
	if flag == nil || !flag.Changed {
		return false, false
	}
	return flagTrue(cmd, "standby-safe"), true
}

func extFlagMap(cmd *cobra.Command, name string) (map[string]string, error) {
	values, err := cmd.Flags().GetStringArray(name)
	if err != nil || len(values) == 0 {
		return nil, nil //nolint:nilerr // a missing flag is not an error
	}
	out := map[string]string{}
	for _, value := range values {
		key, val, ok := strings.Cut(value, "=")
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if !ok || key == "" {
			return nil, output.NewError(output.CodeUsage,
				"--"+name+" needs key=value", output.ExitUsage, "For example --"+name+" work_mem=64MB.")
		}
		if previous, exists := out[key]; exists && previous != val {
			return nil, output.NewError(output.CodeUsage,
				"--"+name+" sets "+key+" twice with different values", output.ExitUsage, "")
		}
		out[key] = val
	}
	return out, nil
}

func ensureConfirmToken(cmd *cobra.Command, tokens []string, token string) ([]string, error) {
	if slices.Contains(tokens, token) {
		return tokens, nil
	}
	if err := promptConfirm(cmd, token); err != nil {
		return nil, err
	}
	return append(tokens, token), nil
}

func splitConfirmTokens(tokens []string) (string, []string) {
	if len(tokens) == 0 {
		return "", nil
	}
	return tokens[0], tokens[1:]
}

func extAddMessage(res extAddResult) string {
	switch {
	case res.Updated:
		msg := "updated " + res.Extension + " in " + res.Database + " from " + res.PreviousVersion + " to " + res.Version
		if res.Restarted {
			msg += "; restarted " + res.Database + " (only that database)"
		}
		return msg + extAddDescriptorNote(res) + extAddPreloadNote(res)
	case res.Created:
		msg := "created " + res.Extension
		if res.Version != "" {
			msg += " " + res.Version
		}
		msg += " in " + res.Database
		if res.Package != "" {
			msg += "; the package " + res.Package + " stays installed"
		}
		if res.Restarted {
			msg += "; restarted " + res.Database + " (only that database)"
		}
		return msg + extAddDescriptorNote(res) + extAddPreloadNote(res)
	case res.AlreadyEnabled && !res.DescriptorSaved:
		msg := res.Extension + " " + res.Version + " is in " + res.Database
		if res.PendingRestart {
			msg += "; it waits for a restart"
		}
		return msg + extAddPreloadNote(res)
	default:
		msg := "added " + res.Extension
		if res.Version != "" {
			msg += " " + res.Version
		}
		msg += " to " + res.Database
		if res.Package != "" {
			msg += "; the package " + res.Package + " stays installed"
		}
		if res.Restarted {
			msg += "; restarted " + res.Database + " (only that database)"
		}
		return msg + extAddDescriptorNote(res) + extAddPreloadNote(res)
	}
}

func extAddDescriptorNote(res extAddResult) string {
	if res.DescriptorSaved {
		return "; saved its [extensions] definition"
	}
	return ""
}

func extAddPreloadNote(res extAddResult) string {
	if res.PreloadMissing {
		return "; its preload library is not loaded and no restart is pending"
	}
	return ""
}

func extRemoveMessage(res extRemoveResult) string {
	msg := "removed " + res.Extension + " from " + res.Database
	if res.Cascade && len(res.Dependents) > 0 {
		msg += " with " + extCountText(res.DependentsTotal, "dependent object")
	}
	if res.Restarted {
		msg += "; restarted " + res.Database + " (only that database)"
	}
	if res.PackageKept != "" {
		msg += "; the package " + res.PackageKept + " stays installed"
	}
	return msg
}

func extListLines(data extListData) []string {
	const row = "  %-18s %-30s %-10s %-10s %-8s %-9s %-8s %s"
	lines := []string{fmt.Sprintf(row, "NAME", "PACKAGE", "SOURCE", "PRELOAD", "RESTART", "VERSION", "STATUS", "DATABASES")}
	for _, e := range data.Extensions {
		pkg := "preinstalled"
		if e.Package != "" {
			pkg = e.Package
			switch {
			case !e.PackageKnown:
				pkg += " (unknown)"
			case !e.PackageInstalled:
				pkg += " (missing)"
			case e.Unused:
				pkg += " (unused)"
			default:
				pkg += " (installed)"
			}
		}
		preload := "-"
		if e.Preload != "" {
			preload = e.Preload
		}
		restart := "-"
		if e.Restart {
			restart = "yes"
		}
		version := e.DefaultVersion
		if version == "" {
			version = "-"
		}
		status := "ok"
		switch {
		case !e.AvailableKnown:
			status = "unknown"
		case !e.Available:
			status = "missing"
		}
		var dbs []string
		for _, d := range e.Databases {
			label := d.DB
			switch {
			case !d.Available:
				label += " (enabled, unavailable: " + d.State + ")"
			case d.PendingRestart:
				label += " (waits for restart)"
			case e.ExtName == "" && d.Enabled:
				label += " (preload-only)"
			case d.Enabled && !d.Installed && e.ExtName != "":
				label += " (missing)"
			case !d.Enabled && d.Installed:
				label += " (" + d.Version + ", not enabled)"
			case d.UpdateAvailable:
				label += " (" + d.Version + ", update to " + d.DefaultVersion + ")"
			case d.Version != "":
				label += " (" + d.Version + ")"
			}
			dbs = append(dbs, label)
		}
		where := "-"
		if len(dbs) > 0 {
			where = strings.Join(dbs, ", ")
		}
		lines = append(lines, fmt.Sprintf(row, e.Name, pkg, e.Source, preload, restart, version, status, where))
	}
	for _, d := range data.Databases {
		switch {
		case !d.Available:
			lines = append(lines, "  "+d.DB+": "+d.State+", installed extensions are unavailable"+
				extErrorSuffix(d.Error))
		case !d.PreloadKnown:
			lines = append(lines, "  "+d.DB+": preload state is unavailable")
		}
		for _, u := range d.Unmanaged {
			lines = append(lines, "  "+d.DB+" also has "+u.Name+" "+u.Version+" (unmanaged)")
		}
	}
	return lines
}

func extErrorSuffix(err string) string {
	if err == "" {
		return ""
	}
	return " (" + err + ")"
}

func extPreviewText(items []string, truncated bool) string {
	if len(items) == 0 {
		return "unknown objects"
	}
	shown := items
	if len(shown) > 3 {
		shown = shown[:3]
	}
	text := strings.Join(shown, "; ")
	if len(items) > len(shown) || truncated {
		text += "; ..."
	}
	return text
}

func extCountText(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
