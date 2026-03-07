package common

import (
	"os"
	"path/filepath"
)

func CacheDir() string {
	return filepath.Join(cacheHome(), "tofu")
}

// https://specifications.freedesktop.org/basedir/latest/#variables
func cacheHome() string {
	dir := os.Getenv("XDG_CACHE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".cache")
	}
	return dir
}
