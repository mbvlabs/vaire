package steps

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/masterbackup"
	"github.com/mbvlabs/vaire/internal/render"
	"github.com/mbvlabs/vaire/internal/secrets"
	"github.com/mbvlabs/vaire/internal/state"
)

const pgbouncerID = "pgbouncer-v1"

type pgbouncer struct{}

func (pgbouncer) ID() string { return pgbouncerID }

func (pgbouncer) Describe(config.Cluster) string {
	return "render both poolers' configuration with an entry per database, the session pooler's unit and userlist.txt, then reload PgBouncer"
}

// SkipReason skips the step while there is no database: the service roles
// whose verifiers fill userlist.txt live in each database's cluster.
func (pgbouncer) SkipReason(cfg config.Cluster) string {
	if len(cfg.Clusters) == 0 {
		recs, err := poolerRecords()
		if err != nil {
			return "" // Inputs reports unknown state rather than silently skipping.
		}

		if _, _, ok := recoverInspectionSource(cfg, recs); ok {
			return ""
		}

		return "no_databases"
	}

	return ""
}

// pgbouncerIni is the transaction pooler's rendered ini, which never
// contains a secret.
func pgbouncerIni(cfg config.Cluster, rt engine.Runtime, dbs []render.PoolerDatabase) string {
	return string(render.PgbouncerIni(cfg, rt.Host, dbs))
}

// poolerDatabases is one [databases] entry per cluster, routed by name to its
// socket and capped at its pool for that pooler: pool_size for the
// transaction pooler, session_pool_size for the session pooler (RFC 8.1).
// An unfinished unfollow's alias is left out on both poolers until its
// privileges are settled: a reload or a pooler restart then cannot expose a
// promoted database whose renames or protection policy are still in flight.
// Both poolers are rendered from one records snapshot so they cannot disagree
// about which aliases the transition withholds.
func poolerDatabases(cfg config.Cluster, recs backup.Records, session bool) []render.PoolerDatabase {
	fenced := fencedPoolerAliases(recs)

	out := make([]render.PoolerDatabase, 0, len(cfg.Clusters))
	for _, c := range cfg.Clusters {
		if fenced[c.Name] || recs.IsAwaiting(c.Name) {
			continue
		}

		db := render.PoolerDatabase{Name: c.Name, Port: c.Port}
		if c.IsFollower() {
			// The follower's cluster holds its source's database (Phase 6).
			db.DBName = c.Follows
		}

		if r, err := cfg.Resources(c); err == nil {
			db.MaxConnections = r.PoolSize
			if session {
				db.MaxConnections = r.SessionPoolSize
			}
		}

		out = append(out, db)
	}

	if session {
		out = append(out, restoreEntries(cfg, recs)...)
	}

	return out
}

// fencedPoolerAliases are the databases whose unfollow has not settled its
// privileges yet. The record is durable, so a render or restart after an
// interruption keeps withholding them.
func fencedPoolerAliases(recs backup.Records) map[string]bool {
	var out map[string]bool

	for name, u := range recs.Unfollowing {
		if u.Settled {
			continue
		}

		if out == nil {
			out = map[string]bool{}
		}

		out[name] = true
	}
	// A recovered target is not an application target until access has been
	// installed and the owned restore record has been retired. This also
	// fences older deferred recoveries that cleared AwaitingRecovery early.
	for name, r := range recs.Restores {
		if !r.Recover {
			continue
		}

		if out == nil {
			out = map[string]bool{}
		}

		out[name] = true
	}

	for name, fence := range recs.Activation {
		if fence.Active() {
			if out == nil {
				out = map[string]bool{}
			}

			out[name] = true
		}
	}

	return out
}

// poolerRecords is one read of records.json for rendering both poolers. A
// missing file is the node's legitimate empty initial state; any other read or
// decode failure is unknown transition state and stops the render rather than
// dropping an unfinished unfollow's fence. The aliases stay withheld until the
// records are readable again.
func poolerRecords() (backup.Records, error) {
	recs, err := backup.LoadRecords()
	if err != nil {
		return backup.Records{}, output.NewError(output.CodePrecondition,
			"the node's unfollow records could not be read, so the poolers' configuration cannot be rendered safely: "+err.Error(),
			output.ExitPrecondition,
			"Inspect the node's records file and fix it; vaire never resets it on its own.").
			WithDetails(map[string]any{"reason": "unfollow_state_unknown"})
	}

	return recs, nil
}

// restoreEntries route each ready restore's <db>__restoring to its
// temporary cluster, where the database is still called <db>, so vaire sql
// --db <db>__restoring reaches the paused copy (RFC 10.1). Only the session
// pooler has them; promote and abort reload it without them.
func restoreEntries(cfg config.Cluster, recs backup.Records) []render.PoolerDatabase {
	var out []render.PoolerDatabase

	for db, r := range recs.Restores {
		_, visible := cfg.ClusterByName(db)
		if !visible && r.Recover && r.ID != "" && r.Database == db && recs.IsAwaiting(db) {
			for _, spec := range cfg.Awaiting {
				if spec.Name == db {
					visible = true
					break
				}
			}
		}

		if !visible || r.Phase != backup.PhaseReady || r.Port == 0 {
			continue
		}
		// A restore whose promotion has begun must not keep an inspection
		// route to the copy that is being swapped or is still unsettled.
		if f, ok := recs.ActivationFenceFor(db); ok && f.Active() {
			continue
		}

		out = append(out, render.PoolerDatabase{Name: backup.RestoringName(db), DBName: db, Port: r.Port, MaxConnections: restoreEntryPool})
	}

	return out
}

func recoverInspectionSource(cfg config.Cluster, recs backup.Records) (config.ClusterSpec, backup.Restore, bool) {
	for _, spec := range cfg.Awaiting {
		r, ok := recs.Restores[spec.Name]
		if ok && recs.IsAwaiting(spec.Name) && r.Recover && r.ID != "" &&
			r.Database == spec.Name && r.Phase == backup.PhaseReady && r.Port != 0 {
			return spec, r, true
		}
	}

	return config.ClusterSpec{}, backup.Restore{}, false
}

// restoreEntryPool caps the session pooler's connections to a restore's copy:
// enough to inspect it.
const restoreEntryPool = 5

// pgbouncerFile is one file the step owns, apart from the userlist.
type pgbouncerFile struct {
	path, content, owner, group string
	mode                        os.FileMode
}

// poolerFilePaths are the managed files pgbouncerFiles renders. Production
// values are the constants in pgcommon.go; they are variables so the
// fail-closed regression can use real files in a temporary directory.
var poolerFilePaths = struct {
	ini, hba, sessionIni, sessionHBA, sessionUnit string
}{
	ini: PgbouncerIniPath, hba: PgbouncerHBAPath, sessionIni: SessionIniPath,
	sessionHBA: SessionHBAPath, sessionUnit: SessionUnitPath,
}

// pgbouncerFiles are both poolers' configuration and the session pooler's
// unit (RFC 8.1). None of them contains a secret. The records are read once,
// before any file is rendered or written: an unfinished unfollow's alias and a
// ready restore are both taken from that one snapshot, and unreadable records
// fail the whole render closed. The normal step does not narrow its config
// here: the engine already handed it the view that keeps an explicitly owned
// follow or recovery target.
func pgbouncerFiles(cfg config.Cluster, rt engine.Runtime) ([]pgbouncerFile, error) {
	recs, err := poolerRecords()
	if err != nil {
		return nil, err
	}

	if err := validateRoutingEligibility(cfg, recs); err != nil {
		return nil, err
	}

	operators, err := poolerOperators(cfg)
	if err != nil {
		return nil, err
	}

	return pgbouncerFilesFrom(cfg, rt, recs, operators), nil
}

var loadPoolerOperators = func() (config.Operators, error) {
	return config.LoadOperators(config.OperatorsPath)
}

func poolerOperators(cfg config.Cluster) ([]string, error) {
	ops, err := loadPoolerOperators()
	if err != nil {
		return nil, err
	}

	if len(ops.Operators) == 0 {
		return nil, nil
	}

	subnet, err := netip.ParsePrefix(cfg.Transport.Subnet)
	if err != nil || !subnet.Addr().Is4() {
		return nil, fmt.Errorf("invalid IPv4 WireGuard subnet for operator peers")
	}

	addresses := make([]string, 0, len(ops.Operators))
	for _, op := range ops.Operators {
		addr, err := netip.ParseAddr(op.WGAddress)
		if err != nil || !addr.Is4() || !subnet.Contains(addr) {
			return nil, fmt.Errorf("operator %q has an invalid IPv4 WireGuard address", op.Name)
		}

		addresses = append(addresses, addr.String())
	}

	return addresses, nil
}

// pgbouncerFilesFrom renders both poolers from one records snapshot.
func pgbouncerFilesFrom(cfg config.Cluster, rt engine.Runtime, recs backup.Records, operators []string) []pgbouncerFile {
	transactionDBs := poolerDatabases(cfg, recs, false)
	sessionDBs := poolerDatabases(cfg, recs, true)

	return []pgbouncerFile{
		{poolerFilePaths.ini, pgbouncerIni(cfg, rt, transactionDBs), pgFileOwner, pgFileGroup, 0o640},
		{poolerFilePaths.hba, string(render.PgbouncerHBA(cfg, transactionDBs, operators)), pgFileOwner, pgFileGroup, 0o640},
		{poolerFilePaths.sessionIni, string(render.SessionIni(cfg, sessionDBs, SessionSocketDir)), pgFileOwner, pgFileGroup, 0o640},
		{poolerFilePaths.sessionHBA, string(render.SessionHBA(cfg, sessionDBs, operators)), pgFileOwner, pgFileGroup, 0o640},
		{poolerFilePaths.sessionUnit, string(render.SessionUnit()), "", "", 0o644},
	}
}

// writePoolerFile writes one of both poolers' managed files. It is a variable
// so the fail-closed regression can prove unreadable records leave the existing
// safe files untouched.
var writePoolerFile = writeFileOwned

func (pgbouncer) RenderedFiles(cfg config.Cluster, rt engine.Runtime) []engine.RenderedFile {
	files, err := pgbouncerFiles(cfg, rt)
	if err != nil {
		// Inputs runs before a plan renders a diff, so an unreadable records
		// file has already failed the plan; there is nothing safe to show.
		return nil
	}

	out := make([]engine.RenderedFile, 0, len(files))
	for _, f := range files {
		out = append(out, engine.RenderedFile{Path: f.path, Content: []byte(f.content)})
	}

	return out
}

func (pgbouncer) Inputs(cfg config.Cluster, rt engine.Runtime) ([]byte, error) {
	// The verifiers change on rotation without changing the desired state, so
	// they are not part of the inputs; Check re-renders them each time.
	files, err := pgbouncerFiles(cfg, rt)
	if err != nil {
		return nil, err
	}

	rendered := map[string]string{}
	for _, f := range files {
		rendered[f.path] = f.content
	}

	return canonical(map[string]any{"files": rendered, "users": ServiceRoles})
}

func (pgbouncer) Check(ctx context.Context, cfg config.Cluster, rt engine.Runtime) (engine.CheckResult, error) {
	files, err := pgbouncerFiles(cfg, rt)
	if err != nil {
		return engine.CheckResult{}, err
	}

	for _, f := range files {
		if !fileIsOwned(f.path, f.content, f.mode, f.owner, f.group) {
			return incomplete(f.path + " differs"), nil
		}
	}

	verifiers, err := roleVerifiers(ctx, cfg)
	if err != nil || len(verifiers) < len(ServiceRoles) {
		return incomplete("cannot read the service role verifiers"), nil
	}

	if !fileIsOwned(PgbouncerUserlistPath, string(render.PgbouncerUserlist(verifiers)), 0o640, pgFileOwner, pgFileGroup) {
		return incomplete(PgbouncerUserlistPath + " differs"), nil
	}

	for _, unit := range []string{PgbouncerService, SessionPoolerService} {
		if !serviceActive(ctx, unit) {
			return incomplete(unit + " is not active"), nil
		}
	}

	if err := pgbouncerResponds(ctx, PGSocketDir, PgbouncerPort); err != nil {
		return incomplete("the transaction pooler does not answer: " + err.Error()), nil
	}

	if err := pgbouncerResponds(ctx, SessionSocketDir, SessionPoolerPort); err != nil {
		return incomplete("the session pooler does not answer: " + err.Error()), nil
	}

	return complete(), nil
}

func (s pgbouncer) Apply(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	var result engine.ApplyResult

	err := poolerExposure(cfg, func() error {
		var err error

		result, err = s.applyGuarded(ctx, cfg, rt, report)

		return err
	})

	return result, err
}

func (s pgbouncer) applyGuarded(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	files, err := pgbouncerFiles(cfg, rt)
	if err != nil {
		return engine.ApplyResult{}, err
	}

	verifiers, err := roleVerifiers(ctx, cfg)
	if err != nil {
		return engine.ApplyResult{}, err
	}

	if len(verifiers) < len(ServiceRoles) {
		return engine.ApplyResult{}, fmt.Errorf("the service role verifiers are missing; run postgres-roles-v1 first")
	}

	if err := writePoolerFiles(files); err != nil {
		return engine.ApplyResult{}, err
	}

	if err := writeFileOwned(PgbouncerUserlistPath, string(render.PgbouncerUserlist(verifiers)), 0o640, pgFileOwner, pgFileGroup); err != nil {
		return engine.ApplyResult{}, err
	}

	err = reloadPoolers(ctx, rt, report)

	return engine.ApplyResult{Restart: engine.RestartNone}, err
}

var poolerExposure = (state.FileStore{}).WithExposure

// writePoolerFiles writes both poolers' rendered configuration. None of them
// contains a secret.
func writePoolerFiles(files []pgbouncerFile) error {
	for _, f := range files {
		if err := writePoolerFile(f.path, f.content, f.mode, f.owner, f.group); err != nil {
			return err
		}
	}

	return nil
}

// reloadPoolers reloads both poolers after their files are written. It is a
// variable so a test can model the single shell call's success or failure
// without a live systemd.
var reloadPoolers = func(ctx context.Context, rt engine.Runtime, report engine.Reporter) error {
	return rt.Shell.Run(ctx, pgbouncerID, Script("pgbouncer"), map[string]string{
		"PGBOUNCER_DIR":   PgbouncerDir,
		"SESSION_SERVICE": SessionPoolerService,
	}, report)
}

// poolerInstance is this install's real instance identity for routing
// eligibility. It is a variable so a test can model a rebuilt node.
var poolerInstance = func() (string, error) { return masterbackup.ReadInstance("") }

// validateRoutingEligibility proves every routing fence before any exposing
// render or reload: the actual instance, current stanza/port/paths,
// registration and recorded transition must all still match. An invalid or
// stale record refuses instead of publishing an alias the poolers cannot
// safely route.
func validateRoutingEligibility(cfg config.Cluster, recs backup.Records) error {
	if !routingFenceActive(recs) {
		return nil
	}

	instance, err := routingInstanceIdentity()
	if err != nil {
		return err
	}

	for db, f := range recs.Activation {
		if f.OperationID == "" || f.Phase != "routing" {
			continue
		}

		if err := validateRoutingFence(cfg, recs, instance, db, f); err != nil {
			return err
		}
	}

	return nil
}

func routingFenceActive(recs backup.Records) bool {
	for _, f := range recs.Activation {
		if f.OperationID != "" && f.Phase == "routing" {
			return true
		}
	}

	return false
}

func routingInstanceIdentity() (string, error) {
	instance, err := poolerInstance()
	if err != nil {
		return "", output.NewError(output.CodePrecondition,
			"the node instance identity could not be read, so pooler routing eligibility is unknown: "+err.Error(),
			output.ExitPrecondition, "Repair the node's instance file deliberately; no pooler file was written.").
			WithDetails(map[string]any{"reason": "routing_instance_unknown"})
	}

	instance = strings.TrimSpace(instance)
	if instance == "" {
		return "", output.NewError(output.CodePrecondition,
			"the node instance identity is missing, so pooler routing eligibility is unknown",
			output.ExitPrecondition, "Repair the node's instance file deliberately; no pooler file was written.").
			WithDetails(map[string]any{"reason": "routing_instance_unknown"})
	}

	return instance, nil
}

func validateRoutingFence(cfg config.Cluster, recs backup.Records, instance, db string, f backup.ActivationFence) error {
	fail := func(detail string) error {
		return output.NewError(output.CodePrecondition,
			db+"'s routing fence is not eligible: "+detail,
			output.ExitPrecondition, "Finish or repair the owning operation; no pooler file was written.").
			WithDetails(map[string]any{"reason": "routing_eligibility", "database": db, "detail": detail})
	}

	spec, ok := cfg.ClusterByName(db)
	if !ok {
		return fail("its database entry is missing")
	}

	if detail := routingFenceTargetGap(spec, f, instance); detail != "" {
		return fail(detail)
	}

	if detail := routingFenceRestoreGap(recs, db, f); detail != "" {
		return fail(detail)
	}

	if detail := routingFencePathGap(cfg, spec, db, f); detail != "" {
		return fail(detail)
	}

	if detail := routingFenceRegistrationGap(db, f); detail != "" {
		return fail(detail)
	}

	return nil
}

func routingFenceTargetGap(spec config.ClusterSpec, f backup.ActivationFence, instance string) string {
	if f.Instance == "" || f.Instance != instance {
		return "its installation instance does not match this node"
	}

	if f.Kind != "fork" && f.Kind != "unfollow" && f.Kind != "restore-promote" && f.Kind != "restore-abort" {
		return "its operation kind is unknown"
	}

	if f.TargetStanza == "" || f.TargetStanza != spec.Stanza {
		return "its target stanza does not match cluster.toml"
	}

	if f.OldStanza == "" || f.NewStanza == "" || f.NewStanza != f.TargetStanza {
		return "its recorded old/new stanza transition is incomplete"
	}

	return ""
}

func routingFenceRestoreGap(recs backup.Records, db string, f backup.ActivationFence) string {
	if f.Kind != "restore-abort" {
		return ""
	}

	if _, ok := recs.Restores[db]; ok {
		return "its database still has a restore record"
	}

	if f.OldStanza != f.TargetStanza || f.Aside == "" {
		return "its abort checkpoint does not describe the untouched live cluster"
	}

	return ""
}

func routingFencePathGap(cfg config.Cluster, spec config.ClusterSpec, db string, f backup.ActivationFence) string {
	if f.Port != spec.Port {
		return "its recorded port does not match cluster.toml"
	}

	if f.DataDir != cfg.Server.DataDirectory(db) || f.ConfDir != PGConfDir(db) {
		return "its recorded data/configuration directory does not match the database"
	}

	return ""
}

func routingFenceRegistrationGap(db string, f backup.ActivationFence) string {
	name, rest, ok := strings.Cut(f.Registration, ":")

	portText, _, _ := strings.Cut(rest, ":")
	if !ok || name != db || portText != strconv.Itoa(f.Port) {
		return "its recorded registration does not match the database"
	}

	if !backup.ClusterRegistered(db) {
		return "no postgresql-common registration exists"
	}

	data, err := backup.ClusterDataDirectory(db)
	if err != nil {
		return "its registration cannot be read: " + err.Error()
	}

	if data == "" || data != f.DataDir {
		return "its registration points at a different data directory"
	}

	return ""
}

// ApplyPoolerFence writes both poolers' rendered configuration and reloads
// them, without touching userlist.txt: db unfollow uses it to withhold an
// unsettled alias durably, and the service roles (and their verifiers) do not
// change. Unlike the pgbouncer-v1 step it always reloads, so a retry after a
// crash between the file writes and the reload still completes the cut.
//
// It is a full-configuration entry point, so it applies the routing lifecycle
// eligibility itself: a database awaiting recovery or an unfinished ordinary
// follow has no routable cluster and is withheld exactly as general apply would
// withhold it. Both the eligibility and the unsettled-unfollow aliases come
// from one records snapshot. The normal pgbouncer-v1 step must not use this
// narrowing: the engine already hands it a view that keeps an explicitly owned
// follow or recovery target. Unreadable records fail before any file is
// overwritten or a service is reloaded, so a retry never re-exposes an alias it
// cannot prove is safe.
func ApplyPoolerFence(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) error {
	return poolerExposure(cfg, func() error {
		return applyPoolerFenceGuarded(ctx, cfg, rt, report)
	})
}

func applyPoolerFenceGuarded(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) error {
	recs, err := poolerRecords()
	if err != nil {
		return err
	}

	if err := validateRoutingEligibility(cfg, recs); err != nil {
		return err
	}

	operators, err := poolerOperators(cfg)
	if err != nil {
		return err
	}

	files := pgbouncerFilesFrom(cfg.RoutingView(recs.IsAwaiting, ""), rt, recs, operators)
	if err := writePoolerFiles(files); err != nil {
		return err
	}

	return reloadPoolers(ctx, rt, report)
}

// PublishFollowerRoutes renders both poolers' routes for target while its
// recovery still awaits. The caller invokes it after the follower started and
// clears the awaiting record only after it succeeds, so a write or reload
// failure leaves that exact recovery retryable. Only target's application and
// __auth routes are published: every other database awaiting recovery, every
// ready-but-unpromoted restore and every unsettled unfollow stays withheld.
// The records snapshot is read-only; a failure changes no durable transition.
func PublishFollowerRoutes(ctx context.Context, cfg config.Cluster, rt engine.Runtime, target string, report engine.Reporter) error {
	return poolerExposure(cfg, func() error {
		return publishFollowerRoutesGuarded(ctx, cfg, rt, target, report)
	})
}

func publishFollowerRoutesGuarded(ctx context.Context, cfg config.Cluster, rt engine.Runtime, target string, report engine.Reporter) error {
	recs, err := poolerRecords()
	if err != nil {
		return err
	}

	if !recs.IsAwaiting(target) {
		return output.NewError(output.CodePrecondition,
			target+" no longer awaits recovery, so its follower routes cannot be published as a recovery", output.ExitPrecondition,
			"Run vaire apply to publish its routes.").
			WithDetails(map[string]any{"reason": "not_awaiting_recovery", "database": target})
	}

	// Treat exactly this target as published; every other transition record
	// still withholds its own routes.
	delete(recs.AwaitingRecovery, target)

	if fencedPoolerAliases(recs)[target] {
		return output.NewError(output.CodePrecondition,
			target+" is still fenced by an unfinished transition, so its follower routes cannot be published", output.ExitPrecondition,
			"Finish the owning transition, then retry db recover "+target+".").
			WithDetails(map[string]any{"reason": "routing_fenced", "database": target})
	}

	view := cfg.RoutingView(recs.IsAwaiting, target)
	if _, ok := view.ClusterByName(target); !ok {
		return output.NewError(output.CodePrecondition,
			target+" is not a routable cluster, so its follower routes cannot be published", output.ExitPrecondition,
			"Check cluster.toml's entry for "+target+", then retry.").
			WithDetails(map[string]any{"reason": "not_routable", "database": target})
	}

	if err := validateRoutingEligibility(cfg, recs); err != nil {
		return err
	}

	operators, err := poolerOperators(cfg)
	if err != nil {
		return err
	}

	files := pgbouncerFilesFrom(view, rt, recs, operators)
	if err := writePoolerFiles(files); err != nil {
		return err
	}

	return reloadPoolers(ctx, rt, report)
}

// roleVerifierSource names the cluster and port the service roles are read
// from. A rebuilt master's await-recovery view has no ordinary cluster; the
// ready copy is then the only source, and a first recovery's kept target uses
// its temporary port. The target alias itself is never the source before the
// copy is promoted.
func roleVerifierSource(cfg config.Cluster, recs backup.Records) (config.ClusterSpec, int, error) {
	if len(cfg.Clusters) != 0 {
		spec := cfg.Clusters[0]
		if !recs.IsAwaiting(spec.Name) {
			return spec, spec.Port, nil
		}

		r, ok := recs.Restores[spec.Name]
		if !ok || !r.Recover || r.ID == "" || r.Database != spec.Name || r.Phase != backup.PhaseReady || r.Port == 0 {
			return spec, 0, fmt.Errorf("service role source for %s is not ready for inspection", spec.Name)
		}

		return spec, r.Port, nil
	}

	if ready, r, ok := recoverInspectionSource(cfg, recs); ok {
		return ready, r.Port, nil
	}

	return config.ClusterSpec{}, 0, fmt.Errorf("there is no cluster to read the service roles from")
}

// roleVerifiers reads the SCRAM verifiers of the service roles from the
// first cluster, or from the ready copy while every database awaits recovery.
// They are the same in every cluster (RFC 12).
func roleVerifiers(ctx context.Context, cfg config.Cluster) (map[string]string, error) {
	recs, err := poolerRecords()
	if err != nil {
		return nil, err
	}

	spec, port, err := roleVerifierSource(cfg, recs)
	if err != nil {
		return nil, err
	}

	local := ClusterPG(spec)
	local.Port = port

	rows, err := local.Query(ctx, "postgres",
		"SELECT rolname, rolpassword FROM pg_authid WHERE rolname IN ('pgbouncer_auth','vaire_monitor')", nil)
	if err != nil {
		return nil, err
	}

	out := make(map[string]string, len(rows))
	for _, row := range rows {
		verifier := row["rolpassword"]
		if verifier == "" {
			continue
		}

		secrets.Add(verifier)
		out[row["rolname"]] = verifier
	}

	return out, nil
}

// pgbouncerResponds proves a pooler's admin console answers on its socket,
// using the monitor password the node keeps for the exporters.
// PoolerRoute is one alias a running pooler serves: its backend host, port and
// database mapping and whether a transition PAUSE still holds it.
type PoolerRoute struct {
	Name     string
	Host     string
	Port     int
	Database string
	Paused   bool
}

// PoolerRoutes reads the actual [databases] routes a running pooler serves
// from SHOW DATABASES. The backend mapping and pause state are verified, not
// only the rendered file. A pooler that does not answer returns its error.
func PoolerRoutes(ctx context.Context, dir string, port int) (map[string]PoolerRoute, error) {
	rows, err := PoolerAdminPG(dir, port).Query(ctx, "pgbouncer", "SHOW DATABASES;", nil)
	if err != nil {
		return nil, err
	}

	out := make(map[string]PoolerRoute, len(rows))
	for _, row := range rows {
		name := row["name"]
		if name == "" {
			continue
		}

		backendPort, _ := strconv.Atoi(row["port"])
		out[name] = PoolerRoute{
			Name: name, Host: row["host"], Port: backendPort,
			Database: row["database"], Paused: row["paused"] == "1" || row["paused"] == "true",
		}
	}

	return out, nil
}

func pgbouncerResponds(ctx context.Context, socketDir string, port int) error {
	password, err := os.ReadFile(RuntimeDir + "/monitor.pass")
	if err != nil {
		return err
	}

	f, err := os.CreateTemp("", "vaire-pgpass-*")
	if err != nil {
		return err
	}

	name := f.Name()
	defer func() { _ = os.Remove(name) }()

	if _, err := fmt.Fprintf(f, "*:*:*:vaire_monitor:%s\n", strings.TrimSpace(string(password))); err != nil {
		_ = f.Close()
		return err
	}

	if err := f.Close(); err != nil {
		return err
	}

	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}

	_, err = ProbeWithEnv(ctx, []string{"PGPASSFILE=" + name}, "psql",
		"-h", socketDir, "-p", strconv.Itoa(port), "-U", "vaire_monitor", "-d", "pgbouncer", "-tAc", "SHOW VERSION")

	return err
}

// poolerAdminTimeout bounds one admin console command. PAUSE waits for the
// database's running queries to finish, so it gets the longest.
const poolerAdminTimeout = 15 * time.Second

// poolerAdmin runs one admin console command on the pooler. It logs in as
// pgbouncer over the socket as the postgres OS user, which PgBouncer lets in
// without a password because it runs as that user.
func poolerAdmin(ctx context.Context, command string) error {
	ctx, cancel := context.WithTimeout(ctx, poolerAdminTimeout)
	defer cancel()

	return runCmd(ctx, "runuser", "-u", "postgres", "--", "psql", "-X", "-q",
		"-h", PGSocketDir, "-p", strconv.Itoa(PgbouncerPort), "-U", "pgbouncer", "-d", "pgbouncer", "-c", command)
}
