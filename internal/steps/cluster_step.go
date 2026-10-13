package steps

import (
	"fmt"

	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
)

// clusterStep is what a per-cluster step's instance knows: its cluster. The
// zero value is the template in the catalogue (RFC 11.2).
type clusterStep struct {
	cluster string
}

// id is the template's ID, or the instance's <id>@<cluster>.
func (c clusterStep) id(base string) string {
	if c.cluster == "" {
		return base
	}
	return engine.InstanceID(base, c.cluster)
}

// spec is the instance's [[clusters]] entry.
func (c clusterStep) spec(cfg config.Cluster) (config.ClusterSpec, error) {
	if c.cluster == "" {
		return config.ClusterSpec{}, fmt.Errorf("a per-cluster step ran without a cluster; this is an eru bug")
	}
	spec, ok := cfg.ClusterByName(c.cluster)
	if !ok {
		return config.ClusterSpec{}, fmt.Errorf("cluster %s is not in cluster.toml", c.cluster)
	}
	return spec, nil
}
