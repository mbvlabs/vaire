//go:build !e2etest

package config

// ClusterExtra holds build-specific tables. It has none in production builds.
type ClusterExtra struct{}
