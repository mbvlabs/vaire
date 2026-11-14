package node

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/mbvlabs/vaire/internal/backup"
)

// Authority is issued only to the live runner holding the exclusive target
// lease. Records and copied capabilities are not authority.
type forkMaterialisationAuthority struct {
	mu                                                                          sync.Mutex
	env                                                                         *jobEnv
	capability                                                                  *materialisationCapability
	artifacts                                                                   *materialisationArtifacts
	m                                                                           *backup.Materialised
	sourcePort                                                                  int
	target                                                                      string
	active, adopted, dropped, populated, registered, started, promoted, renamed bool
}

var forkCapabilities = struct {
	sync.Mutex
	active map[*materialisationCapability]*forkMaterialisationAuthority
	envs   map[*jobEnv]bool
}{active: make(map[*materialisationCapability]*forkMaterialisationAuthority), envs: make(map[*jobEnv]bool)}

var forkReservationLock = backup.LockForkReservation

func newForkMaterialisationAuthority(env *jobEnv, target string) (*forkMaterialisationAuthority, error) {
	var result *forkMaterialisationAuthority
	err := withLockedJobTarget(env, target, func() error {
		if env.Job != "db-fork" {
			return errors.New("fork job required")
		}
		var p dbForkParams
		if err := json.Unmarshal(env.Params, &p); err != nil {
			return err
		}
		source, ok := env.Cluster.ClusterByName(p.Source)
		if !ok {
			return errors.New("fork source missing")
		}
		dest, ok := env.Cluster.ClusterByName(p.Name)
		if !ok || p.Name != target {
			return errors.New("fork target missing")
		}
		uid, err := materialisationPostgresUID()
		if err != nil {
			return err
		}
		a, err := openMaterialisationArtifacts(target, env.Cluster.Server.DataRoot, uid)
		if err != nil {
			return err
		}
		o := a.owner
		if o.Kind != materialisationFork || o.Source != p.Source || o.Target != p.Name ||
			o.Job != env.ID || o.Operation != env.OperationID || o.Instance != env.installID ||
			o.Stanza != source.Stanza || o.IntendedPort != dest.Port || o.Phase != materialisationPrepared ||
			o.ArtifactStage != "" || o.TemporaryPort != 0 || source.Port < 1 {
			a.close()
			return errors.New("fork ownership does not match locked job")
		}
		capability := &materialisationCapability{owner: o}
		result = &forkMaterialisationAuthority{env: env, capability: capability, artifacts: a, sourcePort: source.Port, target: target, active: true}
		forkCapabilities.Lock()
		if forkCapabilities.envs[env] {
			forkCapabilities.Unlock()
			a.close()
			result = nil
			return errors.New("fork authority already issued")
		}
		forkCapabilities.active[capability] = result
		forkCapabilities.envs[env] = true
		forkCapabilities.Unlock()
		return nil
	})
	return result, err
}

func (a *forkMaterialisationAuthority) run(ctx context.Context, fn func() error) error {
	if a == nil || ctx == nil || fn == nil {
		return errors.New("missing fork authority")
	}
	return withLockedJobTarget(a.env, a.target, func() error {
		a.mu.Lock()
		defer a.mu.Unlock()
		forkCapabilities.Lock()
		registered := forkCapabilities.active[a.capability] == a
		forkCapabilities.Unlock()
		if !a.active || !registered || a.capability.owner.Job != a.env.ID || ctx.Err() != nil {
			return errors.New("fork authority inactive")
		}
		return fn()
	})
}

type forkViewKey struct{}
type forkView struct {
	capability *materialisationCapability
	target     string
	active     atomic.Bool
}

func hasForkMaterialisationView(ctx context.Context, target string) bool {
	if ctx == nil {
		return false
	}
	v, ok := ctx.Value(forkViewKey{}).(*forkView)
	if !ok || v == nil || !v.active.Load() || v.target != target {
		return false
	}
	forkCapabilities.Lock()
	a := forkCapabilities.active[v.capability]
	forkCapabilities.Unlock()
	if a == nil || a.target != target {
		return false
	}
	lockedJobs.Lock()
	_, active := lockedJobs.active[a.env.lease]
	lockedJobs.Unlock()
	return active && v.active.Load()
}

func (a *forkMaterialisationAuthority) withProvisionedForkTarget(ctx context.Context, effect func(context.Context) error) error {
	if effect == nil {
		return errors.New("missing provisioning action")
	}
	return a.run(ctx, func() error {
		if !a.adopted || a.dropped || a.artifacts.owner.Phase != materialisationProvisioning ||
			a.artifacts.owner.ArtifactStage != materialisationRegistered {
			return errors.New("fork provisioning not authorized")
		}
		if err := errors.Join(a.artifacts.validate(), a.reservation(ctx, false)); err != nil {
			return err
		}
		v := &forkView{capability: a.capability, target: a.target}
		v.active.Store(true)
		defer v.active.Store(false)
		err := effect(context.WithValue(ctx, forkViewKey{}, v))
		return errors.Join(err, a.artifacts.validate(), a.reservation(ctx, false))
	})
}

func (a *forkMaterialisationAuthority) markProvisioned(ctx context.Context, verify func() error) error {
	return a.run(ctx, func() error {
		if !a.adopted || a.dropped || a.artifacts.owner.Phase != materialisationProvisioning {
			return errors.New("fork provisioning verification not authorized")
		}
		if err := a.reservation(ctx, false); err != nil {
			return err
		}
		return a.artifacts.markMaterialisationProvisioned(verify)
	})
}

func (a *forkMaterialisationAuthority) retire(ctx context.Context) error {
	return a.run(ctx, func() error {
		if !a.adopted || a.dropped {
			return errors.New("fork retirement not authorized")
		}
		return a.artifacts.retireProvisioned(func(o materialisationOwnership) (func(), error) {
			return forkReservationLock(ctx, o.Target, o.TemporaryPort, false)
		})
	})
}

func (a *forkMaterialisationAuthority) retireDropped(ctx context.Context) error {
	return a.run(ctx, func() error {
		if !a.dropped || a.adopted {
			return errors.New("fork cleanup retirement not authorized")
		}
		return a.artifacts.retireMaterialisation(func(o materialisationOwnership) (func(), error) {
			return forkReservationLock(ctx, o.Target, o.TemporaryPort, false)
		}, a.artifacts.validateCleanupCompleted)
	})
}

func (a *forkMaterialisationAuthority) pathsMatch(p backup.MaterialisationPaths) bool {
	o := a.artifacts.owner
	return p.Name == o.Target && p.Source == o.Source && p.DataDir == o.DataPath &&
		p.ConfigDir == o.RegisteredPath && p.Port == o.TemporaryPort && p.SourcePort == a.sourcePort
}

func (a *forkMaterialisationAuthority) reservation(ctx context.Context, present bool) error {
	if present {
		if a.m == nil {
			return errors.New("fork reservation capability missing")
		}
		return a.m.WithForkReservation(ctx, func() error { return nil })
	}
	o := a.artifacts.owner
	unlock, err := forkReservationLock(ctx, o.Target, o.TemporaryPort, false)
	if err != nil {
		return err
	}
	unlock()
	return nil
}

func (a *forkMaterialisationAuthority) bindReservation(ctx context.Context, m *backup.Materialised) error {
	return a.run(ctx, func() error {
		p := m.OwnershipPaths()
		o := a.artifacts.owner
		if p.Name != o.Target || p.Source != o.Source || p.DataDir != o.DataPath || p.ConfigDir != o.RegisteredPath || p.SourcePort != a.sourcePort {
			return errors.New("fork reservation paths mismatch")
		}
		id, err := m.ForkReservationIdentity()
		if err != nil {
			return err
		}
		err = a.artifacts.bindMaterialisationReservation(p.Port, func(_ materialisationOwnership, _ int, persist func() error) error {
			return m.WithForkReservation(ctx, persist)
		}, id)
		if err == nil {
			a.m = m
		}
		return err
	})
}

func (a *forkMaterialisationAuthority) create(ctx context.Context, p backup.MaterialisationPaths, configDir bool) error {
	return a.run(ctx, func() error {
		if !a.pathsMatch(p) || a.adopted || a.dropped || a.artifacts.owner.Phase != materialisationCopying || configDir && !a.populated {
			return errors.New("fork creation not authorized")
		}
		if err := a.reservation(ctx, true); err != nil {
			return err
		}
		err := a.artifacts.bindCreated(configDir)
		return errors.Join(err, a.artifacts.validate(), a.reservation(ctx, true))
	})
}
func (a *forkMaterialisationAuthority) CreateData(ctx context.Context, p backup.MaterialisationPaths) error {
	return a.create(ctx, p, false)
}
func (a *forkMaterialisationAuthority) CreateConfig(ctx context.Context, p backup.MaterialisationPaths) error {
	return a.create(ctx, p, true)
}

func forkActionAllowed(action backup.MaterialisationAction, stage string) bool {
	switch action {
	case backup.MaterialisationRestore, backup.MaterialisationBaseBackup, backup.MaterialisationInitdb:
		return stage == materialisationDataCreated
	case backup.MaterialisationDrop:
		return stage == "" || stage == materialisationDataCreated || stage == materialisationRegistered
	case backup.MaterialisationRegister, backup.MaterialisationPrepare, backup.MaterialisationInspect, backup.MaterialisationStart,
		backup.MaterialisationStop, backup.MaterialisationPromote, backup.MaterialisationRename, backup.MaterialisationAdopt, backup.MaterialisationCopyRoles:
		return stage == materialisationRegistered
	}
	return false
}

func (a *forkMaterialisationAuthority) actionReady(action backup.MaterialisationAction) bool {
	switch action {
	case backup.MaterialisationRegister:
		return a.populated && !a.registered
	case backup.MaterialisationPrepare, backup.MaterialisationInspect, backup.MaterialisationStart:
		return a.registered && !a.started
	case backup.MaterialisationPromote:
		return a.started && !a.promoted
	case backup.MaterialisationRename:
		return a.promoted && !a.renamed
	case backup.MaterialisationAdopt:
		return a.renamed
	case backup.MaterialisationStop, backup.MaterialisationCopyRoles:
		return a.started
	}
	return true
}

func (a *forkMaterialisationAuthority) Guard(ctx context.Context, p backup.MaterialisationPaths, action backup.MaterialisationAction, effect func() error) error {
	if effect == nil {
		return errors.New("missing fork action")
	}
	return a.run(ctx, func() error {
		if !a.pathsMatch(p) || a.adopted || a.dropped || a.artifacts.owner.Phase != materialisationCopying ||
			!forkActionAllowed(action, a.artifacts.owner.ArtifactStage) || !a.actionReady(action) ||
			a.populated && (action == backup.MaterialisationRestore || action == backup.MaterialisationBaseBackup || action == backup.MaterialisationInitdb) {
			return errors.New("fork action not authorized")
		}
		if err := a.artifacts.validate(); err != nil {
			return err
		}
		if err := a.reservation(ctx, true); err != nil {
			return err
		}
		err := effect()
		if action == backup.MaterialisationDrop && err == nil {
			post := errors.Join(a.artifacts.validateCleanupCompleted(), a.reservation(ctx, false))
			if post == nil {
				a.dropped = true
			}
			return post
		}
		post := a.artifacts.validate()
		if action == backup.MaterialisationAdopt && err == nil {
			post = errors.Join(post, a.reservation(ctx, false))
			if post == nil {
				a.adopted = true
			}
		} else {
			post = errors.Join(post, a.reservation(ctx, true))
		}
		if err == nil && post == nil {
			switch action {
			case backup.MaterialisationRestore, backup.MaterialisationBaseBackup, backup.MaterialisationInitdb:
				a.populated = true
			case backup.MaterialisationRegister:
				a.registered = true
			case backup.MaterialisationStart:
				a.started = true
			case backup.MaterialisationPromote:
				a.promoted = true
			case backup.MaterialisationRename:
				a.renamed = true
			}
		}
		return errors.Join(err, post)
	})
}

func (a *forkMaterialisationAuthority) advance(ctx context.Context, phase, reason string) error {
	return a.run(ctx, func() error {
		if phase == materialisationCopying && a.artifacts.owner.TemporaryPort == 0 || phase == materialisationProvisioning && !a.adopted {
			return errors.New("fork phase transition not authorized")
		}
		next := a.artifacts.owner
		next.Phase, next.Reason = phase, reason
		return a.artifacts.persistMaterialisation(next)
	})
}

func (a *forkMaterialisationAuthority) Close() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.active {
		return
	}
	a.active = false
	forkCapabilities.Lock()
	delete(forkCapabilities.active, a.capability)
	delete(forkCapabilities.envs, a.env)
	forkCapabilities.Unlock()
	if a.m != nil {
		a.m.ReleaseForkReservationAuthority()
	}
	a.artifacts.close()
}

var _ backup.MaterialisationOwnership = (*forkMaterialisationAuthority)(nil)
