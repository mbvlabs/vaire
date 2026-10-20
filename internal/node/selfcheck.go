package node

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

// BinaryPath is where the node binary lives.
const BinaryPath = "/usr/local/bin/vaire"

// Check statuses.
const (
	CheckPass = "pass"
	CheckWarn = "warn"
	CheckFail = "fail"
)

// Check is one selfcheck item.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// SelfCheckData is the selfcheck result.
type SelfCheckData struct {
	Checks []Check `json:"checks"`
}

var requiredTools = []string{"bash", "systemd-run", "systemctl", "sha256sum", "install"}

var toolDirs = []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"}

// nodeDirs are the directories the node writes, with the mode used when vaire
// creates them.
var nodeDirs = []struct {
	path string
	mode os.FileMode
}{
	{filepath.Dir(engine.JournalPath), 0o700},
	{engine.LogDir, 0o750},
	{filepath.Dir(engine.LockPath), 0o1777},
}

// SelfCheck runs the §7.1 checks. Any fail returns precondition_failed with
// details.checks; warnings alone return ok with a selfcheck_warn warning.
func SelfCheck() (SelfCheckData, []output.Warning, error) {
	if os.Geteuid() != 0 {
		checks := []Check{{Name: "root", Status: CheckFail, Detail: "selfcheck must run as root"}}
		return SelfCheckData{Checks: checks}, nil, output.NewError(
			output.CodePrecondition,
			"vaire node selfcheck must run as root",
			output.ExitPrecondition,
			"Run it with sudo.",
		).WithDetails(map[string]any{"reason": "root_required", "checks": checks})
	}
	checks := []Check{
		{Name: "root", Status: CheckPass},
		checkBinary(),
		checkConfig("node_toml", config.NodePath, func(path string) error { _, err := config.LoadNode(path); return err }),
		checkConfig("cluster_toml", config.ClusterPath, func(path string) error { _, err := config.LoadCluster(path); return err }),
		checkJournal(),
		checkDirs(),
		checkTools(),
	}
	data := SelfCheckData{Checks: checks}
	var failed, warned []string
	for _, c := range checks {
		switch c.Status {
		case CheckFail:
			failed = append(failed, c.Name)
		case CheckWarn:
			warned = append(warned, c.Name)
		}
	}
	if len(failed) > 0 {
		return data, nil, output.NewError(
			output.CodePrecondition,
			"node selfcheck failed: "+strings.Join(failed, ", "),
			output.ExitPrecondition,
			"Fix the failed checks in details.checks.",
		).WithDetails(map[string]any{"reason": "selfcheck_failed", "checks": checks})
	}
	var warnings []output.Warning
	if len(warned) > 0 {
		warnings = append(warnings, output.Warning{
			Code:    "selfcheck_warn",
			Message: "selfcheck warnings: " + strings.Join(warned, ", "),
		})
	}
	return data, warnings, nil
}

func checkBinary() Check {
	c := Check{Name: "binary"}
	info, err := os.Lstat(BinaryPath)
	if err != nil {
		return fail(c, BinaryPath+" is missing")
	}
	if !info.Mode().IsRegular() {
		return fail(c, BinaryPath+" is not a regular file")
	}
	if perm := info.Mode().Perm(); perm != 0o755 {
		return fail(c, fmt.Sprintf("%s has mode %04o, want 0755", BinaryPath, perm))
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && (st.Uid != 0 || st.Gid != 0) {
		return fail(c, BinaryPath+" is not owned by root:root")
	}
	exe, err := os.Executable()
	if err != nil {
		return fail(c, "cannot resolve the running executable")
	}
	self, err := os.Stat(exe)
	if err != nil || !os.SameFile(self, info) {
		return fail(c, "the running binary "+exe+" is not "+BinaryPath)
	}
	c.Status = CheckPass
	return c
}

func checkConfig(name, path string, load func(string) error) Check {
	c := Check{Name: name}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			c.Status = CheckWarn
			c.Detail = path + " is absent: not an vaire node yet"
			return c
		}
		return fail(c, err.Error())
	}
	if err := load(path); err != nil {
		return fail(c, err.Error())
	}
	c.Status = CheckPass
	return c
}

func checkJournal() Check {
	c := Check{Name: "journal"}
	info, err := os.Stat(engine.JournalPath)
	if err != nil {
		if os.IsNotExist(err) {
			c.Status = CheckPass
			c.Detail = "no journal yet"
			return c
		}
		return fail(c, err.Error())
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		return fail(c, fmt.Sprintf("%s has mode %04o, want 0600", engine.JournalPath, perm))
	}
	if _, err := engine.LoadJournal(engine.JournalPath); err != nil {
		return fail(c, err.Error())
	}
	c.Status = CheckPass
	return c
}

// checkDirs creates the node directories when missing and proves root can
// write them. A directory without owner rwx fails even though root could
// bypass the mode.
func checkDirs() Check {
	c := Check{Name: "dirs"}
	for _, d := range nodeDirs {
		if err := os.MkdirAll(d.path, d.mode); err != nil {
			return fail(c, "cannot create "+d.path+": "+err.Error())
		}
		info, err := os.Stat(d.path)
		if err != nil {
			return fail(c, err.Error())
		}
		if !info.IsDir() {
			return fail(c, d.path+" is not a directory")
		}
		if info.Mode().Perm()&0o700 != 0o700 {
			return fail(c, fmt.Sprintf("%s has mode %04o; root needs rwx", d.path, info.Mode().Perm()))
		}
		probe, err := os.CreateTemp(d.path, ".vaire-selfcheck-*")
		if err != nil {
			return fail(c, "cannot write "+d.path+": "+err.Error())
		}
		name := probe.Name()
		_ = probe.Close()
		_ = os.Remove(name)
	}
	c.Status = CheckPass
	return c
}

func checkTools() Check {
	c := Check{Name: "tools"}
	var missing []string
	for _, tool := range requiredTools {
		if findTool(tool) == "" {
			missing = append(missing, tool)
		}
	}
	if len(missing) > 0 {
		return fail(c, "missing "+strings.Join(missing, ", "))
	}
	c.Status = CheckPass
	return c
}

func findTool(name string) string {
	for _, dir := range toolDirs {
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return path
		}
	}
	return ""
}

func fail(c Check, detail string) Check {
	c.Status = CheckFail
	c.Detail = detail
	return c
}
