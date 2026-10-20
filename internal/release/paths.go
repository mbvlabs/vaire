package release

import (
	"os"
	"path/filepath"
)

func configHome() string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "vaire-config")
	}
	return filepath.Join(home, ".config")
}

func cacheHome() string {
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "vaire-cache")
	}
	return filepath.Join(home, ".cache")
}

func releaseCacheDir(version, goos, goarch string) string {
	return filepath.Join(configHome(), "vaire", "cache", "releases", version, goos+"-"+goarch)
}

func tufCacheDir() string {
	return filepath.Join(cacheHome(), "vaire", "sigstore-tuf")
}
