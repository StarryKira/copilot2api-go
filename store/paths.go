package store

import (
	"os"
	"path/filepath"
)

var AppDir string

func init() {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	AppDir = filepath.Join(home, ".local", "share", "copilot-api")
}

func AccountsFile() string {
	return filepath.Join(AppDir, "accounts.json")
}

func PoolConfigFile() string {
	return filepath.Join(AppDir, "pool-config.json")
}

func AdminFile() string {
	return filepath.Join(AppDir, "admin.json")
}

func ModelMapFile() string {
	return filepath.Join(AppDir, "model_map.json")
}

func ProxyConfigFile() string {
	return filepath.Join(AppDir, "proxy-config.json")
}

func UsageStatsFile() string {
	return filepath.Join(AppDir, "usage-stats.json")
}

func EnsurePaths() error {
	if err := os.MkdirAll(AppDir, 0755); err != nil {
		return err
	}
	files := []string{AccountsFile(), PoolConfigFile(), AdminFile(), ModelMapFile(), ProxyConfigFile(), UsageStatsFile()}
	for _, f := range files {
		if _, err := os.Stat(f); os.IsNotExist(err) {
			if err := os.WriteFile(f, []byte("{}"), 0644); err != nil {
				return err
			}
		}
	}
	return nil
}
