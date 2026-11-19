package node

import (
	"slices"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/steps"
)

func checkExtensionPackageChange(old, next config.Cluster, trusted bool) error {
	if trusted {
		return nil
	}

	var introduced []steps.ExtensionPrerequisite

	for name, def := range next.Extensions {
		if def.Package == "" || def.Package == old.Extensions[name].Package {
			continue
		}

		ext, err := next.ExtensionResolver().Resolve(name)
		if err != nil {
			return err
		}

		introduced = append(introduced, steps.ExtensionPrerequisite{Name: ext.Name, Source: ext.Source, Package: ext.Package})
	}

	for _, spec := range slices.Concat(next.Clusters, next.Awaiting) {
		before, err := packageSelection(old, spec.Name)
		if err != nil {
			return err
		}

		after, err := packageSelection(next, spec.Name)
		if err != nil {
			return err
		}

		for _, req := range after {
			if !slices.Contains(before, req) {
				introduced = append(introduced, req)
			}
		}
	}

	return steps.CheckExtensionPackageAuthority(introduced, false)
}

func packageSelection(cfg config.Cluster, name string) ([]steps.ExtensionPrerequisite, error) {
	for _, spec := range slices.Concat(cfg.Clusters, cfg.Awaiting) {
		if spec.Name == name {
			cfg.Clusters = []config.ClusterSpec{spec}
			cfg.Awaiting = nil

			return steps.ExtensionPrerequisites(cfg)
		}
	}

	return nil, nil
}
