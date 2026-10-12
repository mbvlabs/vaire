package steps

import (
	"context"
	"os"
	"sort"

	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/render"
	"github.com/mbvlabs/eru/internal/versions"
)

const tlsID = "tls-v1"

// CertSyncUnitNames are the three units step 12 installs (D11).
var CertSyncUnitNames = []string{"eru-certs-sync.service", "eru-certs-sync.path", "eru-certs-sync.timer"}

type tlsStep struct{}

func (tlsStep) ID() string { return tlsID }

func (tlsStep) Describe(config.Cluster) string {
	return "install Caddy, place a placeholder certificate and install certs-sync"
}

func (tlsStep) RenderedFiles(cfg config.Cluster, _ engine.Runtime) []engine.RenderedFile {
	out := []engine.RenderedFile{{Path: CaddyfilePath, Content: render.Caddyfile(cfg)}}
	units := render.CertsSyncUnits()
	names := make([]string, 0, len(units))
	for name := range units {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out = append(out, engine.RenderedFile{Path: SystemdSystemDir + "/" + name, Content: []byte(units[name])})
	}
	return out
}

func (tlsStep) Inputs(cfg config.Cluster, _ engine.Runtime) ([]byte, error) {
	units := render.CertsSyncUnits()
	return canonical(map[string]any{
		"caddy_version": versions.CaddyVersion,
		"caddyfile":     string(render.Caddyfile(cfg)),
		"units":         units,
	})
}

func (tlsStep) Check(ctx context.Context, cfg config.Cluster, _ engine.Runtime) (engine.CheckResult, error) {
	if !fileIs(CaddyfilePath, string(render.Caddyfile(cfg)), 0o644) {
		return incomplete(CaddyfilePath + " differs"), nil
	}
	for name, content := range render.CertsSyncUnits() {
		if !fileIs(SystemdSystemDir+"/"+name, string(content), 0o644) {
			return incomplete(name + " differs"), nil
		}
	}
	if !serviceActive(ctx, CaddyService) {
		return incomplete("caddy is not active"), nil
	}
	if _, err := os.Stat(TLSCertPath); err != nil {
		return incomplete(TLSCertPath + " is missing"), nil
	}
	return complete(), nil
}

func (s tlsStep) Apply(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	if err := writeFile(CaddyfilePath, string(render.Caddyfile(cfg)), 0o644); err != nil {
		return engine.ApplyResult{}, err
	}
	for name, content := range render.CertsSyncUnits() {
		if err := writeFile(SystemdSystemDir+"/"+name, string(content), 0o644); err != nil {
			return engine.ApplyResult{}, err
		}
	}
	err := rt.Shell.Run(ctx, s.ID(), Script("tls"), map[string]string{
		"CADDY_VERSION": versions.CaddyVersion,
		"CADDY_SHA256":  versions.CaddySHA256[rt.Host.Arch],
		"DOMAIN":        cfg.Domain,
	}, report)
	return engine.ApplyResult{Restart: engine.RestartNone}, err
}
