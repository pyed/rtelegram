package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/pyed/rtapi"
)

const indexerKey = "INDEXERKEY123"

// fakeIndexer is a Torznab endpoint at /api that requires indexerKey, and
// serves .torrent downloads at /dl/NAME, with /dl/redirect sending a magnet.
type fakeIndexer struct {
	server  *httptest.Server
	mu      sync.Mutex
	items   map[string][]string // query → item XML
	queries []string
}

func newFakeIndexer(t *testing.T) *fakeIndexer {
	t.Helper()
	ix := &fakeIndexer{items: make(map[string][]string)}
	ix.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("apikey") != indexerKey {
			io.WriteString(w, `<?xml version="1.0"?><error code="100" description="Invalid API Key"/>`)
			return
		}
		switch {
		case r.URL.Path == "/api":
			ix.mu.Lock()
			query := r.URL.Query().Get("q")
			ix.queries = append(ix.queries, r.URL.Query().Get("t")+":"+query)
			items := strings.Join(ix.items[query], "")
			ix.mu.Unlock()
			fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel>%s</channel></rss>`, items)
		case r.URL.Path == "/dl/redirect":
			http.Redirect(w, r, magnetFor("redirected"), http.StatusFound)
		case r.URL.Path == "/dl/file":
			io.WriteString(w, "d4:infod4:name4:testee")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ix.server.Close)
	return ix
}

func (ix *fakeIndexer) setItems(query string, items ...string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.items[query] = items
}

func (ix *fakeIndexer) download(name string) string {
	return ix.server.URL + "/dl/" + name + "?apikey=" + indexerKey
}

// hashOf derives a stable info-hash from a name.
func hashOf(name string) string {
	sum := sha1.Sum([]byte(name))
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

func magnetFor(name string) string {
	return "magnet:?xt=urn:btih:" + hashOf(name) + "&dn=" + url.QueryEscape(name)
}

func torznabItem(title, link string, size uint64, seeders int) string {
	return fmt.Sprintf(`<item><title>%s</title><guid>guid-%s</guid><link>%s</link><size>%d</size>`+
		`<prowlarrindexer id="1">TestIndexer</prowlarrindexer>`+
		`<torznab:attr name="seeders" value="%d"/></item>`, html.EscapeString(title), html.EscapeString(title), html.EscapeString(link), size, seeders)
}

// loadByName makes the fake rTorrent load a torrent for any magnet made by
// magnetFor, and for the /dl/file download.
func loadByName(rtorrentFake *fakeRtorrent, names ...string) {
	fileHash := sha1.Sum([]byte("d4:name4:teste"))
	rtorrentFake.onLoad(func(body string) *rtapi.Torrent {
		if strings.Contains(body, "load.raw") {
			return &rtapi.Torrent{Name: "downloaded file", Hash: strings.ToUpper(hex.EncodeToString(fileHash[:]))}
		}
		for _, name := range names {
			if strings.Contains(body, hashOf(name)) {
				return &rtapi.Torrent{Name: name, Hash: hashOf(name)}
			}
		}
		return nil
	})
}

func TestIndexerSearchParsesTorznab(t *testing.T) {
	ix := newFakeIndexer(t)
	ix.setItems("ubuntu",
		torznabItem("Ubuntu 24.04", ix.download("file"), 6<<30, 120),
		`<item><title>Enclosure only</title><enclosure url="https://example.org/e.torrent" length="2048"/></item>`,
		`<item><title>Magnet attr</title><link>https://example.org/m.torrent</link>`+
			`<torznab:attr name="magneturl" value="magnet:?xt=urn:btih:`+hashOf("m")+`"/><torznab:attr name="infohash" value="`+hashOf("m")+`"/></item>`,
		`<item><title>No link</title></item>`,
	)
	results, err := newIndexer(ix.server.URL+"/api", indexerKey).search(context.Background(), "ubuntu")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("results = %+v", results)
	}
	if r := results[0]; r.Title != "Ubuntu 24.04" || r.Size != 6<<30 || r.Seeders != 120 || r.Indexer != "TestIndexer" || r.GUID != "guid-Ubuntu 24.04" {
		t.Fatalf("first result = %+v", r)
	}
	if r := results[1]; r.Link != "https://example.org/e.torrent" || r.Size != 2048 {
		t.Fatalf("enclosure result = %+v", r)
	}
	if r := results[2]; !strings.HasPrefix(r.Link, "magnet:") || r.key() != hashOf("m") {
		t.Fatalf("magnet result = %+v", r)
	}
	if ix.queries[0] != "search:ubuntu" {
		t.Fatalf("queries = %v", ix.queries)
	}
}

func TestIndexerErrorsHideTheKey(t *testing.T) {
	ix := newFakeIndexer(t)
	if _, err := newIndexer(ix.server.URL+"/api", "wrong").search(context.Background(), "x"); err == nil || err.Error() != "indexer: Invalid API Key" {
		t.Fatalf("wrong key = %v", err)
	}
	if _, err := newIndexer(ix.server.URL+"/missing", indexerKey).search(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("missing endpoint = %v", err)
	}
	address := ix.server.URL + "/api"
	ix.server.Close()
	_, err := newIndexer(address, indexerKey).search(context.Background(), "x")
	if err == nil || strings.Contains(err.Error(), indexerKey) {
		t.Fatalf("connection error = %v", err)
	}
}

func TestFindShowsResultsAndAddsThem(t *testing.T) {
	ix := newFakeIndexer(t)
	ix.setItems("linux",
		torznabItem("Few seeders", magnetFor("few"), 1<<30, 3),
		torznabItem("Torrent file", ix.download("file"), 2<<30, 50),
		torznabItem("Redirects", ix.download("redirect"), 3<<30, 20),
	)
	app, telegramFake, rtorrentFake := buttonApp(t, nil)
	app.indexer = newIndexer(ix.server.URL+"/api", indexerKey)
	loadByName(rtorrentFake, "few", "redirected")

	command(app, "find linux")
	app.wg.Wait()
	results := nextSent(t, telegramFake)
	if !strings.HasPrefix(results.text, "Results for linux.") || !strings.Contains(results.text, "1. Torrent file\n2.0 GiB, 50 seeders, TestIndexer") {
		t.Fatalf("results = %q", results.text)
	}
	if got := buttonTexts(results.buttons); len(got) != 3 || !strings.HasPrefix(got[0], "1. Torrent file=fd:0") || !strings.HasPrefix(got[2], "3. Few seeders=fd:2") {
		t.Fatalf("buttons = %v", got)
	}

	for index, want := range []string{"Added: <", "Added: <", "Added: <"} {
		press(app, master, results.messageID, fmt.Sprintf("fd:%d", index))
		app.wg.Wait()
		if got := nextSent(t, telegramFake).text; !strings.HasPrefix(got, want) {
			t.Fatalf("adding result %d = %q", index, got)
		}
	}
	if raw, links := len(rtorrentFake.called("load.raw_start")), len(rtorrentFake.called("load.start_verbose")); raw != 1 || links != 2 {
		t.Fatalf("raw loads = %d, link loads = %d", raw, links)
	}
	for _, request := range rtorrentFake.requestsContaining(indexerKey) {
		t.Fatalf("the indexer key reached rTorrent: %s", request)
	}
}

func TestFindNeedsAnIndexerAndAQuery(t *testing.T) {
	app, telegramFake, _ := buttonApp(t, nil)
	if got := lastSentText(t, telegramFake, app, "find ubuntu"); got != "find: no indexer is configured; set -indexer-url and RT_INDEXER_KEY" {
		t.Fatalf("without an indexer = %q", got)
	}
	ix := newFakeIndexer(t)
	app.indexer = newIndexer(ix.server.URL+"/api", indexerKey)
	if got := lastSentText(t, telegramFake, app, "find"); got != "find: use find QUERY" {
		t.Fatalf("without a query = %q", got)
	}
	command(app, "find nothing")
	app.wg.Wait()
	if got := nextSent(t, telegramFake).text; got != "find: nothing found for nothing" {
		t.Fatalf("no results = %q", got)
	}
}

func TestWatchAddsOnlyNewReleases(t *testing.T) {
	ix := newFakeIndexer(t)
	ix.setItems("show", torznabItem("Show S01E01", magnetFor("e1"), 1<<30, 9))
	app, telegramFake, rtorrentFake := buttonApp(t, nil)
	app.indexer = newIndexer(ix.server.URL+"/api", indexerKey)
	app.downloadRoot = "/downloads"
	var names []string
	for i := 1; i <= 9; i++ {
		names = append(names, fmt.Sprintf("e%d", i))
	}
	loadByName(rtorrentFake, names...)

	if got := lastSentText(t, telegramFake, app, "watch add tv show l=tv d=tv"); got != "Watching tv: show. The 1 results there are now are skipped; new ones are added as they appear." {
		t.Fatalf("watch add = %q", got)
	}
	app.checkWatches(context.Background())
	app.wg.Wait()
	if sent := drain(telegramFake); len(sent) != 0 || len(rtorrentFake.loadCalls()) != 0 {
		t.Fatalf("existing releases were added: %q", sent)
	}

	var items []string
	for _, name := range names {
		items = append(items, torznabItem("Show "+name, magnetFor(name), 1<<30, 9))
	}
	ix.setItems("show", items...)
	app.checkWatches(context.Background())
	app.wg.Wait()
	loads := rtorrentFake.loadCalls()
	if len(loads) != maxWatchAddsPerCheck {
		t.Fatalf("one check added %d releases", len(loads))
	}
	for _, load := range loads {
		if !strings.Contains(strings.Join(load, " "), `d.custom1.set="tv"`) || !strings.Contains(strings.Join(load, " "), `d.directory.set="/downloads/tv"`) {
			t.Fatalf("load without the rule's label and directory: %q", load)
		}
	}
	if sent := drain(telegramFake); len(sent) != maxWatchAddsPerCheck || !strings.HasPrefix(sent[0], "Added: <") {
		t.Fatalf("announcements = %q", sent)
	}
	app.checkWatches(context.Background())
	app.wg.Wait()
	app.checkWatches(context.Background())
	app.wg.Wait()
	if loads := len(rtorrentFake.loadCalls()); loads != len(names)-1 {
		t.Fatalf("after more checks, %d releases were added, want %d", loads, len(names)-1)
	}
}

func TestWatchListsAndDeletesRules(t *testing.T) {
	ix := newFakeIndexer(t)
	app, telegramFake, _ := buttonApp(t, nil)
	app.indexer = newIndexer(ix.server.URL+"/api", indexerKey)
	for text, want := range map[string]string{
		"watch":             "No watch rules. Add one with watch add NAME QUERY.",
		"watch add":         "watch: use watch, watch add NAME QUERY [l=LABEL] [d=DIR], or watch del NAME",
		"watch add x l=tv":  "watch: give a search query",
		"watch del missing": "watch: no rule named missing",
	} {
		if got := lastSentText(t, telegramFake, app, text); got != want {
			t.Errorf("%s = %q, want %q", text, got, want)
		}
	}
	lastSentText(t, telegramFake, app, "watch add news some query l=news")
	if got := lastSentText(t, telegramFake, app, "watch add NEWS other"); got != "watch: a rule named NEWS already exists" {
		t.Fatalf("duplicate = %q", got)
	}
	if got := lastSentText(t, telegramFake, app, "watch"); got != "Watch rules:\nnews: some query, label news" {
		t.Fatalf("list = %q", got)
	}
	if got := lastSentText(t, telegramFake, app, "watch del news"); got != "Stopped watching news" {
		t.Fatalf("del = %q", got)
	}
}

func TestIndexerConfig(t *testing.T) {
	env := map[string]string{"RT_TOKEN": "123:SECRET", "RT_MASTERS": "7", "RT_INDEXER_URL": "http://prowlarr:9696/1/api", "RT_INDEXER_KEY": "k"}
	getenv := func(name string) string { return env[name] }
	cfg, err := parseConfig(nil, getenv, io.Discard)
	if err != nil || cfg.indexerURL != "http://prowlarr:9696/1/api" || cfg.indexerKey != "k" || cfg.feedInterval != defaultFeedInterval {
		t.Fatalf("config = %+v, %v", cfg, err)
	}
	for _, args := range [][]string{{"-indexer-url", "prowlarr:9696"}, {"-feed-interval", "1m"}} {
		if _, err := parseConfig(args, getenv, io.Discard); err == nil {
			t.Errorf("parseConfig(%v) succeeded", args)
		}
	}
	if got := indexerName("http://prowlarr:9696/1/api?apikey=k"); got != "prowlarr:9696/1/api" {
		t.Fatalf("indexerName = %q", got)
	}
}
