// Package wsl provides utilities for detecting and working with WSL (Windows Subsystem for Linux).
package wsl

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

var (
	isWSLCached     bool
	isWSLChecked    bool
	isWSLMu         sync.Mutex
	powershellPath  string
	powershellOnce  sync.Once
)

// IsWSL returns true if running inside Windows Subsystem for Linux.
func IsWSL() bool {
	isWSLMu.Lock()
	defer isWSLMu.Unlock()

	if isWSLChecked {
		return isWSLCached
	}

	data, err := os.ReadFile("/proc/version")
	if err != nil {
		isWSLChecked = true
		isWSLCached = false
		return false
	}

	lower := strings.ToLower(string(data))
	isWSLCached = strings.Contains(lower, "microsoft") || strings.Contains(lower, "wsl")
	isWSLChecked = true
	return isWSLCached
}

// FindPowerShell locates powershell.exe by scanning /mnt for Windows drives.
// It finds the highest version in WindowsPowerShell and caches the result.
// Returns empty string if not found.
func FindPowerShell() string {
	powershellOnce.Do(func() {
		powershellPath = discoverPowerShellPath()
	})
	return powershellPath
}

// discoverPowerShellPath scans for PowerShell in Windows drives mounted under /mnt.
func discoverPowerShellPath() string {
	// Look for drive letters in /mnt
	entries, err := os.ReadDir("/mnt")
	if err != nil {
		return ""
	}

	for _, entry := range entries {
		if !entry.IsDir() || len(entry.Name()) != 1 {
			continue // Skip non-single-letter entries
		}

		drivePath := filepath.Join("/mnt", entry.Name())
		windowsPath := filepath.Join(drivePath, "Windows")

		// Check if this drive has Windows
		if _, err := os.Stat(windowsPath); os.IsNotExist(err) {
			continue
		}

		// Look for WindowsPowerShell
		psBasePath := filepath.Join(windowsPath, "System32", "WindowsPowerShell")
		if _, err := os.Stat(psBasePath); os.IsNotExist(err) {
			continue
		}

		// Find version directories and pick the highest
		versions, err := os.ReadDir(psBasePath)
		if err != nil {
			continue
		}

		var versionDirs []string
		for _, v := range versions {
			if v.IsDir() {
				versionDirs = append(versionDirs, v.Name())
			}
		}

		if len(versionDirs) == 0 {
			continue
		}

		// Sort versions descending (highest first)
		sort.Sort(sort.Reverse(sort.StringSlice(versionDirs)))

		// Check each version for powershell.exe
		for _, ver := range versionDirs {
			psExe := filepath.Join(psBasePath, ver, "powershell.exe")
			if _, err := os.Stat(psExe); err == nil {
				return psExe
			}
		}
	}

	return ""
}
