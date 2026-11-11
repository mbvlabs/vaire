package doctor

import (
	"sort"
	"strconv"
	"strings"
)

// Extension checks (Phase 7). The node gathers the facts; these checks never
// read the host themselves. Unknown facts stay unknown: a stopped or
// unreadable database is not reported as having no extensions, and a pending
// restart is kept separate from a missing extension.

// ExtensionInfo is one extension a database enables, in the shape the checks
// need. ExtName is empty for a preload-only module such as auto_explain.
type ExtensionInfo struct {
	Name    string
	ExtName string
	Preload string
}

// ExtensionVersion is an installed extension's version and the version its
// installed control file or package reports as the default.
type ExtensionVersion struct {
	Version string
	Default string
}

// ExtensionFacts are one database's extension facts as the node read them.
type ExtensionFacts struct {
	// Checked says gatherFacts attempted the host-wide facts. A pure test
	// fixture can leave it false and skip the checks.
	Checked bool
	// Enabled is the effective selection, including implicit default modules.
	Enabled []ExtensionInfo
	// Known maps SQL extension names to their configured definitions, so an
	// installed extension can be classified as managed or unmanaged.
	Known map[string]ExtensionInfo
	// PackagesKnown is false when dpkg-query could not answer; PackageError
	// says why. MissingPackages are APT packages the selection needs and
	// dpkg reports as not installed.
	PackagesKnown   bool
	PackageError    string
	MissingPackages []string
	// PrerequisitesKnown is false when the selection itself could not be
	// resolved. MissingPrerequisites is the node's file/control diagnosis.
	PrerequisitesKnown   bool
	MissingPrerequisites string
	// Available is true when pg_extension and the cluster's preloads were
	// read. Error explains an unavailable reading; it is never "none".
	Available bool
	Error     string
	Installed map[string]ExtensionVersion
	// LoadedKnown and PendingKnown are separate: a cluster can expose its
	// installed extensions while its effective preload state cannot be read.
	LoadedKnown    bool
	Loaded         map[string]bool
	PendingKnown   bool
	PendingRestart bool
	// Unmanaged are installed SQL extensions no built-in or custom definition
	// knows, excluding plpgsql.
	Unmanaged []string
}

// ExtensionChecks are the extension checks, appended to NodeChecks.
func ExtensionChecks() []Check {
	return []Check{
		{ID: "ext.packages", Category: CatPostgres, AtInit: true, Run: extPackages},
		{ID: "ext.state", Category: CatPostgres, AtInit: true, Run: extState},
		{ID: "ext.updates", Category: CatPostgres, AtInit: true, Run: extUpdates},
	}
}

func extPackages(f Facts) []Result {
	return perCluster(f, false, func(c ClusterFacts) (Status, string, string) {
		x := c.Ext
		if !x.Checked {
			return Skip, "extension facts were not gathered", ""
		}
		if len(x.MissingPackages) > 0 {
			return Fail, "the packages of its extensions are not installed: " + joinSorted(x.MissingPackages), "vaire apply"
		}
		if x.MissingPrerequisites != "" {
			return Fail, x.MissingPrerequisites, "vaire apply, or install the preinstalled extension files named above"
		}
		if !x.PackagesKnown {
			return Warn, "cannot read the package state: " + x.PackageError, "vaire apply"
		}
		return Pass, "every package and file its extensions need is present", ""
	})
}

func extState(f Facts) []Result {
	return perCluster(f, true, func(c ClusterFacts) (Status, string, string) {
		x := c.Ext
		if !x.Checked {
			return Skip, "extension facts were not gathered", ""
		}
		if !x.Available {
			return Warn, "cannot read its extensions: " + x.Error, "vaire status --db " + c.Name + "; vaire logs postgres --db " + c.Name
		}
		enabled := map[string]bool{}
		var waiting, notLoaded, missing, unconfigured, unmanaged []string
		for _, e := range x.Enabled {
			enabled[e.Name] = true
			if e.Preload != "" && x.LoadedKnown && !x.Loaded[e.Preload] {
				if x.PendingKnown && x.PendingRestart {
					waiting = append(waiting, e.Name)
				} else {
					notLoaded = append(notLoaded, e.Name)
				}
			}
			if e.ExtName != "" {
				if _, ok := x.Installed[e.ExtName]; !ok {
					missing = append(missing, e.Name)
				}
			}
		}
		for extname, v := range x.Installed {
			if extname == "plpgsql" {
				continue
			}
			info, ok := x.Known[extname]
			if !ok {
				unmanaged = append(unmanaged, extname+" "+v.Version)
				continue
			}
			if !enabled[info.Name] {
				unconfigured = append(unconfigured, info.Name)
			}
		}
		sort.Strings(waiting)
		sort.Strings(notLoaded)
		sort.Strings(missing)
		sort.Strings(unconfigured)
		sort.Strings(unmanaged)
		switch {
		case len(waiting) > 0:
			return Warn, joinSorted(waiting) + " wait for the restart that preloads them", "vaire apply --db " + c.Name + " --confirm " + c.Name + "/restart"
		case len(notLoaded) > 0:
			return Warn, joinSorted(notLoaded) + " need a preload library that is not loaded and no restart is pending", "vaire apply --db " + c.Name
		case len(missing) > 0:
			return Warn, "cluster.toml enables " + joinSorted(missing) + ", which the database doesn't have", "vaire apply --db " + c.Name
		case len(unconfigured) > 0:
			return Warn, "the database has " + joinSorted(unconfigured) + ", which cluster.toml doesn't enable", "vaire ext add " + unconfigured[0] + " --db " + c.Name
		case len(unmanaged) > 0:
			return Warn, "the database has unmanaged extensions: " + joinSorted(unmanaged), "vaire ext list --db " + c.Name
		case !x.LoadedKnown:
			return Warn, "cannot read its shared_preload_libraries", "vaire status --db " + c.Name + "; vaire logs postgres --db " + c.Name
		}
		return Pass, "the extensions match cluster.toml: " + describeExtensionVersions(x), ""
	})
}

func extUpdates(f Facts) []Result {
	return perCluster(f, true, func(c ClusterFacts) (Status, string, string) {
		x := c.Ext
		if !x.Checked || !x.Available {
			return Skip, "its extensions could not be read", ""
		}
		var stale, unknown []string
		fix := ""
		for extname, v := range x.Installed {
			info, ok := x.Known[extname]
			if !ok {
				continue
			}
			if v.Default == "" {
				unknown = append(unknown, info.Name)
				continue
			}
			if !extensionVersionNewer(v.Version, v.Default) {
				continue
			}
			stale = append(stale, info.Name+" "+v.Version+" -> "+v.Default)
			if fix == "" {
				fix = "vaire ext add " + info.Name + " --db " + c.Name
			}
		}
		sort.Strings(stale)
		sort.Strings(unknown)
		switch {
		case len(stale) > 0:
			return Warn, "the package is newer than the extension in the database: " + strings.Join(stale, ", "), fix
		case len(unknown) > 0:
			return Warn, "cannot read the default version of " + joinSorted(unknown), "vaire apply --db " + c.Name
		}
		return Pass, "every extension runs its installed package's default version", ""
	})
}

// extensionVersionNewer reports whether def is plausibly newer than version.
// Numeric dotted versions are compared; an unparseable pair is treated as a
// possible update rather than silently hidden.
func extensionVersionNewer(version, def string) bool {
	if def == "" || def == version {
		return false
	}
	as, aok := extensionVersionParts(version)
	bs, bok := extensionVersionParts(def)
	if !aok || !bok {
		return true
	}
	for len(as) < len(bs) {
		as = append(as, 0)
	}
	for len(bs) < len(as) {
		bs = append(bs, 0)
	}
	for i := range as {
		if as[i] < bs[i] {
			return true
		}
		if as[i] > bs[i] {
			return false
		}
	}
	return false
}

func extensionVersionParts(v string) ([]int, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, false
	}
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

func describeExtensionVersions(x ExtensionFacts) string {
	var parts []string
	for extname, v := range x.Installed {
		if extname == "plpgsql" {
			continue
		}
		parts = append(parts, extname+" "+v.Version)
	}
	sort.Strings(parts)
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}
