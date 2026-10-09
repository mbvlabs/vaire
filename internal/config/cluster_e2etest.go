//go:build e2etest

package config

// ClusterExtra adds the [e2e] table the stub steps read.
type ClusterExtra struct {
	E2E E2ETable `toml:"e2e"`
}

// E2ETable is the [e2e] table, compiled only into e2etest builds.
type E2ETable struct {
	Value string `toml:"value"`
}
