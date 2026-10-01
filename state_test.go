package main

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/go-telegram/bot/models"
	"github.com/pyed/rtapi"
)

func TestStateSavesAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	loaded, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := loaded.update(func(data *stateData) {
		data.Sorts = map[int64]rtapi.Sorting{111: rtapi.ByNameRev}
	}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode = %v, want 0600", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}

	reloaded, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	reloaded.read(func(data *stateData) {
		if data.Version != stateVersion || data.Sorts[111] != rtapi.ByNameRev {
			t.Fatalf("reloaded state = %+v", data)
		}
	})
}

func TestStateRefusesDamagedOrNewerFiles(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"damaged.json": `{"version": 1, "sorts": `,
		"newer.json":   `{"version": 99}`,
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadState(path); err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("loadState(%s) = %v, want an error naming the file", name, err)
		}
	}
	if fresh, err := loadState(filepath.Join(dir, "missing.json")); err != nil || fresh == nil {
		t.Fatalf("a missing file should start empty: %v", err)
	}
}

func TestNilStateReadsEmptyAndRefusesUpdates(t *testing.T) {
	var missing *state
	missing.read(func(data *stateData) {
		if data.Sorts != nil {
			t.Fatal("nil state read non-empty data")
		}
	})
	if err := missing.update(func(*stateData) {}); err == nil {
		t.Fatal("nil state accepted an update")
	}
}

func TestSortOrderSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	stored, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	_, client := newFakeRtorrent(t)
	telegramFake := &fakeTelegram{sent: make(chan sentMessage, 1)}
	app := &application{
		bot: newTestBot(t, telegramFake, "123:SECRET"), rtorrent: client,
		logger: log.New(io.Discard, "", 0), token: "123:SECRET", state: stored,
		masters: principals{ids: map[int64]struct{}{7: {}}},
	}
	app.handle(context.Background(), &models.Update{Message: &models.Message{
		From: &models.User{ID: 7}, Chat: models.Chat{ID: 111, Type: models.ChatTypePrivate}, Text: "sort rev size",
	}})
	expectSent(t, telegramFake.sent, 111, "sort: by reversed size")

	restarted, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	restarted.read(func(data *stateData) {
		if data.Sorts[111] != rtapi.BySizeRev {
			t.Fatalf("sort after restart = %v", data.Sorts[111])
		}
	})
}
