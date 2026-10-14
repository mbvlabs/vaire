package node

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
)

// UnprotectToken is the confirm token that lifts clusters.<db>.protected.
func UnprotectToken(db string) string { return db + "/unprotect" }

// CheckNotProtected refuses action on database name while cluster.toml marks
// it protected. A confirm token never overrides it: the protection has to be
// lifted first, by a human.
func CheckNotProtected(cfg config.Cluster, name, action string) error {
	spec, ok := cfg.ClusterByName(name)
	if !ok || !spec.Protected {
		return nil
	}
	return protectedError(name, action)
}

func protectedError(name, action string) error {
	return output.NewError(output.CodeDatabaseProtected,
		"database "+name+" is protected, so "+action+" is refused", output.ExitUnsafe,
		"A human lifts the protection first, at a terminal: eru config set clusters."+name+".protected false (it asks for "+UnprotectToken(name)+"). A confirm token does not override it.").
		WithDetails(map[string]any{"reason": "database_protected", "database": name, "action": action})
}

// refuseProtected wraps a method handler so it refuses before doing anything
// when the database it names (params[field]) is protected and applies says
// the request is one protection forbids. The dispatcher has loaded
// cluster.toml into env.Cluster (the method sets cluster). A nil applies
// covers every request.
func refuseProtected(action, field string, applies func(json.RawMessage) bool,
	handler func(context.Context, *Env) (Result, error)) func(context.Context, *Env) (Result, error) {
	return func(ctx context.Context, env *Env) (Result, error) {
		if applies == nil || applies(env.Req.Params) {
			var params map[string]any
			_ = json.Unmarshal(env.Req.Params, &params)
			if name, _ := params[field].(string); name != "" {
				if err := CheckNotProtected(env.Cluster, name, action); err != nil {
					return Result{}, err
				}
			}
		}
		return handler(ctx, env)
	}
}

// restorePromotes reports whether a restore.start request promotes at once.
func restorePromotes(params json.RawMessage) bool {
	var p struct {
		Promote bool `json:"promote"`
	}
	return json.Unmarshal(params, &p) == nil && p.Promote
}

// Unprotected lists the databases that are protected in old and are not in
// updated: the protection is lifted, or the entry is gone.
func Unprotected(old, updated config.Cluster) []string {
	var out []string
	for _, c := range old.Clusters {
		if !c.Protected {
			continue
		}
		if u, ok := updated.ClusterByName(c.Name); !ok || !u.Protected {
			out = append(out, c.Name)
		}
	}
	sort.Strings(out)
	return out
}

// checkUnprotect refuses a config.set that lifts a database's protection (or
// removes a protected entry) unless the request carries <db>/unprotect for
// each one. The operator only sends it after a human typed it at a terminal.
func checkUnprotect(old, updated config.Cluster, tokens []string) error {
	have := map[string]bool{}
	for _, t := range tokens {
		have[t] = true
	}
	var missing []string
	for _, name := range Unprotected(old, updated) {
		if !have[UnprotectToken(name)] {
			missing = append(missing, UnprotectToken(name))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return output.NewError(output.CodeDatabaseProtected,
		"lifting the protection of a database needs "+strings.Join(missing, ", "), output.ExitUnsafe,
		"A human runs eru config set clusters.<db>.protected false at a terminal and types the token. Agents and scripts cannot lift it.").
		WithDetails(map[string]any{"reason": "unprotect_requires_confirmation", "confirm_tokens": missing})
}

// ResizeShrinks reports whether resized has less memory than old.
func ResizeShrinks(cfg config.Cluster, old, resized config.ClusterSpec) bool {
	before, err := cfg.Resources(old)
	if err != nil {
		return false
	}
	after, err := cfg.Resources(resized)
	if err != nil {
		return false
	}
	return after.MemoryMiB < before.MemoryMiB
}
