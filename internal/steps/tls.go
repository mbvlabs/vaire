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

const tlsID = "tls-v2"

// CertsUnitNames are the units step 12 installs (D11).
var CertsUnitNames = []string{"eru-certs.service", "eru-certs.timer"}

type tlsStep struct{}

func (tlsStep) ID() string { return tlsID }

func (tlsStep) Describe(config.Cluster) string {
	return "install nginx, place a placeholder certificate and start certificate issuance"
}

func nginxConf(cfg config.Cluster) []byte { return render.NginxConf(cfg, ACMEWebroot) }

func (tlsStep) RenderedFiles(cfg config.Cluster, _ engine.Runtime) []engine.RenderedFile {
	out := []engine.RenderedFile{{Path: NginxConfPath, Content: nginxConf(cfg)}}
	units := render.CertsUnits()
	names := make([]string, 0, len(units))
	for name := range units {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out = append(out, engine.RenderedFile{Path: SystemdSystemDir + "/" + name, Content: units[name]})
	}
	return out
}

func (tlsStep) Inputs(cfg config.Cluster, _ engine.Runtime) ([]byte, error) {
	units := render.CertsUnits()
	return canonical(map[string]any{
		"nginx_min_version": versions.NginxMinVersion,
		"nginx_conf":        string(nginxConf(cfg)),
		"units":             units,
		"tls_mode":          cfg.TLS.Mode,
		"acme_ca":           cfg.TLS.ACMECA,
		"acme_email":        cfg.ACMEEmail,
	})
}

func (tlsStep) Check(ctx context.Context, cfg config.Cluster, _ engine.Runtime) (engine.CheckResult, error) {
	if !fileIs(NginxConfPath, string(nginxConf(cfg)), 0o644) {
		return incomplete(NginxConfPath + " differs"), nil
	}
	for name, content := range render.CertsUnits() {
		if !fileIs(SystemdSystemDir+"/"+name, string(content), 0o644) {
			return incomplete(name + " differs"), nil
		}
	}
	if !serviceActive(ctx, NginxService) {
		return incomplete("nginx is not active"), nil
	}
	if _, err := os.Stat(TLSCertPath); err != nil {
		return incomplete(TLSCertPath + " is missing"), nil
	}
	return complete(), nil
}

func (s tlsStep) Apply(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	// nginx and its distribution config are installed first, so eru's
	// nginx.conf is written over the package's default afterwards.
	env := map[string]string{
		"NGINX_MIN_VERSION": versions.NginxMinVersion,
		"DOMAIN":            cfg.Domain,
		"ACME_WEBROOT":      ACMEWebroot,
		"PHASE":             "install",
	}
	if err := rt.Shell.Run(ctx, s.ID(), Script("tls"), env, report); err != nil {
		return engine.ApplyResult{}, err
	}
	if err := writeFile(NginxConfPath, string(nginxConf(cfg)), 0o644); err != nil {
		return engine.ApplyResult{}, err
	}
	for name, content := range render.CertsUnits() {
		if err := writeFile(SystemdSystemDir+"/"+name, string(content), 0o644); err != nil {
			return engine.ApplyResult{}, err
		}
	}
	env["PHASE"] = "start"
	err := rt.Shell.Run(ctx, s.ID(), Script("tls"), env, report)
	return engine.ApplyResult{Restart: engine.RestartNone}, err
}
