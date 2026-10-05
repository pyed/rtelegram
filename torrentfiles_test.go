package main

import (
	"fmt"
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

func TestFilesListOpensFileCards(t *testing.T) {
	torrent, files := showTorrent(t, t.TempDir())
	app, telegramFake, rtorrentFake := fileApp(t, "", torrent, files)
	priority := func(index int) rtapi.FilePriority {
		var p rtapi.FilePriority
		rtorrentFake.set(func(f *fakeRtorrent) { p = f.files[torrent.Hash][index].Priority })
		return p
	}

	command(app, "files aaaaaaa")
	list := nextSent(t, telegramFake)
	if !strings.HasPrefix(list.text, "show\n3 files, 57.2 MiB; downloading 3 of them, 57.2 MiB.") ||
		!strings.Contains(list.text, "\n2. ✅ Extras/sample.mkv\n3 B · 50%") {
		t.Fatalf("files list = %q", list.text)
	}
	// One numbered button per file, however long the names: rows stay even.
	if got := buttonTexts(list.buttons); strings.Join(got, ",") != "✅ 1=fo:0,✅ 2=fo:1,✅ 3=fo:2,⬜ Skip all=fp:skip,✅ Download all=fp:normal" {
		t.Fatalf("files buttons = %v", got)
	}

	press(app, master, list.messageID, "fo:1")
	card := lastEdit(t, telegramFake)
	if !strings.HasPrefix(card.text, "Extras/sample.mkv\nFile 2 of 3 in show\n3 B · 50% done\nPriority: ✅ download") ||
		!strings.Contains(card.text, "Sending files is off") || hasButton(card.buttons, "📥 Send") {
		t.Fatalf("file card = %q %v", card.text, buttonTexts(card.buttons))
	}
	if !hasButton(card.buttons, "• ✅ Download") || !hasButton(card.buttons, "⬜ Skip") || !hasButton(card.buttons, "« Files") {
		t.Fatalf("file card buttons = %v", buttonTexts(card.buttons))
	}

	press(app, master, list.messageID, "fp:1:high")
	if priority(1) != rtapi.FileHigh || lastAnswer(t, telegramFake).text != "sample.mkv: download first" ||
		!hasButton(lastEdit(t, telegramFake).buttons, "• ⭐ First") {
		t.Fatalf("after First: priority %d, %+v", priority(1), lastAnswer(t, telegramFake))
	}
	press(app, master, list.messageID, "fp:1:skip")
	if priority(1) != rtapi.FileSkip {
		t.Fatalf("after Skip: priority %d", priority(1))
	}
	press(app, master, list.messageID, "fp:1:loud")
	if priority(1) != rtapi.FileSkip || lastAnswer(t, telegramFake).text != "This button no longer applies." {
		t.Fatalf("an unknown priority was applied: %d", priority(1))
	}

	press(app, master, list.messageID, "fl")
	back := lastEdit(t, telegramFake)
	if !strings.Contains(back.text, "downloading 2 of them") || !hasButton(back.buttons, "⬜ 2") {
		t.Fatalf("back to the list = %q %v", back.text, buttonTexts(back.buttons))
	}
	press(app, master, list.messageID, "fp:normal")
	for index := range files {
		if priority(index) != rtapi.FileNormal {
			t.Fatalf("file %d priority %d after Download all", index, priority(index))
		}
	}
	if lastAnswer(t, telegramFake).text != "3 files: download" {
		t.Fatalf("answer = %+v", lastAnswer(t, telegramFake))
	}
}

func TestFilesFilterNarrowsTheListAndBulkChoices(t *testing.T) {
	torrent, files := showTorrent(t, t.TempDir())
	app, telegramFake, rtorrentFake := fileApp(t, "", torrent, files)

	command(app, "files aaaaaaa MKV extras")
	list := nextSent(t, telegramFake)
	if !strings.Contains(list.text, `1 match "MKV extras".`) || strings.Contains(list.text, "e01.mkv") ||
		!hasButton(list.buttons, "⬜ Skip these") || !hasButton(list.buttons, "✅ 2") || hasButton(list.buttons, "✅ 1") {
		t.Fatalf("filtered list = %q %v", list.text, buttonTexts(list.buttons))
	}
	press(app, master, list.messageID, "fp:skip")
	rtorrentFake.set(func(f *fakeRtorrent) {
		for _, file := range f.files[torrent.Hash] {
			if want := file.Path == "Extras/sample.mkv"; (file.Priority == rtapi.FileSkip) != want {
				t.Errorf("%s priority %d after Skip these", file.Path, file.Priority)
			}
		}
	})

	command(app, "files aaaaaaa nothing")
	if none := nextSent(t, telegramFake); !strings.Contains(none.text, `0 match "nothing".`) || hasButton(none.buttons, "⬜ Skip these") {
		t.Fatalf("no matches = %q %v", none.text, buttonTexts(none.buttons))
	}
}

func TestFilesPagesJumpToTheEnds(t *testing.T) {
	torrent := &rtapi.Torrent{Name: "pack", Hash: strings.Repeat("A", 40), State: rtapi.Seeding, MultiFile: true}
	files := make([]rtapi.File, 45)
	for i := range files {
		files[i] = rtapi.File{Index: i, Path: fmt.Sprintf("file %02d.pkg", i+1), Size: 1, Chunks: 1, Priority: rtapi.FileNormal}
	}
	app, telegramFake, _ := fileApp(t, "", torrent, files)
	command(app, "files aaaaaaa")
	list := nextSent(t, telegramFake)
	if got := buttonTexts(list.buttons[2:3]); strings.Join(got, ",") != "1/5=noop,▶=pg:1,⏭=pg:4" {
		t.Fatalf("first page navigation = %v", got)
	}
	if len(list.buttons[0]) != filesPerRow || len(list.buttons[1]) != filesPerRow {
		t.Fatalf("file button rows = %v", buttonTexts(list.buttons))
	}
	// ⏮ and ⏭ appear only where ◀ and ▶ do not already reach the ends.
	for page, want := range map[string]string{
		"1": "◀=pg:0,2/5=noop,▶=pg:2,⏭=pg:4",
		"3": "⏮=pg:0,◀=pg:2,4/5=noop,▶=pg:4",
	} {
		press(app, master, list.messageID, "pg:"+page)
		if got := buttonTexts(lastEdit(t, telegramFake).buttons[2:3]); strings.Join(got, ",") != want {
			t.Errorf("page %s navigation = %v, want %s", page, got, want)
		}
	}
	press(app, master, list.messageID, "pg:4")
	last := lastEdit(t, telegramFake)
	if got := buttonTexts(last.buttons[1:2]); strings.Join(got, ",") != "⏮=pg:0,◀=pg:3,5/5=noop" || !strings.Contains(last.text, "45. ✅ file 45.pkg") {
		t.Fatalf("last page = %q %v", last.text, got)
	}
	// A page button pressed as a card opens, as with a quick second tap,
	// shows that page of the list.
	press(app, master, list.messageID, "fo:44")
	press(app, master, list.messageID, "pg:0")
	if first := lastEdit(t, telegramFake); !strings.Contains(first.text, "1. ✅ file 01.pkg") {
		t.Fatalf("page press on a card = %q", first.text)
	}
}

func TestCardOpensFilesAndGoesBack(t *testing.T) {
	torrent, files := showTorrent(t, t.TempDir())
	app, telegramFake, _ := fileApp(t, "", torrent, files)
	command(app, "info aaaaaaa")
	card := nextSent(t, telegramFake)

	press(app, master, card.messageID, "files")
	if shown := lastEdit(t, telegramFake); !strings.Contains(shown.text, "1. ✅ e01.mkv") || !hasButton(shown.buttons, "« Back") {
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

	// The file goes in the background, so the bot goes on answering.
	if got := lastSentText(t, telegramFake, app, "get aaaaaaa 1"); got != "Sending e01.mkv (11 B)…" {
		t.Fatalf("get aaaaaaa 1 = %q", got)
	}
	app.wg.Wait()
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
	if !strings.HasPrefix(choose.text, "Files marked 📥 can be sent.") || strings.Count(choose.text, "📥") != 2 ||
		!strings.Contains(choose.text, "11 B · 100% · 📥") {
		t.Fatalf("choice = %q", choose.text)
	}
	for index, want := range map[string]string{"1": "It can be sent once it has downloaded.", "2": "It is too large to send"} {
		press(app, master, choose.messageID, "fo:"+index)
		if card := lastEdit(t, telegramFake); !strings.Contains(card.text, want) || hasButton(card.buttons, "📥 Send") {
			t.Errorf("card of file %s = %q %v", index, card.text, buttonTexts(card.buttons))
		}
	}
	press(app, master, choose.messageID, "fo:0")
	if card := lastEdit(t, telegramFake); !hasButton(card.buttons, "📥 Send") {
		t.Fatalf("finished file's card = %q %v", card.text, buttonTexts(card.buttons))
	}
	press(app, master, choose.messageID, "fg:0")
	app.wg.Wait()
	if len(telegramFake.documents) != 2 || lastAnswer(t, telegramFake).text != "Sending e01.mkv…" {
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
	app.wg.Wait()
	if len(telegramFake.documents) != 1 || telegramFake.documents[0].content != "film" {
		t.Fatalf("documents = %+v", telegramFake.documents)
	}
}

func TestGetRefusesWithoutADataRootOrOutsideIt(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "downloads")
	torrent, files := showTorrent(t, root)

	app, telegramFake, _ := fileApp(t, "", torrent, files)
	if got := lastSentText(t, telegramFake, app, "get aaaaaaa 1"); got != "get: sending files is off; set -data-root to the directory on this machine where rTorrent keeps data" {
		t.Fatalf("without -data-root = %q", got)
	}

	elsewhere := filepath.Join(base, "elsewhere")
	app, telegramFake, _ = fileApp(t, elsewhere, torrent, files)
	if got := lastSentText(t, telegramFake, app, "get aaaaaaa 1"); !strings.Contains(got, "not strictly inside") {
		t.Fatalf("outside -data-root = %q", got)
	}
	// Nothing outside -data-root is marked 📥 or offers a Send button, and
	// its card says why.
	command(app, "get aaaaaaa")
	choose := nextSent(t, telegramFake)
	if strings.Count(choose.text, "📥") != 1 {
		t.Fatalf("files outside -data-root = %q", choose.text)
	}
	press(app, master, choose.messageID, "fo:0")
	if card := lastEdit(t, telegramFake); hasButton(card.buttons, "📥 Send") || !strings.Contains(card.text, "\nIt cannot be sent: ") ||
		!strings.Contains(card.text, "not strictly inside") {
		t.Fatalf("card outside -data-root = %q %v", card.text, buttonTexts(card.buttons))
	}

	// What is on disk counts, whatever rTorrent reports.
	files = append(files, rtapi.File{Index: 3, Path: "Extras", Size: 3, Chunks: 1, CompletedChunks: 1, Priority: rtapi.FileNormal})
	app, telegramFake, _ = fileApp(t, root, torrent, files)
	if got := lastSentText(t, telegramFake, app, "get aaaaaaa 4"); got != "get: Extras is not a regular file under 50 MB" {
		t.Fatalf("directory = %q", got)
	}

	secret := filepath.Join(base, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "show", "escape.mkv")); err != nil {
		t.Logf("symlink checks unavailable: %v", err)
		return
	}
	files = append(files, rtapi.File{Index: 4, Path: "escape.mkv", Size: 6, Chunks: 1, CompletedChunks: 1, Priority: rtapi.FileNormal})
	app, telegramFake, _ = fileApp(t, root, torrent, files)
	if got := lastSentText(t, telegramFake, app, "get aaaaaaa 5"); !strings.HasPrefix(got, "get: read escape.mkv") {
		t.Fatalf("symlink escape = %q", got)
	}
	if len(telegramFake.documents) != 0 {
		t.Fatal("a file outside -data-root was sent")
	}
}

// A file that cannot be read when its turn comes is reported in the chat,
// since the reply that it was being sent has already gone.
func TestGetSaysWhenSendingFails(t *testing.T) {
	root := t.TempDir()
	torrent, files := showTorrent(t, root)
	app, telegramFake, _ := fileApp(t, root, torrent, files)
	// Another file is still being sent, so this one waits its turn, and
	// disappears meanwhile.
	app.uploadMu.Lock()
	if got := lastSentText(t, telegramFake, app, "get aaaaaaa 1"); got != "Sending e01.mkv (11 B)…" {
		t.Fatalf("get aaaaaaa 1 = %q", got)
	}
	if err := os.Remove(filepath.Join(root, "show", "e01.mkv")); err != nil {
		t.Fatal(err)
	}
	app.uploadMu.Unlock()
	app.wg.Wait()
	if failed := nextSent(t, telegramFake).text; !strings.HasPrefix(failed, "get: read e01.mkv: ") {
		t.Fatalf("failure = %q", failed)
	}
}
