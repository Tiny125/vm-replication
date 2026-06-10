package main

import "path/filepath"

// defaultManifestPath derives a manifest filename from the device path.
func defaultManifestPath(device string) string {
	return filepath.Base(device) + ".cbt"
}
