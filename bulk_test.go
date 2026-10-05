package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pyed/rtapi"
)

func greekTorrents() rtapi.Torrents {
	example, _ := url.Parse("https://tracker.example.org/announce")
	return rtapi.Torrents{
		{Name: "Alpha", Hash: strings.Repeat("A", 40), State: rtapi.Seeding, Completed: 1 << 20, Tracker: example},
		{Name: "Beta", Hash: strings.Repeat("B", 40), State: rtapi.Seeding, Completed: 1 << 20, Tracker: example},
		{Name: "Gamma", Hash: strings.Repeat("C", 40), State: rtapi.Seeding, Completed: 1 << 20},
		{Name: "Delta", Hash: strings.Repeat("D", 40), State: rtapi.Stopped},
	}
}

// hashesCalled lists the torrents each call of method was for.
func hashesCalled(rtorrentFake *fakeRtorrent, method string) []string {
	var hashes []string
	for _, args := range rtorrentFake.called(method) {
		hashes = append(hashes, args[0][:1])
	}
	return hashes
}

// A bulk action asks first, naming the torrents, and then acts only on
// those of them that the list still shows: never on one that joined the
// list after the menu opened.
func TestBulkActionsAskAndActOnlyOnWhatTheListShowed(t *testing.T) {
	app, telegramFake, rtorrentFake := buttonApp(t, greekTorrents())
	command(app, "seeding")
	list := nextSent(t, telegramFake)
	if !hasButton(list.buttons, "☰ All 3…") {
		t.Fatalf("list buttons = %v", buttonTexts(list.buttons))
	}
	press(app, master, list.messageID, "bk")
	menu := lastEdit(t, telegramFake)
	if menu.text != "What should happen to 3 torrents seeding?\n• Alpha\n• Beta\n• Gamma" {
		t.Fatalf("menu = %q", menu.text)
	}
	if got := strings.Join(buttonTexts(menu.buttons), ","); got != "▶ Start=ba:start,⏸ Stop=ba:stop,🔍 Check=ba:check,🏷 Label=ba:pick,🗑 Remove=ba:del,« Back=back" {
		t.Fatalf("menu buttons = %s", got)
	}
	press(app, master, list.messageID, "ba:stop")
	confirm := lastEdit(t, telegramFake)
	if confirm.text != "Stop 3 torrents seeding?\n• Alpha\n• Beta\n• Gamma" || strings.Join(buttonTexts(confirm.buttons), ",") != "⏸ Stop 3=bc,Cancel=back" {
		t.Fatalf("confirmation = %q %v", confirm.text, buttonTexts(confirm.buttons))
	}

	// Meanwhile Gamma stops, and Epsilon starts seeding.
	rtorrentFake.set(func(f *fakeRtorrent) {
		f.torrents[2].State = rtapi.Stopped
		f.torrents = append(f.torrents, &rtapi.Torrent{Name: "Epsilon", Hash: strings.Repeat("E", 40), State: rtapi.Seeding})
	})
	press(app, master, list.messageID, "bc")
	press(app, master, list.messageID, "bc") // a second tap does nothing
	app.wg.Wait()
	if stopped := hashesCalled(rtorrentFake, "d.stop"); strings.Join(stopped, "") != "AB" {
		t.Fatalf("stopped %v, want Alpha and Beta only", stopped)
	}
	if answers := telegramFake.answers; answers[len(answers)-2].text != "Working on it…" || !answers[len(answers)-1].alert {
		t.Fatalf("answers = %+v", answers)
	}
	outcome := lastEdit(t, telegramFake)
	if outcome.text != "Stopped 2 torrents.\n• Alpha\n• Beta\n\nNot touched: 1 torrent that left the list meanwhile." ||
		strings.Join(buttonTexts(outcome.buttons), ",") != "« Back to the list=back" {
		t.Fatalf("outcome = %q %v", outcome.text, buttonTexts(outcome.buttons))
	}
	press(app, master, list.messageID, "bc")
	if answer := lastAnswer(t, telegramFake); !answer.alert || len(rtorrentFake.called("d.stop")) != 2 {
		t.Fatalf("bc on the outcome = %+v", answer)
	}
	press(app, master, list.messageID, "back")
	if back := lastEdit(t, telegramFake); !strings.Contains(back.text, "Epsilon") || !hasButton(back.buttons, "☰ All 3…") {
		t.Fatalf("back to the list = %q %v", back.text, buttonTexts(back.buttons))
	}

	// Cancel goes back to the list, having done nothing.
	press(app, master, list.messageID, "bk")
	press(app, master, list.messageID, "ba:start")
	press(app, master, list.messageID, "back")
	if back := lastEdit(t, telegramFake); !strings.Contains(back.text, "Alpha") || len(rtorrentFake.called("d.start")) != 0 {
		t.Fatalf("after cancelling = %q, %d starts", back.text, len(rtorrentFake.called("d.start")))
	}

	// Labelling goes through the label picker.
	press(app, master, list.messageID, "bk")
	press(app, master, list.messageID, "ba:label")
	if answer := lastAnswer(t, telegramFake); !answer.alert {
		t.Fatalf("ba:label = %+v", answer)
	}

	// When every torrent noted has left the list, the list says so.
	press(app, master, list.messageID, "back")
	press(app, master, list.messageID, "bk")
	rtorrentFake.set(func(f *fakeRtorrent) {
		for _, torrent := range f.torrents {
			torrent.State = rtapi.Stopped
		}
		f.torrents = append(f.torrents, &rtapi.Torrent{Name: "Zeta", Hash: strings.Repeat("F", 40), State: rtapi.Seeding})
	})
	press(app, master, list.messageID, "ba:stop")
	if shown, answer := lastEdit(t, telegramFake), lastAnswer(t, telegramFake); !strings.Contains(shown.text, "Zeta") ||
		answer.text != "None of those torrents is in the list any more." {
		t.Fatalf("after every torrent left = %q, %+v", shown.text, answer)
	}
}

// Data that disappears before it is deleted is reported, not deleted again.
func TestBulkRemovalWithDataReportsDataThatDisappeared(t *testing.T) {
	root := t.TempDir()
	app, telegramFake, rtorrentFake := buttonApp(t, rtapi.Torrents{dataTorrent(t, "x-a", "A", filepath.Join(root, "a"), false)})
	app.dataRoot = root
	rtorrentFake.set(func(f *fakeRtorrent) {
		f.onCall = func(method string) {
			if method == "d.erase" {
				os.RemoveAll(filepath.Join(root, "a"))
			}
		}
	})
	command(app, "search x-")
	list := nextSent(t, telegramFake)
	press(app, master, list.messageID, "bk:deldata")
	press(app, master, list.messageID, "bc")
	app.wg.Wait()
	if outcome := lastEdit(t, telegramFake).text; !strings.HasPrefix(outcome, "Removed 1 torrent from rTorrent, but kept their data:\n• x-a: validate torrent data path: ") {
		t.Fatalf("outcome = %q", outcome)
	}
}

func TestBulkActionsEachCallRTorrent(t *testing.T) {
	for action, method := range map[string]string{bulkStart: "d.start", bulkStop: "d.stop", bulkCheck: "d.check_hash", bulkDel: "d.erase"} {
		app, telegramFake, rtorrentFake := buttonApp(t, greekTorrents())
		command(app, "seeding")
		list := nextSent(t, telegramFake)
		press(app, master, list.messageID, "bk")
		press(app, master, list.messageID, "ba:"+action)
		press(app, master, list.messageID, "bc")
		app.wg.Wait()
		if got := strings.Join(hashesCalled(rtorrentFake, method), ""); got != "ABC" {
			t.Errorf("%s called %s for %q", action, method, got)
		}
	}
}

// A failure says rTorrent may have applied the action to some torrents.
func TestBulkActionFailuresWarnOfPartialChanges(t *testing.T) {
	app, telegramFake, rtorrentFake := buttonApp(t, greekTorrents())
	rtorrentFake.set(func(f *fakeRtorrent) { f.faults = map[string]bool{"d.start " + strings.Repeat("B", 40): true} })
	command(app, "seeding")
	list := nextSent(t, telegramFake)
	press(app, master, list.messageID, "bk")
	press(app, master, list.messageID, "ba:start")
	press(app, master, list.messageID, "bc")
	app.wg.Wait()
	if outcome := lastEdit(t, telegramFake); !strings.HasPrefix(outcome.text, "start: ") || !strings.Contains(outcome.text, "refresh before retrying") {
		t.Fatalf("outcome = %q", outcome.text)
	}
}

// The list of every torrent offers no removal: one tap must never remove
// the whole library.
func TestBulkRemovalIsNotOfferedForTheWholeLibrary(t *testing.T) {
	app, telegramFake, rtorrentFake := buttonApp(t, greekTorrents())
	app.dataRoot = t.TempDir()
	command(app, "list")
	list := nextSent(t, telegramFake)
	press(app, master, list.messageID, "bk")
	if menu := lastEdit(t, telegramFake); hasButton(menu.buttons, "🗑 Remove") || hasButton(menu.buttons, "💣 Remove + data") ||
		!strings.HasPrefix(menu.text, "What should happen to all 4 torrents?") {
		t.Fatalf("menu of the whole library = %q %v", menu.text, buttonTexts(menu.buttons))
	}
	for _, data := range []string{"ba:del", "ba:deldata"} {
		press(app, master, list.messageID, data)
		if answer := lastAnswer(t, telegramFake); !answer.alert {
			t.Errorf("%s = %+v", data, answer)
		}
	}
	command(app, "list")
	list = nextSent(t, telegramFake)
	press(app, master, list.messageID, "bk:deldata")
	if answer := lastAnswer(t, telegramFake); !answer.alert || len(rtorrentFake.called("d.erase")) != 0 {
		t.Fatalf("bk:deldata on the whole library = %+v", answer)
	}

	// A tracker's list is not the whole library.
	command(app, "list example")
	list = nextSent(t, telegramFake)
	press(app, master, list.messageID, "bk")
	if menu := lastEdit(t, telegramFake); !hasButton(menu.buttons, "🗑 Remove") || !hasButton(menu.buttons, "💣 Remove + data") {
		t.Fatalf("menu of a tracker = %q %v", menu.text, buttonTexts(menu.buttons))
	}
}

func TestBulkLabelsPickThenAsk(t *testing.T) {
	app, telegramFake, rtorrentFake := buttonApp(t, labelledTorrents())
	command(app, "labels movies")
	list := nextSent(t, telegramFake)
	press(app, master, list.messageID, "bk")
	press(app, master, list.messageID, "ba:pick")
	picker := lastEdit(t, telegramFake)
	if !strings.HasPrefix(picker.text, "Label 2 torrents labelled movies: choose a label, or reply to this message with a new one.") ||
		strings.Join(buttonTexts(picker.buttons), ",") != "🏷 Movies=lb:0,🏷 TV Shows=lb:1,✖ No label=lb:-,« Back=back" {
		t.Fatalf("picker = %q %v", picker.text, buttonTexts(picker.buttons))
	}
	press(app, master, list.messageID, "lb:1")
	confirm := lastEdit(t, telegramFake)
	if confirm.text != "Label 2 torrents labelled movies: TV Shows?\n• Alien\n• Brazil" || !hasButton(confirm.buttons, "🏷 Label 2") {
		t.Fatalf("confirmation = %q %v", confirm.text, buttonTexts(confirm.buttons))
	}
	if labelOfFake(rtorrentFake, 0) != "Movies" {
		t.Fatal("labelled before confirming")
	}
	press(app, master, list.messageID, "bc")
	app.wg.Wait()
	if labelOfFake(rtorrentFake, 0) != "TV%20Shows" || labelOfFake(rtorrentFake, 1) != "TV%20Shows" || labelOfFake(rtorrentFake, 3) != "" {
		t.Fatalf("labels = %q %q %q", labelOfFake(rtorrentFake, 0), labelOfFake(rtorrentFake, 1), labelOfFake(rtorrentFake, 3))
	}
	if outcome := lastEdit(t, telegramFake); !strings.HasPrefix(outcome.text, "Labelled 2 torrents: TV Shows.") {
		t.Fatalf("outcome = %q", outcome.text)
	}

	// A new label comes as a reply to the picker.
	command(app, "labels tv shows")
	list = nextSent(t, telegramFake)
	press(app, master, list.messageID, "bk")
	press(app, master, list.messageID, "ba:pick")
	replyTo(app, list.messageID, "Classics")
	if got := nextSent(t, telegramFake).text; got != "Confirm on the message you replied to." {
		t.Fatalf("reply = %q", got)
	}
	if confirm := lastEdit(t, telegramFake); !strings.HasPrefix(confirm.text, "Label 3 torrents labelled tv shows: Classics?") {
		t.Fatalf("confirmation = %q", confirm.text)
	}
	press(app, master, list.messageID, "back")
	press(app, master, list.messageID, "bk")
	press(app, master, list.messageID, "ba:pick")
	press(app, master, list.messageID, "lb:-")
	if confirm := lastEdit(t, telegramFake); !strings.HasPrefix(confirm.text, "Remove the label of 3 torrents labelled tv shows?") || !hasButton(confirm.buttons, "✖ Unlabel 3") {
		t.Fatalf("unlabel confirmation = %q %v", confirm.text, buttonTexts(confirm.buttons))
	}
	press(app, master, list.messageID, "bc")
	app.wg.Wait()
	if labelOfFake(rtorrentFake, 2) != "" {
		t.Fatalf("label after unlabelling = %q", labelOfFake(rtorrentFake, 2))
	}
}

// dataTorrent is a seeding multi-file torrent whose data is dir, which it
// creates unless absent.
func dataTorrent(t *testing.T, name, hash, dir string, absent bool) *rtapi.Torrent {
	t.Helper()
	if !absent {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".bin"), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &rtapi.Torrent{Name: name, Hash: strings.Repeat(hash, 40), State: rtapi.Seeding, Path: dir, Directory: dir, MultiFile: true, Completed: 1 << 20}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

// Removing torrents with their data keeps deldata's safeguards for each:
// data is deleted only inside -data-root, and only once rTorrent has let go
// of every torrent that uses it.
func TestBulkRemovalWithDataDeletesOnlyWhatNothingElseUses(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "downloads")
	in := func(names ...string) string { return filepath.Join(append([]string{root}, names...)...) }
	torrents := rtapi.Torrents{
		dataTorrent(t, "x-a", "A", in("a"), false),
		dataTorrent(t, "x-b", "B", in("b"), false),
		dataTorrent(t, "keep-c", "C", in("b"), false), // seeds x-b's data
		dataTorrent(t, "x-d", "D", in("d"), false),
		dataTorrent(t, "x-e", "E", in("d"), false), // seeds x-d's data, and goes too
		dataTorrent(t, "x-f", "F", filepath.Join(base, "elsewhere"), false),
		dataTorrent(t, "x-g", "1", in("never-downloaded"), true),
		dataTorrent(t, "x-h", "2", in("h"), false), // rTorrent refuses to remove it
		dataTorrent(t, "x-i", "3", in("h", "sub"), false),
	}
	app, telegramFake, rtorrentFake := buttonApp(t, torrents)
	app.dataRoot = root
	rtorrentFake.set(func(f *fakeRtorrent) { f.faults = map[string]bool{"d.erase " + strings.Repeat("2", 40): true} })

	command(app, "search x-")
	list := nextSent(t, telegramFake)
	press(app, master, list.messageID, "bk")
	press(app, master, list.messageID, "ba:deldata")
	if confirm := lastEdit(t, telegramFake); !strings.HasPrefix(confirm.text, `Remove 8 torrents whose name contains "x-" and delete their data, 8.0 MiB, from disk? This cannot be undone.`) ||
		!hasButton(confirm.buttons, "💣 Delete 8 + data") {
		t.Fatalf("confirmation = %q %v", confirm.text, buttonTexts(confirm.buttons))
	}
	press(app, master, list.messageID, "bc")
	app.wg.Wait()

	if erased := strings.Join(hashesCalled(rtorrentFake, "d.erase"), ""); erased != "ADE23" {
		t.Fatalf("asked to remove %q", erased)
	}
	for path, want := range map[string]bool{in("a"): false, in("b"): true, in("d"): false, filepath.Join(base, "elsewhere"): true, in("h"): true, in("h", "sub"): true} {
		if exists(t, path) != want {
			t.Errorf("%s exists = %v, want %v", path, !want, want)
		}
	}
	outcome := lastEdit(t, telegramFake).text
	for _, want := range []string{
		"Removed 3 torrents and deleted their data, 3.0 MiB.\n• x-a\n• x-d\n• x-e",
		"\n\nRemoved 1 torrent from rTorrent, but kept their data:\n• x-i: its data overlaps that of x-h, which is loaded",
		"\n\nKept 4 torrents loaded, with their data:\n• x-b: its data overlaps that of keep-c, which stays\n• x-f: torrent data path is not strictly inside the configured data root\n" +
			"• x-g: it has no data where rTorrent says, so remove it without its data instead\n• x-h: rTorrent did not remove it: ",
	} {
		if !strings.Contains(outcome, want) {
			t.Errorf("outcome lacks %q:\n%s", want, outcome)
		}
	}
}

// Nothing is removed while rTorrent has not said where some torrent's data
// is, since that torrent may share it.
func TestBulkRemovalWithDataNeedsEveryTorrentsLocation(t *testing.T) {
	root := t.TempDir()
	torrents := rtapi.Torrents{
		dataTorrent(t, "x-a", "A", filepath.Join(root, "a"), false),
		{Name: "unknown", Hash: strings.Repeat("B", 40), State: rtapi.Stopped},
	}
	app, telegramFake, rtorrentFake := buttonApp(t, torrents)
	app.dataRoot = root
	command(app, "search x-")
	list := nextSent(t, telegramFake)
	press(app, master, list.messageID, "bk:deldata")
	press(app, master, list.messageID, "bc")
	app.wg.Wait()
	if outcome := lastEdit(t, telegramFake).text; outcome != "Nothing was done: rTorrent did not report where unknown keeps its data, so it may share these torrents' data." ||
		len(rtorrentFake.called("d.erase")) != 0 || !exists(t, filepath.Join(root, "a")) {
		t.Fatalf("outcome = %q", outcome)
	}
}

// When the torrents left cannot be listed after removing, which went is
// unknown, so no data is deleted.
func TestBulkRemovalWithDataDeletesNothingWhenItCannotCheck(t *testing.T) {
	root := t.TempDir()
	app, telegramFake, rtorrentFake := buttonApp(t, rtapi.Torrents{
		dataTorrent(t, "x-a", "A", filepath.Join(root, "a"), false),
		dataTorrent(t, "x-b", "B", filepath.Join(root, "b"), false),
	})
	app.dataRoot = root
	rtorrentFake.set(func(f *fakeRtorrent) {
		f.onCall = func(method string) {
			if method == "d.erase" {
				f.faults = map[string]bool{"d.multicall2": true} // the fake's lock is held
			}
		}
	})
	command(app, "search x-")
	list := nextSent(t, telegramFake)
	press(app, master, list.messageID, "bk:deldata")
	press(app, master, list.messageID, "bc")
	app.wg.Wait()
	if outcome := lastEdit(t, telegramFake).text; !strings.HasPrefix(outcome, "rTorrent was asked to remove 2 torrents, but they could not be listed afterwards (") ||
		!strings.HasSuffix(outcome, "so no data was deleted. Check the list before trying again.") {
		t.Fatalf("outcome = %q", outcome)
	}
	if !exists(t, filepath.Join(root, "a")) || !exists(t, filepath.Join(root, "b")) {
		t.Fatal("data was deleted")
	}
}

func TestBulkRemovalWithDataRefusesSymbolicLinks(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "downloads")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{root, outside} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	torrent := &rtapi.Torrent{Name: "x-link", Hash: strings.Repeat("A", 40), State: rtapi.Seeding, Path: link, Directory: link, MultiFile: true}
	app, telegramFake, rtorrentFake := buttonApp(t, rtapi.Torrents{torrent})
	app.dataRoot = root
	command(app, "search x-")
	list := nextSent(t, telegramFake)
	press(app, master, list.messageID, "bk:deldata")
	press(app, master, list.messageID, "bc")
	app.wg.Wait()
	if outcome := lastEdit(t, telegramFake).text; !strings.Contains(outcome, "• x-link: torrent data path must not be a symbolic link") ||
		len(rtorrentFake.called("d.erase")) != 0 || !exists(t, outside) {
		t.Fatalf("outcome = %q", outcome)
	}
}

// Data is checked again just before it is deleted: a symbolic link put in
// its place meanwhile is not followed, and a torrent that appeared meanwhile
// without saying where its data is keeps every torrent's data.
func TestBulkRemovalWithDataChecksAgainBeforeDeleting(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "downloads")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(base, "probe")); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	app, telegramFake, rtorrentFake := buttonApp(t, rtapi.Torrents{dataTorrent(t, "x-a", "A", filepath.Join(root, "a"), false)})
	app.dataRoot = root
	rtorrentFake.set(func(f *fakeRtorrent) {
		f.onCall = func(method string) {
			if method == "d.erase" {
				os.RemoveAll(filepath.Join(root, "a"))
				os.Symlink(outside, filepath.Join(root, "a"))
			}
		}
	})
	command(app, "search x-")
	list := nextSent(t, telegramFake)
	press(app, master, list.messageID, "bk:deldata")
	press(app, master, list.messageID, "bc")
	app.wg.Wait()
	if outcome := lastEdit(t, telegramFake).text; !strings.Contains(outcome, "• x-a: torrent data path must not be a symbolic link") || !exists(t, outside) {
		t.Fatalf("outcome = %q", outcome)
	}
}

func TestBulkRemovalWithDataKeepsDataWhenATorrentOfUnknownLocationAppears(t *testing.T) {
	root := t.TempDir()
	app, telegramFake, rtorrentFake := buttonApp(t, rtapi.Torrents{dataTorrent(t, "x-a", "A", filepath.Join(root, "a"), false)})
	app.dataRoot = root
	rtorrentFake.set(func(f *fakeRtorrent) {
		f.onCall = func(method string) {
			if method == "d.erase" && len(f.torrents) == 1 {
				f.torrents = append(f.torrents, &rtapi.Torrent{Name: "newcomer", Hash: strings.Repeat("B", 40), State: rtapi.Stopped})
			}
		}
	})
	command(app, "search x-")
	list := nextSent(t, telegramFake)
	press(app, master, list.messageID, "bk:deldata")
	press(app, master, list.messageID, "bc")
	app.wg.Wait()
	if outcome := lastEdit(t, telegramFake).text; !strings.Contains(outcome, "• x-a: rTorrent did not report where newcomer keeps its data, so it may share it") ||
		!exists(t, filepath.Join(root, "a")) {
		t.Fatalf("outcome = %q", outcome)
	}
}

// Confirmations and outcomes name up to ten torrents.
func TestBulkMessagesNameTenTorrents(t *testing.T) {
	var torrents rtapi.Torrents
	why := make(map[*rtapi.Torrent]string)
	for i := range 12 {
		torrent := &rtapi.Torrent{Name: fmt.Sprint("t", i)}
		torrents = append(torrents, torrent)
		why[torrent] = "reason"
	}
	if got := namesOf(torrents); strings.Count(got, "\n• t") != 10 || !strings.HasSuffix(got, "\n• t9\n• and 2 more") {
		t.Fatalf("names = %q", got)
	}
	if got := reasons(torrents, why); strings.Count(got, ": reason") != 10 || !strings.HasSuffix(got, "\n• t9: reason\n• and 2 more") {
		t.Fatalf("reasons = %q", got)
	}
}

func TestListsDescribeThemselves(t *testing.T) {
	for spec, want := range map[listSpec]string{
		{kind: "list"}:                         "all 2 torrents",
		{kind: "list", query: "example"}:       `2 torrents whose tracker matches "example"`,
		{kind: "down"}:                         "2 torrents downloading",
		{kind: "seeding"}:                      "2 torrents seeding",
		{kind: "checking"}:                     "2 torrents being checked",
		{kind: "paused"}:                       "2 torrents stopped",
		{kind: "errors"}:                       "2 torrents with errors",
		{kind: "search", query: "x"}:           `2 torrents whose name contains "x"`,
		{kind: "latest", count: 2}:             "the 2 torrents added last",
		{kind: "label", query: "Movies"}:       "2 torrents labelled Movies",
		{kind: "label"}:                        "2 torrents without a label",
		{kind: "unregistered"}:                 "2 torrents their tracker no longer has",
		{kind: "list", query: fmt.Sprint(1)}:   `2 torrents whose tracker matches "1"`,
		{kind: "search", query: "two words"}:   `2 torrents whose name contains "two words"`,
		{kind: "label", query: "Kids & Teens"}: "2 torrents labelled Kids & Teens",
	} {
		if got := spec.describe(2); got != want {
			t.Errorf("%+v = %q, want %q", spec, got, want)
		}
	}
	if got := (listSpec{kind: "down"}).describe(1); got != "1 torrent downloading" {
		t.Errorf("one = %q", got)
	}
}
