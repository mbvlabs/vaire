package render

import (
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
)

func TestNginxDirectTLSUsesDatabaseAllowlist(t *testing.T) {
	cfg := config.DefaultCluster()

	const entry = "203.0.113.7/32 1;"

	render := func() string {
		return string(NginxConf(cfg, "db1.example.test", "/var/www/acme"))
	}

	if strings.Contains(render(), entry) {
		t.Fatal("default nginx policy admitted the public client")
	}

	cfg.Network.ClientAllowlist = []string{"203.0.113.7/32"}

	if !strings.Contains(render(), entry) {
		t.Fatal("nginx direct-TLS policy omitted the allowlisted client")
	}
}
