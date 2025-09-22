package humanize

import "fmt"

// IBytes formats a byte count using powers of 1024 to match the real package behaviour.
func IBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}

	div := unit
	exp := 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}

	prefixes := "KMGTPE"
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), prefixes[exp])
}
