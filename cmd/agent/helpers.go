package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// defaultManifestPath derives a checkpoint filename from the device path,
// e.g. /dev/sda -> sda.cbt, ./disk.img -> disk.img.cbt.
func defaultManifestPath(device string) string {
	base := filepath.Base(device)
	return base + ".cbt"
}

// hostOf strips the port from a host:port address.
func hostOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[:i]
	}
	return addr
}

func jsonUnmarshal(b []byte, v any) error {
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("decode message: %w", err)
	}
	return nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
