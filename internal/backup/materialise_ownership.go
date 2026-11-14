package backup

import "context"

// MaterialisationPaths identifies a temporary cluster without credentials.
// Port is its currently reserved temporary port, before adoption.
type MaterialisationPaths struct {
	Name       string
	Source     string
	DataDir    string
	ConfigDir  string
	Port       int
	SourcePort int
}

// MaterialisationAction identifies an ownership-guarded effect.
type MaterialisationAction string

const (
	MaterialisationRestore    MaterialisationAction = "restore"
	MaterialisationBaseBackup MaterialisationAction = "base_backup"
	MaterialisationInitdb     MaterialisationAction = "initdb"
	MaterialisationRegister   MaterialisationAction = "register"
	MaterialisationPrepare    MaterialisationAction = "prepare"
	MaterialisationInspect    MaterialisationAction = "inspect"
	MaterialisationStart      MaterialisationAction = "start"
	MaterialisationStop       MaterialisationAction = "stop"
	MaterialisationPromote    MaterialisationAction = "promote"
	MaterialisationRename     MaterialisationAction = "rename"
	MaterialisationAdopt      MaterialisationAction = "adopt"
	MaterialisationDrop       MaterialisationAction = "drop"
	MaterialisationCopyRoles  MaterialisationAction = "copy_roles"
)

// MaterialisationOwnership is supplied by the node's exact leased operation.
// CreateData and CreateConfig must exclusively create the named directory,
// establish postgres ownership and mode (0700 for data, 0755 for config),
// and durably bind its device/inode before returning. They must refuse
// existing or unverifiable paths and must never rebind a replacement
// directory. CreateConfig also checks the already-bound data directory and
// follows successful data population; neither create is called inside Guard.
//
// Guard must verify the active operation and bound path identities before
// calling effect exactly once, and verify them again even if effect fails.
// A failed post-check must be returned, joined with any effect error. Drop and
// Adopt may intentionally change lifecycle state; the implementation must
// validate the action-specific transition rather than rebind paths, and
// reject unknown or unauthorized actions. It must not invoke another guarded
// Materialised operation from effect.
type MaterialisationOwnership interface {
	CreateData(context.Context, MaterialisationPaths) error
	CreateConfig(context.Context, MaterialisationPaths) error
	Guard(context.Context, MaterialisationPaths, MaterialisationAction, func() error) error
}

// OwnershipPaths returns deterministic paths and the reserved port so the
// node can persist an operation record before starting materialisation.
func (m *Materialised) OwnershipPaths() MaterialisationPaths {
	return MaterialisationPaths{
		Name: m.Name, Source: m.Source, DataDir: m.Data, ConfigDir: m.ConfDir(),
		Port: m.Port, SourcePort: m.SourcePort,
	}
}

// GuardOwnership lets the node guard an exact-target operation outside this
// package, such as its fork-role rename. The callback must not call another
// guarded Materialised operation.
func (m *Materialised) GuardOwnership(ctx context.Context, action MaterialisationAction, effect func() error) error {
	if m.Ownership == nil {
		return effect()
	}
	return m.Ownership.Guard(ctx, m.OwnershipPaths(), action, effect)
}

func (m *Materialised) createOwnedData(ctx context.Context) error {
	if m.Ownership == nil {
		return m.makeData()
	}
	return m.Ownership.CreateData(ctx, m.OwnershipPaths())
}

func (m *Materialised) registerOwned(ctx context.Context) error {
	if m.Ownership != nil {
		if err := m.Ownership.CreateConfig(ctx, m.OwnershipPaths()); err != nil {
			return err
		}
	}
	return m.GuardOwnership(ctx, MaterialisationRegister, m.register)
}
