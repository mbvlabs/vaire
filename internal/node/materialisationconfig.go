package node

import (
	"fmt"
	"strings"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/render"
	"github.com/mbvlabs/vaire/internal/steps"
)

var materialisationHostInfo = hostinfo.Detect

// materialisationConfig renders the intended managed layers, not a Go
// approximation of PostgreSQL's final settings. The materialiser asks
// postgres -C after the real temporary configuration is registered.
func materialisationConfig(cfg config.Cluster, spec config.ClusterSpec, host hostinfo.Info) (*backup.MaterialiseConfig, error) {
	out := &backup.MaterialiseConfig{
		Tuning:   string(render.TuningConf(steps.TuningFor(cfg, spec, host))),
		Settings: string(render.SettingsConf(cfg, spec)),
	}
	if spec.Follows == "" {
		return out, nil
	}
	src, ok := cfg.ClusterByName(spec.Follows)
	if !ok {
		return nil, fmt.Errorf("cannot resolve follower source for %s", spec.Name)
	}
	floors, err := steps.FollowerFloors(cfg, spec, src, host)
	if err != nil {
		return nil, err
	}
	var conf strings.Builder
	conf.WriteString("# managed recovery floors\n")
	for _, floor := range floors {
		fmt.Fprintf(&conf, "%s = %s\n", floor.Name, floor.Value)
	}
	follower := conf.String()
	out.Follower = &follower
	return out, nil
}

func prepareMaterialisationConfig(cfg config.Cluster, spec config.ClusterSpec) (*backup.MaterialiseConfig, error) {
	host, _ := materialisationHostInfo(cfg.Server.DataRoot)
	return materialisationConfig(cfg, spec, host)
}
