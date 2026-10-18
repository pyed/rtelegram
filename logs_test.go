package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A log file that reaches its limit moves aside to FILE.1, replacing the one
// before, and the log goes on in a new file; one that starts over its limit,
// from an earlier run, moves aside at the first line.
func TestLogFilesMoveAsideAtTheirLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rtelegram.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("old\n", 30)), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := openLogFile(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	logger := log.New(file, "", 0)
	read := func(path string) string {
		t.Helper()
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(content)
	}
	logger.Print("first")
	if got, aside := read(path), read(path+".1"); got != "first\n" || aside != strings.Repeat("old\n", 30) {
		t.Fatalf("after the first line: %q, aside %q", got, aside)
	}
	for i := range 20 {
		logger.Printf("line %02d", i)
	}
	if got, aside := read(path), read(path+".1"); len(got) > 100 || len(aside) > 100 || !strings.HasSuffix(got, "line 19\n") ||
		strings.Contains(aside, "old") {
		t.Fatalf("after twenty lines: %q, aside %q", got, aside)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
