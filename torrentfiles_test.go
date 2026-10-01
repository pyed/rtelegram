package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pyed/rtapi"
)

// showTorrent is a multi-file torrent whose data is root/show, with a small
// finished file, an unfinished one, and one too large for Telegram.
func showTorrent(t *testing.T, root string) (*rtapi.Torrent, []rtapi.File) {
	t.Helper()
	show := filepath.Join(root, "show")
	if err := os.MkdirAll(filepath.Join(show, "Extras"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"e01.mkv": "episode one", "Extras/sample.mkv": "sam"} {
		if err := os.WriteFile(filepath.Join(show, filepath.FromSlash(name)), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	torrent := &rtapi.Torrent{Name: "show", Hash: strings.Repeat("A", 40), State: rtapi.Seeding, Path: show, Directory: show, MultiFile: true}
	files := []rtapi.File{
		{Index: 0, Path: "e01.mkv", Size: 11, Chunks: 1, CompletedChunks: 1, Priority: rtapi.FileNormal},
		{Index: 1, Path: "Extras/sample.mkv", Size: 3, Chunks: 2, CompletedChunks: 1, Priority: rtapi.FileNormal},
		{Index: 2, Path: "big.mkv", Size: 60_000_000, Chunks: 3, CompletedChunks: 3, Priority: rtapi.FileNormal},
	}
	return torrent, files
}

func fileApp(t *testing.T, dataRoot string, torrent *rtapi.Torrent, files []rtapi.File) (*application, *fakeTelegram, *fakeRtorrent) {
	t.Helper()
	app, telegramFake, rtorrentFake := buttonApp(t, rtapi.Torrents{torrent})
	app.dataRoot = dataRoot
	rtorrentFake.set(func(f *fakeRtorrent) { f.files = map[string][]rtapi.File{torrent.Hash: files} })
	return app, telegramFake, rtorrentFake
}

func TestFilesScreenCyclesPriorities(t *testing.T) {
	torrent, files := showTorrent(t, t.TempDir())
	app, telegramFake, rtorrentFake := fileApp(t, "", torrent, files)
	priority := func(index int) rtapi.FilePriority {
		var p rtapi.FilePriority
		rtorrentFake.set(func(f *fakeRtorrent) { p = f.files[torrent.Hash][index].Priority })
		return p
	}

	command(app, "files aaaaaaa")
	screen := nextSent(t, telegramFake)
	if !strings.Contains(screen.text, "show\n3 files.") || !strings.Contains(screen.text, "2. Extras/sample.mkv\n3 B, 50%, normal") {
		t.Fatalf("files screen = %q", screen.text)
	}
	if !hasButton(screen.buttons, "✅ e01.mkv") || !hasButton(screen.buttons, "✅ sample.mkv") || hasButton(screen.buttons, "📥") {
		t.Fatalf("files buttons = %v", buttonTexts(screen.buttons))
	}

	press(app, master, screen.messageID, "fp:0")
	if priority(0) != rtapi.FileHigh || lastAnswer(t, telegramFake).text != "e01.mkv: high" || !hasButton(lastEdit(t, telegramFake).buttons, "⭐ e01.mkv") {
		t.Fatalf("after one press: priority %d, %+v", priority(0), lastAnswer(t, telegramFake))
	}
	press(app, master, screen.messageID, "fp:0")
	if priority(0) != rtapi.FileSkip {
		t.Fatalf("after two presses: priority %d", priority(0))
	}
	press(app, master, screen.messageID, "fp:skip")
	press(app, master, screen.messageID, "fp:normal")
	for index := range files {
		if priority(index) != rtapi.FileNormal {
			t.Fatalf("file %d priority %d after Download all", index, priority(index))
		}
	}
	if len(rtorrentFake.called("d.update_priorities")) != 4 {
		t.Fatalf("priorities applied %d times", len(rtorrentFake.called("d.update_priorities")))
	}
}

func TestCardOpensFilesAndGoesBack(t *testing.T) {
	torrent, files := showTorrent(t, t.TempDir())
	app, telegramFake, _ := fileApp(t, "", torrent, files)
	command(app, "info aaaaaaa")
	card := nextSent(t, telegramFake)

	press(app, master, card.messageID, "files")
	if shown := lastEdit(t, telegramFake); !strings.Contains(shown.text, "1. e01.mkv") || !hasButton(shown.buttons, "« Back") {
		t.Fatalf("files from card = %q %v", shown.text, buttonTexts(shown.buttons))
	}
	press(app, master, card.messageID, "back")
	if back := lastEdit(t, telegramFake); !strings.HasPrefix(back.text, "show\nSeeding") || !hasButton(back.buttons, "📂 Files") {
		t.Fatalf("back = %q", back.text)
	}
}

func TestGetSendsFinishedFilesInsideTheDataRoot(t *testing.T) {
	root := t.TempDir()
	torrent, files := showTorrent(t, root)
	app, telegramFake, _ := fileApp(t, root, torrent, files)

	command(app, "get aaaaaaa 1")
	if len(telegramFake.documents) != 1 || telegramFake.documents[0].filename != "e01.mkv" || telegramFake.documents[0].content != "episode one" {
		t.Fatalf("documents = %+v", telegramFake.documents)
	}
	for text, want := range map[string]string{
		"get aaaaaaa 2": "get: Extras/sample.mkv has not finished downloading",
		"get aaaaaaa 3": "get: big.mkv is 57.2 MiB, more than the 50 MB Telegram lets bots send",
		"get aaaaaaa 9": "get: file number must be between 1 and 3",
		"get":           "get: use get HASH [FILE NUMBER]",
	} {
		if got := lastSentText(t, telegramFake, app, text); got != want {
			t.Errorf("%s = %q, want %q", text, got, want)
		}
	}

	command(app, "get aaaaaaa")
	choose := nextSent(t, telegramFake)
	if !strings.HasPrefix(choose.text, "Choose a file with 📥.") || !hasButton(choose.buttons, "📥") {
		t.Fatalf("choice = %q %v", choose.text, buttonTexts(choose.buttons))
	}
	if strings.Count(strings.Join(buttonTexts(choose.buttons), " "), "📥") != 1 {
		t.Fatalf("📥 offered for files that cannot be sent: %v", buttonTexts(choose.buttons))
	}
	press(app, master, choose.messageID, "fg:0")
	if len(telegramFake.documents) != 2 || lastAnswer(t, telegramFake).text != "Sent e01.mkv" {
		t.Fatalf("documents = %d, answer %+v", len(telegramFake.documents), lastAnswer(t, telegramFake))
	}
}

func TestGetSendsASingleFileTorrentWithoutANumber(t *testing.T) {
	root := t.TempDir()
	movie := filepath.Join(root, "movie.mkv")
	if err := os.WriteFile(movie, []byte("film"), 0o600); err != nil {
		t.Fatal(err)
	}
	torrent := &rtapi.Torrent{Name: "movie.mkv", Hash: strings.Repeat("A", 40), State: rtapi.Seeding, Path: movie, Directory: root}
	app, telegramFake, _ := fileApp(t, root, torrent, []rtapi.File{{Path: "movie.mkv", Size: 4, Chunks: 1, CompletedChunks: 1, Priority: rtapi.FileNormal}})
	command(app, "get aaaaaaa")
	if len(telegramFake.documents) != 1 || telegramFake.documents[0].content != "film" {
		t.Fatalf("documents = %+v", telegramFake.documents)
	}
}

func TestGetRefusesWithoutADataRootOrOutsideIt(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "downloads")
	torrent, files := showTorrent(t, root)

	app, telegramFake, _ := fileApp(t, "", torrent, files)
	if got := lastSentText(t, telegramFake, app, "get aaaaaaa 1"); got != "get: sending files is disabled; configure an absolute -data-root" {
		t.Fatalf("without -data-root = %q", got)
	}

	elsewhere := filepath.Join(base, "elsewhere")
	app, telegramFake, _ = fileApp(t, elsewhere, torrent, files)
	if got := lastSentText(t, telegramFake, app, "get aaaaaaa 1"); !strings.Contains(got, "not strictly inside") {
		t.Fatalf("outside -data-root = %q", got)
	}

	secret := filepath.Join(base, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "show", "escape.mkv")); err != nil {
		t.Logf("symlink checks unavailable: %v", err)
		return
	}
	files = append(files, rtapi.File{Index: 3, Path: "escape.mkv", Size: 6, Chunks: 1, CompletedChunks: 1, Priority: rtapi.FileNormal})
	app, telegramFake, _ = fileApp(t, root, torrent, files)
	if got := lastSentText(t, telegramFake, app, "get aaaaaaa 4"); !strings.HasPrefix(got, "get: read escape.mkv") {
		t.Fatalf("symlink escape = %q", got)
	}
	if len(telegramFake.documents) != 0 {
		t.Fatal("a file outside -data-root was sent")
	}
}
