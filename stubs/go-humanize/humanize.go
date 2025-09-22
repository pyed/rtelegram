package humanize

import "fmt"

// IBytes is a stubbed implementation that returns the byte count formatted in bytes.
func IBytes(b uint64) string {
	return fmt.Sprintf("%d B", b)
}
