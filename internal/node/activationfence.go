package node

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/steps"
)

// Durable activation fences (Phase 7). A restore, unfollow or fork/adoption
// writes one before its first irreversible step and before publishing an
// ordinary desired entry. Ordinary apply, SQL and pooler rendering refuse an
// active fence; only the owning operation may continue it.
//
// Phases are precise checkpoints, not a loose progress label:
//
//	prepared  identity/requirements fixed; credential-bearing work may remain
//	adopting  a fork's credentials/rename/copied jobs are done, before Adopt
//	adopted   a fork's actual registration is verified; configuration/start,
//	          settings and SQL may remain
//	swapped   a restore's data directories were exchanged
//	promoted  a restore or unfollow left recovery; safety settlement may remain
//	routing   every safety settlement is durable; only verified route
//	          publication and pause release remain
//
// Unknown or malformed phases fail closed.

const (
	fencePhasePrepared = "prepared"
	fencePhaseAdopting = "adopting"
	fencePhaseAdopted  = "adopted"
	fencePhaseSwapped  = "swapped"
	fencePhasePromoted = "promoted"
	fencePhaseRouting  = "routing"
)

func validFenceKind(kind string) bool {
	switch kind {
	case "fork", "unfollow", "restore-promote", "restore-abort":
		return true
	}
	return false
}

func validFencePhase(phase string) bool {
	switch phase {
	case fencePhasePrepared, fencePhaseAdopting, fencePhaseAdopted,
		fencePhaseSwapped, fencePhasePromoted, fencePhaseRouting:
		return true
	}
	return false
}

// validFencePhaseForKind enforces the only phase a restore-abort fence may
// hold. It is created by the atomic abort conversion at routing; it never
// advances or returns to an earlier checkpoint.
func validFencePhaseForKind(kind, phase string) bool {
	if kind == "restore-abort" {
		return phase == fencePhaseRouting
	}
	return true
}

// activationRegistration is the actual cluster registration/ownership text a
// fence records: name, port and follow generation (or awaiting state).
func activationRegistration(spec config.ClusterSpec) string {
	return fmt.Sprintf("%s:%d:%s", spec.Name, spec.Port, spec.FollowGeneration)
}

// activationFenceIdentity is one fence write or advance. Spec is the expected
// current registration; the fence derives its deterministic paths and port
// from it unless an advance overrides the temporary phase's actual values.
type activationFenceIdentity struct {
	Database     string
	Kind         string
	OperationID  string
	Spec         config.ClusterSpec
	TargetStanza string
	OldStanza    string
	NewStanza    string
	Aside        string
	Registration string
	Ownership    string
	Port         int
	DataDir      string
	ConfDir      string
	Requirements []catalog.Extension
	Renames      map[string]string
}

func (id activationFenceIdentity) fence() (backup.ActivationFence, error) {
	instance, err := currentFollowInstance()
	if err != nil {
		return backup.ActivationFence{}, err
	}
	if id.Database == "" || id.Kind == "" || id.OperationID == "" {
		return backup.ActivationFence{}, fmt.Errorf("an activation fence needs a database, kind and operation id")
	}
	if !validFenceKind(id.Kind) {
		return backup.ActivationFence{}, fmt.Errorf("unknown activation fence kind %q", id.Kind)
	}
	if !validFencePhaseForKind(id.Kind, fencePhasePrepared) {
		return backup.ActivationFence{}, fmt.Errorf("a %s activation fence is created only at routing", id.Kind)
	}
	if id.TargetStanza == "" {
		return backup.ActivationFence{}, fmt.Errorf("the activation fence of %s has no target stanza", id.Database)
	}
	port := id.Port
	if port == 0 {
		port = id.Spec.Port
	}
	dataDir := id.DataDir
	if dataDir == "" {
		return backup.ActivationFence{}, fmt.Errorf("the activation fence of %s has no data directory", id.Database)
	}
	confDir := id.ConfDir
	if confDir == "" {
		return backup.ActivationFence{}, fmt.Errorf("the activation fence of %s has no configuration directory", id.Database)
	}
	registration := id.Registration
	if registration == "" {
		registration = activationRegistration(id.Spec)
	}
	return backup.ActivationFence{
		OperationID: id.OperationID, Kind: id.Kind, Database: id.Database,
		Instance: instance, TargetStanza: id.TargetStanza,
		OldStanza: id.OldStanza, NewStanza: id.NewStanza, Aside: id.Aside,
		Registration: registration, Ownership: id.Ownership,
		Port: port, DataDir: dataDir, ConfDir: confDir,
		Requirements: append([]catalog.Extension(nil), id.Requirements...),
		Renames:      cloneRenames(id.Renames), Phase: fencePhasePrepared, CreatedAt: stamp(),
	}, nil
}

func cloneRenames(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func activationFenceRefusal(f backup.ActivationFence, db string) error {
	return output.NewError(output.CodePrecondition,
		db+"'s "+f.Kind+" activation is not settled ("+f.Phase+")",
		output.ExitPrecondition,
		"Finish the owning operation that wrote this fence; ordinary apply, SQL and routing stay blocked until it completes.").
		WithDetails(map[string]any{
			"reason": "activation_fence_active", "database": db,
			"operation_id": f.OperationID, "kind": f.Kind, "phase": f.Phase,
		})
}

func refuseActiveActivationFence(cfg config.Cluster, db string) error {
	recs, err := backup.LoadRecords()
	if err != nil {
		return output.NewError(output.CodePrecondition,
			"the node's activation records could not be read, so "+db+" cannot be activated",
			output.ExitPrecondition, "Inspect the node's records file and fix it; vaire never resets it on its own.").
			WithDetails(map[string]any{"reason": "activation_state_unknown", "database": db})
	}
	f, ok := recs.ActivationFenceFor(db)
	if !ok || !f.Active() {
		return nil
	}
	if err := verifyFenceInstance(f, db); err != nil {
		return err
	}
	if err := verifyFencePaths(cfg, f, db); err != nil {
		return err
	}
	return activationFenceRefusal(f, db)
}

// refuseAnyActivationFence refuses a destructive or re-identifying change
// while any fence, even a routing one, still owns the database's local
// authority. Only the owning operation may retire it.
func refuseAnyActivationFence(cfg config.Cluster, db string) error {
	recs, err := backup.LoadRecords()
	if err != nil {
		return output.NewError(output.CodePrecondition,
			"the node's activation records could not be read, so "+db+" cannot be changed",
			output.ExitPrecondition, "Inspect the node's records file and fix it; vaire never resets it on its own.").
			WithDetails(map[string]any{"reason": "activation_state_unknown", "database": db})
	}
	f, ok := recs.ActivationFenceFor(db)
	if !ok {
		return nil
	}
	if err := verifyFenceInstance(f, db); err != nil {
		return err
	}
	if err := verifyFencePaths(cfg, f, db); err != nil {
		return err
	}
	return activationFenceRefusal(f, db)
}

func refuseActiveActivationFences(cfg config.Cluster, p stepParams) error {
	if p.DB != "" {
		return refuseActiveActivationFence(cfg, p.DB)
	}
	names := make([]string, 0, len(cfg.Clusters)+len(cfg.Awaiting))
	for _, spec := range cfg.Clusters {
		names = append(names, spec.Name)
	}
	for _, spec := range cfg.Awaiting {
		names = append(names, spec.Name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := refuseActiveActivationFence(cfg, name); err != nil {
			return err
		}
	}
	return nil
}

func verifyFenceInstance(f backup.ActivationFence, db string) error {
	instance, err := currentFollowInstance()
	if err != nil {
		return err
	}
	if f.Instance == "" || f.Instance != instance {
		return output.NewError(output.CodePrecondition,
			db+"'s activation fence belongs to another installation instance, so this node will not act on it",
			output.ExitPrecondition, "Retry the owning operation on the node that wrote the fence, or repair the records file deliberately.").
			WithDetails(map[string]any{"reason": "activation_fence_foreign", "database": db, "operation_id": f.OperationID})
	}
	return nil
}

// verifyFencePaths checks the fence's actual registration/port/directory
// evidence against the current configuration. It never trusts the record's
// fields alone: the deterministic path and port must still be the database's.
func verifyFencePaths(cfg config.Cluster, f backup.ActivationFence, db string) error {
	spec, ok := cfg.ClusterByName(db)
	if !ok {
		return output.NewError(output.CodePrecondition,
			db+" has an activation fence but no current cluster.toml entry",
			output.ExitPrecondition, "Finish the owning operation before removing or re-identifying the database.").
			WithDetails(map[string]any{"reason": "activation_fence_orphan", "database": db, "operation_id": f.OperationID})
	}
	if f.Port != 0 && f.Port != spec.Port && !fenceHasTemporaryPort(f) {
		return activationIdentityError(db, "port", strconv.Itoa(f.Port), strconv.Itoa(spec.Port))
	}
	if err := verifyFenceDataDir(cfg, f, db); err != nil {
		return err
	}
	return verifyFenceConfDir(f, db)
}

// fenceHasTemporaryPort reports phases that may still record the temporary
// cluster's port: a fork's adopting checkpoint and a restore's prepared one.
// Temporary paths are narrower; see fenceHasTemporaryPaths.
func fenceHasTemporaryPort(f backup.ActivationFence) bool {
	return (f.Kind == "fork" && f.Phase == fencePhaseAdopting) ||
		(f.Kind == "restore-promote" && f.Phase == fencePhasePrepared)
}

// fenceHasTemporaryPaths admits only a restore's prepared/swapped checkpoint.
// A fork/adopting fence may carry the temporary port, but its data and
// configuration paths must already be the live ones; a restore-abort never
// admits either exception.
func fenceHasTemporaryPaths(f backup.ActivationFence) bool {
	return f.Kind == "restore-promote" && (f.Phase == fencePhasePrepared || f.Phase == fencePhaseSwapped)
}

func verifyFenceDataDir(cfg config.Cluster, f backup.ActivationFence, db string) error {
	if f.DataDir == "" {
		return nil
	}
	live := cfg.Server.DataDirectory(db)
	if f.DataDir == live {
		return nil
	}
	if fenceHasTemporaryPaths(f) && f.DataDir == cfg.Server.DataDirectory(backup.RestoringName(db)) {
		return nil
	}
	return activationIdentityError(db, "data directory", f.DataDir, live)
}

func verifyFenceConfDir(f backup.ActivationFence, db string) error {
	if f.ConfDir == "" {
		return nil
	}
	live := steps.PGConfDir(db)
	if f.ConfDir == live {
		return nil
	}
	if fenceHasTemporaryPaths(f) && f.ConfDir == steps.PGConfDir(backup.RestoringName(db)) {
		return nil
	}
	return activationIdentityError(db, "configuration directory", f.ConfDir, live)
}

func activationIdentityError(db, what, have, want string) error {
	return output.NewError(output.CodePrecondition,
		db+"'s activation fence does not match the database's current "+what+": "+have+" (recorded) and "+want+" (actual)",
		output.ExitPrecondition,
		"Finish the owning operation, or repair the records deliberately; vaire will not adopt another cluster's authority.").
		WithDetails(map[string]any{"reason": "activation_fence_identity", "database": db, "field": what, "recorded": have, "actual": want})
}

func writeActivationFence(cfg config.Cluster, id activationFenceIdentity) (backup.ActivationFence, error) {
	if id.DataDir == "" {
		id.DataDir = cfg.Server.DataDirectory(id.Database)
	}
	if id.ConfDir == "" {
		id.ConfDir = steps.PGConfDir(id.Database)
	}
	if id.Port == 0 {
		id.Port = id.Spec.Port
	}
	candidate, err := id.fence()
	if err != nil {
		return backup.ActivationFence{}, err
	}
	recs, err := backup.LoadRecords()
	if err != nil {
		return backup.ActivationFence{}, err
	}
	f, ok := recs.ActivationFenceFor(id.Database)
	if !ok {
		if err := backup.SetActivationFence(candidate); err != nil {
			return backup.ActivationFence{}, err
		}
		return candidate, nil
	}
	return updateActivationFence(cfg, id, candidate, f)
}

// updateActivationFence reconciles a new fence identity with the existing
// fence of the same database. A later phase keeps the recorded endpoint; a
// prepared fence may only be enriched with facts fixed later.
func updateActivationFence(cfg config.Cluster, id activationFenceIdentity, candidate, f backup.ActivationFence) (backup.ActivationFence, error) {
	if f.OperationID != id.OperationID {
		return backup.ActivationFence{}, output.NewError(output.CodePrecondition,
			id.Database+" has an activation fence from another operation",
			output.ExitPrecondition, "Finish or repair that operation before starting another.").
			WithDetails(map[string]any{"reason": "activation_fence_changed", "database": id.Database, "operation_id": f.OperationID})
	}
	if err := sameFenceImmutable(f, candidate); err != nil {
		return backup.ActivationFence{}, err
	}
	if f.Phase != fencePhasePrepared {
		// An interrupted operation already advanced: its recorded phase
		// owns the endpoint (temporary before Adopt/swap, live after).
		// Validate the existing checkpoint against the actual state
		// instead of recreating prepared expectations.
		if _, _, err := verifyActivationFence(cfg, id.Database, id.OperationID, id.Kind, f.Phase); err != nil {
			return backup.ActivationFence{}, err
		}
		return f, nil
	}
	if err := sameFenceIdentity(f, candidate); err != nil {
		return backup.ActivationFence{}, err
	}
	if !fenceEnrichable(f, candidate) {
		return f, nil
	}
	if err := backup.UpdateActivationFence(id.Database, id.OperationID, func(cur *backup.ActivationFence) error {
		if err := sameFenceIdentity(*cur, candidate); err != nil {
			return err
		}
		if cur.Aside == "" {
			cur.Aside = candidate.Aside
		}
		if len(cur.Requirements) == 0 {
			cur.Requirements = append([]catalog.Extension(nil), candidate.Requirements...)
		}
		if len(cur.Renames) == 0 {
			cur.Renames = cloneRenames(candidate.Renames)
		}
		return nil
	}); err != nil {
		return backup.ActivationFence{}, err
	}
	recs, err := backup.LoadRecords()
	if err != nil {
		return backup.ActivationFence{}, err
	}
	f, _ = recs.ActivationFenceFor(id.Database)
	return f, nil
}

// fenceEnrichable reports whether a prepared fence can record facts fixed
// after it was written without moving its immutable identity.
func fenceEnrichable(f, candidate backup.ActivationFence) bool {
	return f.Aside == "" && candidate.Aside != "" ||
		len(f.Requirements) == 0 && len(candidate.Requirements) > 0 ||
		len(f.Renames) == 0 && len(candidate.Renames) > 0
}

func sameFenceIdentity(have, want backup.ActivationFence) error {
	fields := []struct {
		name string
		a, b string
	}{
		{"instance", have.Instance, want.Instance},
		{"kind", have.Kind, want.Kind},
		{"target stanza", have.TargetStanza, want.TargetStanza},
		{"old stanza", have.OldStanza, want.OldStanza},
		{"new stanza", have.NewStanza, want.NewStanza},
		{"aside", have.Aside, want.Aside},
		{"registration", have.Registration, want.Registration},
		{"ownership", have.Ownership, want.Ownership},
	}
	for _, f := range fields {
		if err := sameFenceIdentityField(have, f.name, f.a, f.b); err != nil {
			return err
		}
	}
	if have.Port != 0 && want.Port != 0 && have.Port != want.Port {
		return activationIdentityError(have.Database, "port", strconv.Itoa(have.Port), strconv.Itoa(want.Port))
	}
	if have.DataDir != "" && want.DataDir != "" && have.DataDir != want.DataDir {
		return activationIdentityError(have.Database, "data directory", have.DataDir, want.DataDir)
	}
	if have.ConfDir != "" && want.ConfDir != "" && have.ConfDir != want.ConfDir {
		return activationIdentityError(have.Database, "configuration directory", have.ConfDir, want.ConfDir)
	}
	return nil
}

// sameFenceIdentityField compares one identity field under its phase-specific
// exemption: an aside fixed later is optional, a registration's generation may
// become empty, and prepared follow ownership is not current after promotion.
func sameFenceIdentityField(have backup.ActivationFence, name, a, b string) error {
	switch name {
	case "aside":
		if a == "" || b == "" {
			return nil
		}
	case "registration":
		if registrationNamePort(a) == registrationNamePort(b) {
			return nil
		}
	case "ownership":
		if have.Phase != fencePhasePrepared && have.Phase != fencePhaseAdopting {
			return nil
		}
	}
	if a == b {
		return nil
	}
	return activationIdentityError(have.Database, name, a, b)
}

func registrationNamePort(registration string) string {
	name, rest, ok := strings.Cut(registration, ":")
	if !ok {
		return registration
	}
	port, _, _ := strings.Cut(rest, ":")
	return name + ":" + port
}

// sameFenceImmutable compares the parts of a fence that never change across
// phase advances. Endpoint fields are phase-specific and validated separately.
func sameFenceImmutable(have, want backup.ActivationFence) error {
	fields := []struct {
		name string
		a, b string
	}{
		{"instance", have.Instance, want.Instance},
		{"kind", have.Kind, want.Kind},
		{"target stanza", have.TargetStanza, want.TargetStanza},
		{"old stanza", have.OldStanza, want.OldStanza},
		{"new stanza", have.NewStanza, want.NewStanza},
	}
	for _, f := range fields {
		if f.a != f.b {
			return activationIdentityError(have.Database, f.name, f.a, f.b)
		}
	}
	if have.Aside != "" && want.Aside != "" && have.Aside != want.Aside {
		return activationIdentityError(have.Database, "aside", have.Aside, want.Aside)
	}
	return nil
}

// verifyActivationFence loads and validates one fence for continuation,
// phase advance or routing clear. The operation, kind, installation instance,
// immutable stanza transition, actual paths/port/registration and phase must
// all agree. An empty phases list accepts any valid phase.
func verifyActivationFence(cfg config.Cluster, db, opID, kind string, phases ...string) (backup.ActivationFence, config.ClusterSpec, error) {
	recs, err := backup.LoadRecords()
	if err != nil {
		return backup.ActivationFence{}, config.ClusterSpec{}, err
	}
	f, ok := recs.ActivationFenceFor(db)
	if !ok || f.OperationID != opID {
		return backup.ActivationFence{}, config.ClusterSpec{}, output.NewError(output.CodePrecondition,
			"the activation fence of "+db+" changed while the operation was continuing",
			output.ExitPrecondition, "Retry the owning operation; vaire will not guess another operation's state.").
			WithDetails(map[string]any{"reason": "activation_fence_changed", "database": db})
	}
	if err := verifyFenceKindAndPhase(f, db, kind, phases); err != nil {
		return backup.ActivationFence{}, config.ClusterSpec{}, err
	}
	if err := verifyFenceInstance(f, db); err != nil {
		return backup.ActivationFence{}, config.ClusterSpec{}, err
	}
	if err := verifyFencePaths(cfg, f, db); err != nil {
		return backup.ActivationFence{}, config.ClusterSpec{}, err
	}
	spec, err := verifyFenceTarget(cfg, f, db)
	if err != nil {
		return backup.ActivationFence{}, config.ClusterSpec{}, err
	}
	if f.Kind == "unfollow" && (f.Phase == fencePhasePrepared || f.Phase == fencePhaseAdopting) &&
		spec.IsFollower() && spec.FollowGeneration != "" && f.Ownership != spec.FollowGeneration {
		return backup.ActivationFence{}, config.ClusterSpec{}, activationIdentityError(db, "follow generation", f.Ownership, spec.FollowGeneration)
	}
	if err := verifyFenceRegistration(f, db); err != nil {
		return backup.ActivationFence{}, config.ClusterSpec{}, err
	}
	if f.Kind == "restore-abort" {
		if err := verifyRestoreAbortFence(cfg, f, spec); err != nil {
			return backup.ActivationFence{}, config.ClusterSpec{}, err
		}
	}
	if err := verifyFenceActualRegistration(f, spec, db); err != nil {
		return backup.ActivationFence{}, config.ClusterSpec{}, err
	}
	return f, spec, nil
}

// verifyFenceKindAndPhase checks the fence's declared kind, a valid phase for
// that kind, and the caller's accepted phases.
func verifyFenceKindAndPhase(f backup.ActivationFence, db, kind string, phases []string) error {
	if f.Kind != kind {
		return activationIdentityError(db, "kind", f.Kind, kind)
	}
	if !validFenceKind(f.Kind) {
		return output.NewError(output.CodePrecondition,
			db+"'s activation fence has an unknown kind "+f.Kind,
			output.ExitPrecondition, "Repair the records file deliberately; vaire will not guess the operation.").
			WithDetails(map[string]any{"reason": "activation_fence_kind", "database": db, "kind": f.Kind})
	}
	if !validFencePhase(f.Phase) || !validFencePhaseForKind(f.Kind, f.Phase) {
		return output.NewError(output.CodePrecondition,
			db+"'s activation fence has an unknown phase "+f.Phase,
			output.ExitPrecondition, "Repair the records file deliberately; vaire will not guess the checkpoint.").
			WithDetails(map[string]any{"reason": "activation_fence_phase", "database": db, "phase": f.Phase})
	}
	if len(phases) == 0 || slices.Contains(phases, f.Phase) {
		return nil
	}
	return output.NewError(output.CodePrecondition,
		db+"'s activation fence is in phase "+f.Phase+", not "+strings.Join(phases, " or "),
		output.ExitPrecondition, "Finish the owning operation from its recorded checkpoint.").
		WithDetails(map[string]any{"reason": "activation_fence_phase", "database": db, "phase": f.Phase})
}

// verifyFenceTarget proves the fence names a current cluster.toml entry, its
// stanza transition, and a recorded old/new stanza pair.
func verifyFenceTarget(cfg config.Cluster, f backup.ActivationFence, db string) (config.ClusterSpec, error) {
	spec, ok := cfg.ClusterByName(db)
	if !ok {
		return config.ClusterSpec{}, activationIdentityError(db, "cluster.toml entry", "present", "missing")
	}
	if !fenceTargetMatches(f, spec) {
		return config.ClusterSpec{}, activationIdentityError(db, "target stanza", f.TargetStanza, spec.Stanza)
	}
	if f.OldStanza == "" || f.NewStanza == "" {
		return config.ClusterSpec{}, output.NewError(output.CodePrecondition,
			db+"'s activation fence has no recorded old/new stanza transition",
			output.ExitPrecondition, "Repair the records deliberately; vaire will not adopt a data identity it cannot prove.").
			WithDetails(map[string]any{"reason": "activation_fence_identity", "database": db})
	}
	return spec, nil
}

// verifyFenceActualRegistration checks a fork/unfollow fence's final phase
// against the host's postgresql-common registration. A real follower's data
// directory exists; a lifecycle test that models only the records has no host
// tree to read.
func verifyFenceActualRegistration(f backup.ActivationFence, spec config.ClusterSpec, db string) error {
	if f.Kind != "fork" && f.Kind != "unfollow" {
		return nil
	}
	check := f.Kind == "fork"
	if !check {
		if _, err := os.Stat(f.DataDir); err == nil {
			check = true
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if !check || (f.Phase != fencePhaseAdopted && f.Phase != fencePhasePromoted && f.Phase != fencePhaseRouting) {
		return nil
	}
	if !followConfRegistered(spec.Name) {
		return output.NewError(output.CodePrecondition,
			db+"'s activation fence says its registration is verified, but no postgresql-common registration exists",
			output.ExitPrecondition, "Finish the owning operation; vaire will not adopt an unregistered cluster.").
			WithDetails(map[string]any{"reason": "activation_registration_missing", "database": db})
	}
	data, err := followRegisteredData(spec.Name)
	if err != nil {
		return err
	}
	if data == "" || data != f.DataDir {
		return activationIdentityError(db, "registered data directory", data, f.DataDir)
	}
	return nil
}

func fenceTargetMatches(f backup.ActivationFence, spec config.ClusterSpec) bool {
	switch f.Kind {
	case "fork":
		return f.TargetStanza == spec.Stanza
	case "unfollow":
		return f.TargetStanza != "" && (spec.Stanza == "" || f.TargetStanza == spec.Stanza)
	case "restore-promote":
		return f.TargetStanza == f.NewStanza || f.TargetStanza == spec.Stanza || f.TargetStanza == f.OldStanza
	case "restore-abort":
		return f.TargetStanza == spec.Stanza
	default:
		return false
	}
}

func verifyFenceRegistration(f backup.ActivationFence, db string) error {
	if f.Registration == "" {
		return output.NewError(output.CodePrecondition,
			db+"'s activation fence has no recorded registration",
			output.ExitPrecondition, "Repair the records deliberately; vaire will not adopt an unregistered cluster.").
			WithDetails(map[string]any{"reason": "activation_fence_identity", "database": db})
	}
	name, rest, ok := strings.Cut(f.Registration, ":")
	if !ok {
		return activationIdentityError(db, "registration", f.Registration, db+":<port>:<generation>")
	}
	portText, _, _ := strings.Cut(rest, ":")
	port, err := strconv.Atoi(portText)
	if err != nil {
		return activationIdentityError(db, "registration", f.Registration, db+":<port>:<generation>")
	}
	restoreInspection := f.Kind == "restore-promote" && name == backup.RestoringName(db)
	if name != db && !restoreInspection {
		return activationIdentityError(db, "registration name", name, db)
	}
	if f.Port != 0 && port != f.Port {
		return activationIdentityError(db, "registration port", strconv.Itoa(port), strconv.Itoa(f.Port))
	}
	return nil
}

// verifyRestoreAbortFence proves a restore-abort fence still owns exactly the
// untouched live cluster: its current stanza/port/paths, full activation
// registration including follow generation, and the verified aside. It carries
// no promotion requirements or renames.
func verifyRestoreAbortFence(cfg config.Cluster, f backup.ActivationFence, spec config.ClusterSpec) error {
	db := spec.Name
	if f.Database != db {
		return activationIdentityError(db, "database", f.Database, db)
	}
	if f.TargetStanza != spec.Stanza || f.OldStanza != spec.Stanza || f.NewStanza != spec.Stanza {
		return activationIdentityError(db, "stanza", f.TargetStanza+"/"+f.OldStanza+"/"+f.NewStanza, spec.Stanza)
	}
	if f.Registration != activationRegistration(spec) {
		return activationIdentityError(db, "registration", f.Registration, activationRegistration(spec))
	}
	if f.Ownership != spec.FollowGeneration {
		return activationIdentityError(db, "follow generation", f.Ownership, spec.FollowGeneration)
	}
	if f.Port != spec.Port {
		return activationIdentityError(db, "port", strconv.Itoa(f.Port), strconv.Itoa(spec.Port))
	}
	if f.DataDir != cfg.Server.DataDirectory(db) {
		return activationIdentityError(db, "data directory", f.DataDir, cfg.Server.DataDirectory(db))
	}
	if f.ConfDir != steps.PGConfDir(db) {
		return activationIdentityError(db, "configuration directory", f.ConfDir, steps.PGConfDir(db))
	}
	if f.Aside == "" || f.CreatedAt == "" {
		return output.NewError(output.CodePrecondition,
			db+"'s restore-abort fence has no verified aside or creation time",
			output.ExitPrecondition, "Repair the records deliberately; vaire will not route a restore whose pre-swap evidence is missing.").
			WithDetails(map[string]any{"reason": "activation_fence_identity", "database": db})
	}
	if len(f.Requirements) != 0 || len(f.Renames) != 0 {
		return output.NewError(output.CodePrecondition,
			db+"'s restore-abort fence retained promotion facts",
			output.ExitPrecondition, "Repair the records deliberately; vaire will not publish a restore's requirements or renames as the live cluster's.").
			WithDetails(map[string]any{"reason": "activation_fence_identity", "database": db})
	}
	return nil
}

func fencePhaseRank(phase string) int {
	switch phase {
	case fencePhasePrepared:
		return 1
	case fencePhaseAdopting:
		return 2
	case fencePhaseAdopted:
		return 3
	case fencePhaseSwapped:
		return 4
	case fencePhasePromoted:
		return 5
	case fencePhaseRouting:
		return 6
	}
	return 0
}

// advanceActivationFence moves one verified fence from a known phase to the
// next checkpoint. It is idempotent: a retry that already reached the target
// (or a later checkpoint) does not move it backwards. update refreshes the
// actual registration/port/paths when a phase changes them; the CAS rechecks
// the operation, instance and immutable stanza transition inside the records
// lock.
func advanceActivationFence(cfg config.Cluster, db, opID, kind, from, to string, spec config.ClusterSpec, update func(*backup.ActivationFence)) error {
	if kind == "restore-abort" {
		return fmt.Errorf("a restore-abort activation fence is created at routing and cannot advance")
	}
	if !validFencePhase(from) || !validFencePhase(to) {
		return fmt.Errorf("invalid activation phase transition %q -> %q", from, to)
	}
	if current, _, err := verifyActivationFence(cfg, db, opID, kind); err == nil {
		if fencePhaseRank(current.Phase) >= fencePhaseRank(to) {
			return nil
		}
	}
	f, _, err := verifyActivationFence(cfg, db, opID, kind, from)
	if err != nil {
		return err
	}
	return backup.UpdateActivationFence(db, opID, func(current *backup.ActivationFence) error {
		if err := sameFenceIdentity(*current, f); err != nil {
			return err
		}
		if current.Phase != from {
			return output.NewError(output.CodePrecondition,
				db+"'s activation fence advanced to "+current.Phase+" while the operation was continuing",
				output.ExitPrecondition, "Retry from the current checkpoint; vaire will not move it backwards.").
				WithDetails(map[string]any{"reason": "activation_fence_phase", "database": db, "phase": current.Phase})
		}
		current.Phase = to
		current.Registration = activationRegistration(spec)
		current.Port = spec.Port
		current.DataDir = cfg.Server.DataDirectory(db)
		current.ConfDir = steps.PGConfDir(db)
		if update != nil {
			update(current)
		}
		return nil
	})
}

// clearActivationFenceVerified retires exactly the verified routing fence in
// one records compare-and-set. The complete verified snapshot is compared
// under the lock, not only operation/kind/phase/target/port: any changed
// requirement, rename, aside, registration, ownership, instance or path leaves
// the fence whole.
func clearActivationFenceVerified(cfg config.Cluster, db, opID, kind string) error {
	want, _, err := verifyActivationFence(cfg, db, opID, kind, fencePhaseRouting)
	if err != nil {
		return err
	}
	err = backup.UpdateRecords(func(s *backup.Records) error {
		f, ok := s.ActivationFenceFor(db)
		if !ok {
			return output.NewError(output.CodePrecondition,
				db+"'s activation fence disappeared before its routing completion",
				output.ExitPrecondition, "Retry vaire apply; vaire will not recreate or guess a fence.").
				WithDetails(map[string]any{"reason": "activation_fence_changed", "database": db})
		}
		if !reflect.DeepEqual(f, want) {
			return activationIdentityError(db, "routing completion", f.OperationID+":"+f.Phase, want.OperationID+":"+want.Phase)
		}
		spec, ok := cfg.ClusterByName(db)
		if !ok {
			return activationIdentityError(db, "cluster.toml entry", "present", "missing")
		}
		if f.TargetStanza != spec.Stanza {
			return activationIdentityError(db, "target stanza", f.TargetStanza, spec.Stanza)
		}
		if f.Port != spec.Port {
			return activationIdentityError(db, "port", strconv.Itoa(f.Port), strconv.Itoa(spec.Port))
		}
		if _, ok := s.Restores[db]; ok {
			return routingContinuationWithRestoreError(db)
		}
		if err := verifyFenceInstance(f, db); err != nil {
			return err
		}
		delete(s.Activation, db)
		return nil
	})
	if err == nil && kind == "fork" {
		// The fence was the fork job's last unresolved authority; its exact
		// retained record can now be pruned with the ordinary history.
		releaseResolvedMaterialisationJob(want.OperationID, "db-fork")
	}
	return err
}

// routingContinuationWithRestoreError is the fail-closed state where a
// routing continuation and a restore record coexist for one database. No
// common finalizer may publish, resume or clear from it.
func routingContinuationWithRestoreError(db string) error {
	return output.NewError(output.CodePrecondition,
		db+" has an open restore record while its activation routing continuation is being completed",
		output.ExitPrecondition,
		"Inspect the node records and repair the ambiguous state deliberately; vaire will not publish or clear routing while a restore record exists.").
		WithDetails(map[string]any{"reason": "activation_fence_changed", "database": db})
}

// completeUnfollowSafety atomically marks the unfollow's privilege settlement
// and moves its activation fence to routing. The unfollow record and fence
// must both still be the exact operation the job owns; a failed write leaves
// the promoted continuation whole.
func completeUnfollowSafety(cfg config.Cluster, spec config.ClusterSpec, unfollow backup.Unfollow, fence backup.ActivationFence) error {
	return backup.UpdateRecords(func(s *backup.Records) error {
		u, ok := s.Unfollowing[spec.Name]
		if !ok {
			// A retry after the safety completion already retired the record
			// may only continue if the fence is already routing.
			if f, ok := s.ActivationFenceFor(spec.Name); ok && f.OperationID == fence.OperationID && f.Phase == fencePhaseRouting {
				return nil
			}
			return fmt.Errorf("the unfollow of %s changed while settling its safety", spec.Name)
		}
		if u.OperationID != unfollow.OperationID || u.Source != unfollow.Source ||
			u.Stanza != unfollow.Stanza {
			return fmt.Errorf("the unfollow of %s changed while settling its safety", spec.Name)
		}
		f, ok := s.ActivationFenceFor(spec.Name)
		if !ok || f.OperationID != fence.OperationID || f.Kind != "unfollow" ||
			f.TargetStanza != fence.TargetStanza || f.OldStanza != fence.OldStanza || f.NewStanza != fence.NewStanza {
			return fmt.Errorf("the activation fence of %s changed while settling its safety", spec.Name)
		}
		if u.Settled && f.Phase == fencePhaseRouting {
			return nil
		}
		if f.Phase != fencePhasePromoted {
			return fmt.Errorf("the activation fence of %s is in phase %s, not %s", spec.Name, f.Phase, fencePhasePromoted)
		}
		if err := verifyFenceInstance(f, spec.Name); err != nil {
			return err
		}
		u.Settled = true
		s.Unfollowing[spec.Name] = u
		f.Phase = fencePhaseRouting
		f.Registration = activationRegistration(spec)
		f.Ownership = spec.FollowGeneration
		f.Port = spec.Port
		f.DataDir = cfg.Server.DataDirectory(spec.Name)
		f.ConfDir = steps.PGConfDir(spec.Name)
		s.Activation[spec.Name] = f
		return nil
	})
}

// refusePendingConfigChanges refuses an ordinary config edit that removes or
// re-identifies a database while an activation fence still owns its local
// authority. A prepared/adopted/routing fence binds port, stanza, follow
// identity and name; a new entry over the same name must not inherit it.
func refusePendingConfigChanges(old, updated config.Cluster) error {
	recs, err := backup.LoadRecords()
	if err != nil {
		return output.NewError(output.CodePrecondition,
			"the node's activation records could not be read, so a pending target cannot be changed",
			output.ExitPrecondition, "Inspect the node's records file and fix it; vaire never resets it on its own.").
			WithDetails(map[string]any{"reason": "activation_state_unknown"})
	}
	for db, f := range recs.Activation {
		if f.OperationID == "" {
			continue
		}
		oldSpec, inOld := old.ClusterByName(db)
		if !inOld {
			continue
		}
		newSpec, inNew := updated.ClusterByName(db)
		if !inNew {
			return output.NewError(output.CodePrecondition,
				db+" has an outstanding "+f.Kind+" activation fence, so its entry cannot be removed",
				output.ExitPrecondition, "Finish or repair the owning operation before removing or re-identifying the database.").
				WithDetails(map[string]any{"reason": "activation_fence_pending", "database": db, "operation_id": f.OperationID})
		}
		if oldSpec.Port != newSpec.Port || oldSpec.Stanza != newSpec.Stanza ||
			oldSpec.Follows != newSpec.Follows || oldSpec.FollowGeneration != newSpec.FollowGeneration ||
			oldSpec.Name != newSpec.Name {
			return output.NewError(output.CodePrecondition,
				db+" has an outstanding "+f.Kind+" activation fence, so its port/stanza/follow identity cannot change",
				output.ExitPrecondition, "Finish or repair the owning operation before removing or re-identifying the database.").
				WithDetails(map[string]any{"reason": "activation_fence_pending", "database": db, "operation_id": f.OperationID})
		}
	}
	return nil
}

// forkFenceFor returns the fork fences an ordinary apply may finish: the
// non-secret adoption/configuration continuation and route finalization.
func forkFenceFor(db string) (backup.ActivationFence, bool, error) {
	recs, err := backup.LoadRecords()
	if err != nil {
		return backup.ActivationFence{}, false, err
	}
	f, ok := recs.ActivationFenceFor(db)
	if !ok || f.Kind != "fork" {
		return backup.ActivationFence{}, false, nil
	}
	if err := verifyFenceInstance(f, db); err != nil {
		return f, false, err
	}
	switch f.Phase {
	case fencePhaseAdopting, fencePhaseAdopted, fencePhaseRouting:
		return f, true, nil
	default:
		return f, false, nil
	}
}

// publishRouting renders and reloads both poolers. It is a variable so a
// lifecycle test can run the real render into temporary files while modelling
// the host's pooler runtime.
var publishRouting = func(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) error {
	return applyPoolers(ctx, cfg, rt, "", report)
}

// poolerRuntimeRoutes reads the actual [databases] routes a running pooler
// serves. The route's backend mapping is verified, not only the rendered file.
var poolerRuntimeRoutes = steps.PoolerRoutes

// poolerResumeDatabase releases a transition pause on both poolers.
var poolerResumeDatabase = steps.ResumePoolerDatabase

// verifyRoutingFence is the shared routing finalizer: it verifies the actual
// route on both running poolers, releases and re-verifies the transition
// pause, and only then clears the exact fence. Any failure leaves the fence in
// routing for an ordinary apply retry.
func verifyRoutingFence(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec, opID, kind string) error {
	if _, _, err := verifyActivationFence(cfg, spec.Name, opID, kind, fencePhaseRouting); err != nil {
		return err
	}
	if err := verifyRoutingRoutes(ctx, spec, false); err != nil {
		return err
	}
	poolerResumeDatabase(ctx, spec.Name)
	if err := verifyRoutingRoutes(ctx, spec, true); err != nil {
		return err
	}
	return clearActivationFenceVerified(cfg, spec.Name, opID, kind)
}

// verifyRoutingRoutes proves both running poolers serve the live mapping and
// no restore-inspection alias. After resume it also proves the transition
// pause is gone; the mapping and alias absence are rechecked so a host that
// changed while releasing the pause cannot be cleared.
func verifyRoutingRoutes(ctx context.Context, spec config.ClusterSpec, afterResume bool) error {
	poolers := []struct {
		name string
		dir  string
		port int
	}{
		{"transaction", steps.PGSocketDir, steps.PgbouncerPort},
		{"session", steps.SessionSocketDir, steps.SessionPoolerPort},
	}
	for _, p := range poolers {
		routes, err := poolerRuntimeRoutes(ctx, p.dir, p.port)
		if err != nil {
			detail := "cannot read its running routes: "
			if afterResume {
				detail = "cannot verify its pause release: "
			}
			return routingIncomplete(spec.Name, p.name, detail+err.Error())
		}
		route, ok := routes[spec.Name]
		if !ok {
			if afterResume {
				return routingIncomplete(spec.Name, p.name, "dropped the alias while releasing its pause")
			}
			return routingIncomplete(spec.Name, p.name, "does not serve the alias")
		}
		if route.Host != steps.PGSocketDir || route.Port != spec.Port || route.Database != spec.DatabaseName() {
			return routingIncomplete(spec.Name, p.name,
				"routes the alias to host "+route.Host+", port "+strconv.Itoa(route.Port)+", database "+route.Database)
		}
		if _, ok := routes[backup.RestoringName(spec.Name)]; ok {
			return routingIncomplete(spec.Name, p.name, "still serves the restore-inspection alias")
		}
		if afterResume && route.Paused {
			return routingIncomplete(spec.Name, p.name, "still has the alias paused")
		}
	}
	return nil
}

func routingIncomplete(db, pooler, detail string) error {
	return output.NewError(output.CodePrecondition,
		db+"'s route is not verified on the "+pooler+" pooler: "+detail,
		output.ExitPrecondition,
		"Run vaire apply to finish the routing; the activation fence stays in its routing phase.").
		WithDetails(map[string]any{"reason": "activation_routing_incomplete", "database": db, "pooler": pooler})
}

// verifyPoolerAliasAbsent proves both running poolers have withdrawn the
// target and its restore-inspection alias after the fence was rendered.
func verifyPoolerAliasAbsent(ctx context.Context, db string) error {
	poolers := []struct {
		name string
		dir  string
		port int
	}{
		{"transaction", steps.PGSocketDir, steps.PgbouncerPort},
		{"session", steps.SessionSocketDir, steps.SessionPoolerPort},
	}
	for _, p := range poolers {
		routes, err := poolerRuntimeRoutes(ctx, p.dir, p.port)
		if err != nil {
			return routingIncomplete(db, p.name, "cannot read its running routes: "+err.Error())
		}
		if _, ok := routes[db]; ok {
			return routingIncomplete(db, p.name, "still serves the target alias")
		}
		if _, ok := routes[backup.RestoringName(db)]; ok {
			return routingIncomplete(db, p.name, "still serves the restore-inspection alias")
		}
	}
	return nil
}

// routingFencesFor returns the fences whose safety transition is complete and
// whose routing an ordinary apply may finalize.
func routingFencesFor(p stepParams) []string {
	recs, err := backup.LoadRecords()
	if err != nil {
		return nil
	}
	var out []string
	for db, f := range recs.Activation {
		if f.OperationID == "" || f.Phase != fencePhaseRouting {
			continue
		}
		if p.DB == "" || p.DB == db {
			out = append(out, db)
		}
	}
	sort.Strings(out)
	return out
}

// finalizeRoutingFences runs the verified finalizer for every routing fence
// an apply selected. A failure is returned as a warning and leaves the fence
// in routing; a successful scoped configuration apply alone never clears it.
func finalizeRoutingFences(ctx context.Context, env *Env, dbs []string) []output.Warning {
	var warnings []output.Warning
	for _, db := range dbs {
		recs, err := backup.LoadRecords()
		if err != nil {
			warnings = append(warnings, output.Warning{Code: "activation_routing_incomplete",
				Message: "the activation records could not be read, so " + db + "'s routing fence was not cleared: " + err.Error()})
			continue
		}
		f, ok := recs.ActivationFenceFor(db)
		if !ok || f.Phase != fencePhaseRouting {
			continue
		}
		spec, ok := env.Cluster.ClusterByName(db)
		if !ok {
			warnings = append(warnings, output.Warning{Code: "activation_routing_incomplete",
				Message: db + "'s routing fence has no current cluster.toml entry"})
			continue
		}
		if f.Kind == "fork" {
			// A normal engine pass may already have rendered shared poolers;
			// the finalizer still refuses publication/resume/clear until the
			// final actual registration and durable marker absence agree.
			if err := verifyForkRoutingBoundary(env.Cluster, db, f.OperationID); err != nil {
				warnings = append(warnings, output.Warning{Code: "activation_routing_incomplete",
					Message: db + " is not durably routable yet: " + err.Error()})
				continue
			}
		}
		var routingErr error
		if f.Kind == "restore-abort" {
			// A normal pooler render can skip on an unchanged input hash, so
			// an aborted restore always re-publishes through the strict fence.
			routingErr = completeRestoreAbortRouting(ctx, env, db, f.OperationID)
		} else {
			routingErr = verifyRoutingFence(ctx, env.Cluster, spec, f.OperationID, f.Kind)
		}
		if routingErr != nil {
			warnings = append(warnings, output.Warning{Code: "activation_routing_incomplete",
				Message: db + " is not routed yet: " + routingErr.Error()})
		}
	}
	return warnings
}

// resumeForkSettlement is the non-secret continuation of a fork whose data is
// already copied. It never replays credential renames or stores passwords. A
// verified continuation scope finishes registration, provisioning and
// settlement; a routing fence whose retained record is already durably absent
// only finalizes routing. Anything else is refused.
func resumeForkSettlement(ctx context.Context, env *Env, f backup.ActivationFence) (Result, error) {
	cfg := env.Cluster
	spec, ok := cfg.ClusterByName(f.Database)
	if !ok {
		return Result{}, databaseNotFound(f.Database)
	}
	if scope := forkContinuationScopeFrom(ctx); scope != nil && scope.hasOwner {
		return resumeForkContinuation(ctx, env, scope)
	}
	if f.Phase != fencePhaseRouting {
		// An adopting or adopted fence without a retained, verified
		// materialisation record has no proven physical ownership.
		return Result{}, materialisationIncomplete(f.Database, nil)
	}
	return resumeForkRoutingOnly(ctx, env, f, spec)
}

// copyPlanFromFence reconstructs the adopted fork's complete guard and
// prerequisites from its durable requirements and current intent. It never
// replays credentials: only non-secret configuration, SQL and routing remain.
func copyPlanFromFence(cfg config.Cluster, spec config.ClusterSpec, f backup.ActivationFence) (*extensionCopyPlan, error) {
	resolver := cfg.ExtensionResolver()
	currentNames, err := resolver.EffectiveNames(spec.Extensions)
	if err != nil {
		return nil, config.ValidationError(err)
	}
	current, err := resolveExtensions(resolver, currentNames)
	if err != nil {
		return nil, config.ValidationError(err)
	}
	// The settled guard must cover the durable copy snapshot, any retained
	// history of the destination's own stanza, and current destination intent.
	// MergeCopyRequirements resolves provider/settings conflicts and never
	// discards a conflicting historical variant by name.
	records, err := backup.LoadRecords()
	if err != nil {
		return nil, err
	}
	known := append([]catalog.Extension(nil), f.Requirements...)
	known = append(known, records.RetainedExtensions(f.TargetStanza)...)
	known = append(known, current...)
	merged, guard, err := backup.MergeCopyRequirements(known, nil, false, spec.DatabaseName())
	if err != nil {
		return nil, err
	}
	permanent := permanentExtensionSettings(cfg, spec, current)
	decision, err := backup.CopyGuardSettings(backup.CopyGuardSettingsOptions{
		Known: merged, Current: current, Permanent: permanent,
		Explicit: spec.Settings, CopyDB: spec.DatabaseName(), Pending: true,
	})
	if err != nil {
		return nil, err
	}
	guard.Settings = decision.Settings
	desiredPreloads, err := resolver.Preloads(spec.Extensions)
	if err != nil {
		return nil, config.ValidationError(err)
	}
	currentPreloads := map[string]bool{}
	for _, p := range desiredPreloads {
		currentPreloads[p] = true
	}
	extra := guard.Preloads[:0]
	for _, p := range guard.Preloads {
		if !currentPreloads[p] {
			extra = append(extra, p)
		}
	}
	guard.Preloads = extra
	cron := false
	reqs := make([]steps.ExtensionPrerequisite, 0, len(merged))
	for _, ext := range merged {
		if ext.Name == "pg_cron" || ext.ExtName == "pg_cron" {
			cron = true
		}
		if ext.ExtName == "" && ext.Preload == "" {
			continue
		}
		reqs = append(reqs, steps.ExtensionPrerequisite{
			Name: ext.Name, Source: ext.Source, Package: ext.Package,
			ExtName: ext.ExtName, Preload: ext.Preload,
		})
	}
	return &extensionCopyPlan{
		Plan: &backup.CopyPlan{
			Guard: guard, Requirements: merged, Stanza: f.TargetStanza,
			Cron: cron, DesiredPreloads: desiredPreloads,
			ValidateGuard: finalPreloadValidator(cfg, spec, spec.DatabaseName(), merged, permanent, false, nil),
		},
		Reqs: reqs, Merged: merged,
	}, nil
}

// forkStartOnly and forkStartJournal let a lifecycle test run the real fork
// start through engine.Apply with a selected real step set and a temporary
// journal. Production leaves them zero and runs the normal kernel, package and
// database steps.
var (
	forkStartOnly    []string
	forkStartJournal string
)

// forkStartCluster runs an adopted fork's owned kernel/package/configuration
// start. It is a host-effect seam so a lifecycle test can prove the non-secret
// continuation without a real PostgreSQL host; production uses the engine.
var forkStartCluster = func(ctx context.Context, cfg config.Cluster, rt engine.Runtime, spec config.ClusterSpec, report engine.Reporter) error {
	if len(forkStartOnly) > 0 {
		_, _, err := engine.Apply(ctx, steps.Catalog(), cfg, rt, engine.Options{
			Only: forkStartOnly, JournalPath: forkStartJournal,
			Restarter: steps.RestarterFor(cfg, rt, report), Report: report,
		})
		return err
	}
	others := otherConfigs(ctx, steps.Catalog(), cfg, rt, spec.Name, "")
	ctx = engine.WithNumbering(ctx, engine.NewNumbering(steps.Catalog(), cfg, createRuns(cfg, spec.Name, others)...))
	if _, _, err := engine.Apply(ctx, steps.Catalog(), cfg, rt, engine.Options{
		Only: steps.KernelStepIDs(), Restarter: steps.RestarterFor(cfg, rt, report), Report: report,
	}); err != nil {
		return err
	}
	if _, _, err := engine.Apply(ctx, steps.Catalog(), cfg, rt, engine.Options{
		Only: steps.PackagesStepIDs(), Restarter: steps.RestarterFor(cfg, rt, report), Report: report,
	}); err != nil {
		return err
	}
	return applyNewCluster(ctx, cfg, rt, spec.Name, "", report)
}

// completeForkActivation is the owned non-secret continuation of an adopted
// fork: persist the copy-disabling settings, install and verify the complete
// guard, run the owned configuration/start, reconcile desired SQL, verify cron
// and the effective handoff, then advance to routing and run the shared
// verified route finalizer. It never stores a password or replays a credential
// rename.
func completeForkActivation(ctx context.Context, spec config.ClusterSpec, f backup.ActivationFence, plan *extensionCopyPlan, rt engine.Runtime, report engine.Reporter) (config.Cluster, []output.Warning, error) {
	var warnings []output.Warning
	cfg, settingsWarnings, err := persistCopySettings(spec.Name, plan.Plan, spec.DatabaseName())
	if err != nil {
		return cfg, warnings, err
	}
	warnings = append(warnings, settingsWarnings...)
	spec, _ = cfg.ClusterByName(spec.Name)
	dataDir := cfg.Server.DataDirectory(spec.Name)
	ep := liveCopyGuardEndpoint(spec)
	if _, _, err := backup.InstallCopyPlanGuard(ep.ConfDir, dataDir, plan.Plan); err != nil {
		return cfg, warnings, err
	}
	restart := func() error {
		return backup.RestartCluster(ctx, rt.Shell, "apply", report, spec.Name, dataDir, spec.Port)
	}
	if clusterRunning(spec) {
		if _, err := verifyGuardEffective(ctx, ep, dataDir, restart); err != nil {
			return cfg, warnings, err
		}
	}
	if err := forkStartCluster(ctx, cfg, rt, spec, report); err != nil {
		return cfg, warnings, err
	}
	local := steps.ClusterPG(spec)
	if _, err := reconcileCopySQL(ctx, cfg, spec, local, spec.DatabaseName()); err != nil {
		return cfg, warnings, err
	}
	more, err := checkCopyExtensions(ctx, cfg, spec, plan.Plan, local, spec.DatabaseName())
	if err != nil {
		return cfg, warnings, err
	}
	warnings = append(warnings, more...)
	source := ""
	for old := range f.Renames {
		source = old
	}
	more, err = pruneCopyGuard(ctx, cfg, spec, dataDir, plan.Plan, true, spec.DatabaseName(), restart)
	if err != nil {
		return cfg, warnings, err
	}
	warnings = append(warnings, more...)
	// After prune: it rewrites cron.database_name for the adopted name (or
	// hands it back to the permanent configuration), so the check must see
	// the final effective value rather than the pre-rename guard.
	if plan.Plan.Cron {
		if err := checkCopyCronSetting(ctx, local, source, spec.DatabaseName()); err != nil {
			return cfg, warnings, err
		}
	}
	if err := advanceActivationFence(cfg, spec.Name, f.OperationID, "fork", fencePhaseAdopted, fencePhaseRouting, spec, func(nf *backup.ActivationFence) {
		nf.Registration = activationRegistration(spec)
		nf.Port = spec.Port
		nf.DataDir = dataDir
		nf.ConfDir = steps.PGConfDir(spec.Name)
	}); err != nil {
		return cfg, warnings, err
	}
	if _, _, err := verifyActivationFence(cfg, spec.Name, f.OperationID, "fork", fencePhaseRouting); err != nil {
		return cfg, warnings, err
	}
	// The physical owner (the job or the verified public continuation) must
	// verify and retire before either pooler can publish the target. Its caller
	// completes routing after retirement.
	if materialisationWithholdsRoutes(ctx, spec.Name) {
		return cfg, warnings, nil
	}
	if err := publishRouting(ctx, cfg, rt, report); err != nil {
		warnings = append(warnings, output.Warning{Code: "activation_routing_incomplete",
			Message: spec.Name + " is settled but its route was not published: " + err.Error()})
		return cfg, warnings, nil
	}
	if err := verifyRoutingFence(ctx, cfg, spec, f.OperationID, "fork"); err != nil {
		warnings = append(warnings, output.Warning{Code: "activation_routing_incomplete",
			Message: spec.Name + " is settled but not routed yet: " + err.Error()})
		return cfg, warnings, nil
	}
	return cfg, warnings, nil
}
