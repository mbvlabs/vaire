package node

import (
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/rpc"
)

func memberRead(env *Env) bool {
	return env != nil && env.DeviceIdentity != nil && env.DeviceIdentity.Role == config.AccessRoleMember
}

func memberOwns(env *Env, name string) bool {
	return memberRead(env) && env.DeviceIdentity.IdentityKind == rpc.IdentityVerifiedDevice &&
		env.Cluster.DatabaseCreatedByOperator(name, env.DeviceIdentity.OperatorID)
}

func memberDatabases(env *Env) config.Cluster {
	visible := env.Cluster
	visible.Clusters = nil

	for _, spec := range env.Cluster.Clusters {
		if memberOwns(env, spec.Name) {
			visible.Clusters = append(visible.Clusters, spec)
		}
	}

	return visible
}

// memberConfig is the minimum desired state needed for client-side sizing
// and connection URL construction. Placement is decided by the node.
func memberConfig(env *Env, cfg config.Cluster) config.Cluster {
	out := config.Cluster{
		Schema: cfg.Schema, ClusterName: cfg.ClusterName, Domain: cfg.Domain,
		TLS:      config.TLS{Mode: cfg.TLS.Mode},
		Postgres: config.Postgres{DefaultSize: cfg.Postgres.DefaultSize},
	}
	for _, spec := range cfg.Clusters {
		if memberOwns(env, spec.Name) {
			out.Clusters = append(out.Clusters, config.ClusterSpec{Name: spec.Name, Node: spec.Node})
		}
	}

	for _, n := range cfg.Nodes {
		if n.Role == config.RoleMaster {
			out.Nodes = append(out.Nodes, config.NodeSpec{Name: n.Name, Role: n.Role})
		}
	}

	return out
}
