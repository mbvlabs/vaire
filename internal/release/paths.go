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
		return filepath.Join(os.TempDir(), "eru-config")
	}
	return filepath.Join(home, ".config")
}

func cacheHome() string {
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "eru-cache")
	}
	return filepath.Join(home, ".cache")
}

func releaseCacheDir(version, goos, goarch string) string {
	return filepath.Join(configHome(), "eru", "cache", "releases", version, goos+"-"+goarch)
}

func tufCacheDir() string {
	return filepath.Join(cacheHome(), "eru", "sigstore-tuf")
}
