package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

type jobLockRequest struct {
	name string
	mode engine.LockMode
}

func leaseJob(job string) bool {
	switch job {
	case "db-fork", "db-follow", "restore", "recover", "restore-drill":
		return true
	}
	return false
}

func validateJobLockParams(env *jobEnv) error {
	if !leaseJob(env.Job) {
		return nil
	}
	var p struct {
		Database string `json:"database"`
		Source   string `json:"source"`
		Name     string `json:"name"`
	}
	if len(env.Params) != 0 {
		if err := json.Unmarshal(env.Params, &p); err != nil {
			return err
		}
	}
	valid := func(name string) bool { return config.DatabasePattern.MatchString(name) }
	switch env.Job {
	case "db-fork", "db-follow":
		if !valid(p.Source) || !valid(p.Name) {
			return errors.New("invalid job source or target")
		}
		if _, ok := env.Cluster.ClusterByName(p.Source); !ok {
			return fmt.Errorf("job source %s is not configured", p.Source)
		}
	case "restore", "recover":
		if !valid(p.Database) {
			return errors.New("invalid job database")
		}
		if _, ok := env.Cluster.ClusterByName(p.Database); !ok {
			return fmt.Errorf("job database %s is not configured", p.Database)
		}
	case "restore-drill":
		if !valid(env.DB) {
			return errors.New("invalid job database")
		}
		if _, ok := env.Cluster.ClusterByName(env.DB); !ok {
			return fmt.Errorf("job database %s is not configured", env.DB)
		}
	}
	return nil
}

func jobLockRequests(env *jobEnv) []jobLockRequest {
	byName := make(map[string]engine.LockMode)
	add := func(name string, mode engine.LockMode) {
		if name != "" && (byName[name] == "" || mode == engine.LockExclusive) {
			byName[name] = mode
		}
	}
	for _, name := range jobClusters(env) {
		mode := jobLockMode(env)
		if env.Job == "db-fork" {
			mode = engine.LockShared
		}
		add(name, mode)
	}
	var p struct {
		Database string `json:"database"`
		Name     string `json:"name"`
	}
	_ = json.Unmarshal(env.Params, &p)
	switch env.Job {
	case "db-fork", "db-follow":
		add(p.Name, engine.LockExclusive)
	case "restore", "recover":
		add(backup.RestoringName(p.Database), engine.LockExclusive)
	case "restore-drill":
		add(backup.DrillName(env.DB), engine.LockExclusive)
	}
	out := make([]jobLockRequest, 0, len(byName))
	for name, mode := range byName {
		out = append(out, jobLockRequest{name, mode})
	}
	slices.SortFunc(out, func(a, b jobLockRequest) int {
		if a.name < b.name {
			return -1
		}
		if a.name > b.name {
			return 1
		}
		return 0
	})
	return out
}

// lockedJobLease is issued only after every OS lock was acquired. Registry
// membership is pointer identity, never the copyable contents of this struct.
type lockedJobLease struct {
	mu                           sync.RWMutex
	once                         sync.Once
	id, kind, operation, install string
	env                          *jobEnv
	params, db, target           string
	targets                      map[string]engine.LockMode
	heavy                        engine.Release
	release                      engine.Release
}

var lockedJobs = struct {
	sync.Mutex
	active map[*lockedJobLease]struct{}
}{active: make(map[*lockedJobLease]struct{})}

func newLockedJobLease(env *jobEnv, requests []jobLockRequest, heavy, release engine.Release) *lockedJobLease {
	l := &lockedJobLease{id: env.ID, kind: env.Job, operation: env.OperationID, install: env.installID,
		env: env, params: string(env.Params), db: env.DB, target: jobLeaseTarget(env),
		targets: make(map[string]engine.LockMode), heavy: heavy, release: release}
	for _, r := range requests {
		l.targets[r.name] = r.mode
	}
	lockedJobs.Lock()
	lockedJobs.active[l] = struct{}{}
	lockedJobs.Unlock()
	return l
}

// withLockedJobTarget holds the lease alive throughout action. Identity and
// target must match the actual runner and an exclusively held cluster lock.
func withLockedJobTarget(env *jobEnv, target string, action func() error) error {
	if env == nil || env.lease == nil || action == nil {
		return errors.New("locked job authority required")
	}
	l := env.lease
	l.mu.RLock()
	defer l.mu.RUnlock()
	lockedJobs.Lock()
	_, active := lockedJobs.active[l]
	lockedJobs.Unlock()
	if !active || l.env != env || env.ID == "" || env.OperationID == "" || env.installID == "" ||
		l.id != env.ID || l.kind != env.Job || l.operation != env.OperationID || l.install != env.installID ||
		l.params != string(env.Params) || l.db != env.DB || l.target != target ||
		target == "" || l.targets[target] != engine.LockExclusive {
		return errors.New("locked job authority does not match target")
	}
	return action()
}

func jobLeaseTarget(env *jobEnv) string {
	var p struct {
		Database string `json:"database"`
		Name     string `json:"name"`
	}
	_ = json.Unmarshal(env.Params, &p)
	switch env.Job {
	case "db-fork", "db-follow":
		return p.Name
	case "restore", "recover":
		return backup.RestoringName(p.Database)
	case "restore-drill":
		return backup.DrillName(env.DB)
	}
	return ""
}

func (l *lockedJobLease) releaseHeavyIO() {
	if l.heavy != nil {
		l.heavy()
	}
}

func (l *lockedJobLease) finish() {
	l.once.Do(func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		lockedJobs.Lock()
		delete(lockedJobs.active, l)
		lockedJobs.Unlock()
		l.release()
	})
}
