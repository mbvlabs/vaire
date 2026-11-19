package render

import (
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
)

func TestNginxHTTPSOnly(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Network.ClientAllowlist = []string{"203.0.113.7/32"}

	conf := string(NginxConf(cfg, "db1.example.test", "/var/www/acme"))
	for _, want := range []string{"listen 443 ssl default_server;", "listen [::]:443 ssl default_server;", "listen 80 default_server;", "root /var/www/acme;", "server_name db1.example.test;"} {
		if !strings.Contains(conf, want) {
			t.Errorf("nginx config lacks %q", want)
		}
	}

	for _, unwanted := range []string{"stream {", "ssl_preread", "proxy_protocol", "real_ip_header", "6432", "6433", "6434", "6435", "8443", "203.0.113.7"} {
		if strings.Contains(conf, unwanted) {
			t.Errorf("nginx config contains %q", unwanted)
		}
	}
}
