// Package size parses and formats human byte sizes like "1000GB", "1GB", "512MB".
package size

import (
	"fmt"
	"strconv"
	"strings"
)

// Parse turns strings like "1000GB", "1.5TB", "512MB", "1024" into bytes.
// Bare numbers are bytes. Units are powers of 1024 (GB = GiB), because
// storage people mean binary when they say GB for a volume.
func Parse(s string) (uint64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	mult := uint64(1)
	// split numeric prefix from unit suffix
	i := 0
	for i < len(s) && (s[i] == '.' || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	numStr, unitStr := s[:i], strings.TrimSpace(s[i:])
	unit := unitStr
	switch unit {
	case "", "B":
		mult = 1
	case "K", "KB", "KIB":
		mult = 1 << 10
	case "M", "MB", "MIB":
		mult = 1 << 20
	case "G", "GB", "GIB":
		mult = 1 << 30
	case "T", "TB", "TIB":
		mult = 1 << 40
	case "P", "PB", "PIB":
		mult = 1 << 50
	default:
		return 0, fmt.Errorf("unknown size unit %q", unit)
	}
	f, err := strconv.ParseFloat(numStr, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("bad size %q", s)
	}
	return uint64(f * float64(mult)), nil
}

// Format renders bytes as e.g. "1.00 GB", "4.20 KB".
func Format(b uint64) string {
	const u = 1 << 10
	f := float64(b)
	switch {
	case b >= 1<<50:
		return fmt.Sprintf("%.2f PB", f/(1<<50))
	case b >= 1<<40:
		return fmt.Sprintf("%.2f TB", f/(1<<40))
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GB", f/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.2f MB", f/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.2f KB", f/u)
	default:
		return fmt.Sprintf("%d B", b)
	}
}
