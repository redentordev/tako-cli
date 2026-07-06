package cmd

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

func currentUsername() string {
	if u, err := user.Current(); err == nil && u != nil {
		name := strings.TrimSpace(u.Username)
		if idx := strings.LastIndexAny(name, `\\/`); idx >= 0 {
			name = name[idx+1:]
		}
		return name
	}
	return strings.TrimSpace(os.Getenv("USER"))
}

func expandHome(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}
