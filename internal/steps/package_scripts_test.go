package steps

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// dpkgAnswer is one package's fake dpkg-query answer. A package absent from the
// fixture is unknown to dpkg, like one that was never installed.
type dpkgAnswer struct {
	status string
	stderr string
	exit   int
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// runPackageScript runs an embedded package script with stubbed host commands
// and returns its output and the apt-get calls it made.
func runPackageScript(t *testing.T, script string, env map[string]string, packages map[string]dpkgAnswer) (string, string) {
	t.Helper()
	dir := t.TempDir()
	aptLog := filepath.Join(dir, "apt.log")

	var dpkg strings.Builder
	dpkg.WriteString("#!/bin/bash\nfor last; do :; done\n")
	dpkg.WriteString("case \"$*\" in *'-f=${Version}'*) printf '2.60\\n'; exit 0;; esac\n")
	dpkg.WriteString("case \"${last}\" in\n")

	for pkg, answer := range packages {
		fmt.Fprintf(&dpkg, "  %s)\n", pkg)

		if answer.stderr != "" {
			fmt.Fprintf(&dpkg, "    printf '%%s\\n' %s >&2\n", shellQuote(answer.stderr))
		}

		if answer.status != "" {
			fmt.Fprintf(&dpkg, "    printf '%%s\\n' %s\n", shellQuote(answer.status))
		}

		fmt.Fprintf(&dpkg, "    exit %d\n    ;;\n", answer.exit)
	}

	dpkg.WriteString("  *) printf 'dpkg-query: no packages found matching %s\\n' \"${last}\" >&2; exit 1 ;;\n")
	dpkg.WriteString("esac\n")

	stubs := map[string]string{
		"dpkg-query": dpkg.String(),
		"apt-get":    "#!/bin/bash\nprintf '%s\\n' \"$*\" >> \"${APT_LOG}\"\nexit 0\n",
		"apt-mark":   "#!/bin/bash\nexit 0\n",
		"cloud-init": "#!/bin/bash\nexit 0\n",
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cmd := exec.Command("/bin/bash", "-s")
	cmd.Stdin = strings.NewReader(Script(script))

	cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "APT_LOG=" + aptLog}
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", script, err, out)
	}

	apt, err := os.ReadFile(aptLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	return string(out), string(apt)
}

// The scripts decide whether to run apt-get from dpkg's ${Status} state, so a
// held package must not be reinstalled and a partial or unknown one must be.
func TestPackageScriptsAcceptHeldAndRejectOtherStates(t *testing.T) {
	baseEnv := map[string]string{"PACKAGES": "curl jq", "OPTIONAL_PACKAGES": ""}
	pgEnv := map[string]string{
		"PACKAGES": "postgresql-18", "HOLD": "", "PGDG_SCRIPT": "", "PGBACKREST_MIN": "",
		"EXTENSION_PACKAGES": "", "STOP_PGBOUNCER": "0",
	}
	installed := dpkgAnswer{status: "install ok installed"}

	held := dpkgAnswer{status: "hold ok installed"}
	for _, tc := range []struct {
		name     string
		script   string
		env      map[string]string
		packages map[string]dpkgAnswer
		want     string // the apt-get install call expected, empty for none
	}{
		{
			name:   "held installed base packages",
			script: "base-packages", env: baseEnv,
			packages: map[string]dpkgAnswer{"curl": held, "jq": held},
		},
		{
			name:   "unheld installed base packages",
			script: "base-packages", env: baseEnv,
			packages: map[string]dpkgAnswer{"curl": installed, "jq": installed},
		},
		{
			name:   "held installed postgres packages",
			script: "postgres-packages", env: pgEnv,
			packages: map[string]dpkgAnswer{"postgresql-common": installed, "postgresql-18": held, "pgbackrest": held},
		},
		{
			name:   "unheld installed postgres packages",
			script: "postgres-packages", env: pgEnv,
			packages: map[string]dpkgAnswer{"postgresql-common": installed, "postgresql-18": installed, "pgbackrest": installed},
		},
		{
			name:   "missing base package",
			script: "base-packages", env: baseEnv,
			packages: map[string]dpkgAnswer{"curl": held},
			want:     "install -y jq",
		},
		{
			name:   "unconfigured base package",
			script: "base-packages", env: baseEnv,
			packages: map[string]dpkgAnswer{"curl": {status: "install ok half-configured"}, "jq": held},
			want:     "install -y curl",
		},
		{
			name:   "base package query error",
			script: "base-packages", env: baseEnv,
			packages: map[string]dpkgAnswer{
				"curl": held,
				"jq":   {stderr: "dpkg-query: error: unable to access the dpkg database", exit: 2},
			},
			want: "install -y jq",
		},
		{
			name:   "missing postgres package",
			script: "postgres-packages", env: pgEnv,
			packages: map[string]dpkgAnswer{"postgresql-common": installed, "pgbackrest": held},
			want:     "install -y postgresql-18",
		},
		{
			name:   "unconfigured postgres package",
			script: "postgres-packages", env: pgEnv,
			packages: map[string]dpkgAnswer{
				"postgresql-common": installed,
				"postgresql-18":     {status: "install ok half-configured"},
				"pgbackrest":        held,
			},
			want: "install -y postgresql-18",
		},
		{
			name:   "postgres package query error",
			script: "postgres-packages", env: pgEnv,
			packages: map[string]dpkgAnswer{
				"postgresql-common": installed,
				"postgresql-18":     {stderr: "dpkg-query: error: unable to access the dpkg database", exit: 2},
				"pgbackrest":        held,
			},
			want: "install -y postgresql-18",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, apt := runPackageScript(t, tc.script, tc.env, tc.packages)
			if tc.want == "" {
				if strings.Contains(apt, "install -y") {
					t.Fatalf("apt-get installed an installed package: %q\n%s", apt, out)
				}

				return
			}

			if !strings.Contains(apt, tc.want) {
				t.Fatalf("apt-get did not run %q: %q\n%s", tc.want, apt, out)
			}
		})
	}
}

// Debian 13's PGDG publishes no postgresql-contrib binary for PostgreSQL 18;
// postgresql-18 Provides postgresql-contrib-18 and ships its files. A
// converged PG18 host must not ask apt to install the virtual package on
// every apply.
func TestPostgresPackagesScriptAcceptsPG18ContribVirtual(t *testing.T) {
	env := postgresPackagesEnv(nil)
	// This test exercises installed-package detection, not PGDG provisioning
	// or stopping the host's pooler.
	env["PGDG_SCRIPT"] = ""
	env["STOP_PGBOUNCER"] = "0"
	installed := dpkgAnswer{status: "hold ok installed"}
	packages := map[string]dpkgAnswer{
		"postgresql-common":    {status: "install ok installed"},
		"postgresql-18":        installed,
		"postgresql-client-18": installed,
		"pgbouncer":            installed,
		"pgbackrest":           installed,
		"postgresql-contrib":   {status: "unknown ok not-installed"},
	}

	out, apt := runPackageScript(t, "postgres-packages", env, packages)
	if strings.Contains(apt, "install -y") {
		t.Fatalf("converged PG18 host reinstalled packages: %q\n%s", apt, out)
	}

	if !strings.Contains(env["PACKAGES"], "postgresql-18") ||
		!strings.Contains(env["PACKAGES"], "postgresql-client-18") ||
		strings.Contains(env["PACKAGES"], "postgresql-contrib") {
		t.Fatalf("PACKAGES = %q", env["PACKAGES"])
	}
}

// tls.sh refuses a Caddy box before it changes anything, so this is the one
// path that runs without root. A caddy held by the old vaire is still a Caddy
// box.
func TestTLSScriptRefusesHeldCaddy(t *testing.T) {
	dir := t.TempDir()

	stubs := map[string]string{
		// A held caddy, still installed.
		"dpkg-query": "#!/bin/bash\nprintf '%s\\n' 'hold ok installed'\n",
		// Must never run: the refusal comes first. It fails the script if it
		// does, so this test cannot touch the host.
		"apt-get": "#!/bin/bash\nexit 1\n",
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cmd := exec.Command("/bin/bash", "-s")
	cmd.Stdin = strings.NewReader(Script("tls"))
	cmd.Env = []string{
		"PATH=" + dir + ":/usr/bin:/bin", "PHASE=install", "NGINX_MIN_VERSION=1.24.0",
		"TLS_HOSTNAME=db.example.test", "ACME_WEBROOT=/tmp/vaire-acme-test",
	}

	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "caddy is installed") {
		t.Fatalf("held caddy: %v\n%s", err, out)
	}
}
