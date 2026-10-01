package main

import (
	"cmp"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot/models"
	"github.com/pyed/rtapi"
)

const (
	maxFindResults        = 8
	maxIndexerResponse    = 4 << 20
	maxWatchAddsPerCheck  = 5
	maxWatchSeen          = 500
	defaultFeedInterval   = 15 * time.Minute
	indexerRequestTimeout = 60 * time.Second
)

// indexer searches a Torznab endpoint, as served by Prowlarr or Jackett.
type indexer struct {
	endpoint string // without the API key
	key      string
	client   *http.Client
}

func newIndexer(endpoint, key string) *indexer {
	return &indexer{endpoint: endpoint, key: key, client: &http.Client{
		Timeout: indexerRequestTimeout,
		// Indexers redirect some downloads to magnet links; hand those back
		// instead of following them.
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if request.URL.Scheme == "magnet" {
				return http.ErrUseLastResponse
			}
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}}
}

// A searchResult is one release an indexer found.
type searchResult struct {
	Title    string
	Indexer  string
	Size     uint64
	Seeders  int
	Link     string // a .torrent download or a magnet link
	GUID     string
	InfoHash string
}

// key identifies a result across searches, for watch rules: by info-hash when
// the indexer or a magnet link gives one, so the same release is recognized
// under another title or from another indexer.
func (r searchResult) key() string {
	hash := r.InfoHash
	if hash == "" {
		hash, _ = magnetInfo(r.Link)
	}
	for _, key := range []string{strings.ToUpper(hash), r.GUID, r.Link} {
		if key != "" {
			return key
		}
	}
	return r.Title
}

type torznabFeed struct {
	XMLName     xml.Name
	Description string `xml:"description,attr"` // set on an <error> root
	Items       []struct {
		Title     string `xml:"title"`
		GUID      string `xml:"guid"`
		Link      string `xml:"link"`
		Size      uint64 `xml:"size"`
		Enclosure struct {
			URL    string `xml:"url,attr"`
			Length uint64 `xml:"length,attr"`
		} `xml:"enclosure"`
		Prowlarr string `xml:"prowlarrindexer"`
		Jackett  string `xml:"jackettindexer"`
		Attrs    []struct {
			Name  string `xml:"name,attr"`
			Value string `xml:"value,attr"`
		} `xml:"attr"`
	} `xml:"channel>item"`
}

// search asks the indexer for query, or for its latest releases when query is
// empty. Errors never include the API key.
func (ix *indexer) search(ctx context.Context, query string) ([]searchResult, error) {
	endpoint, err := url.Parse(ix.endpoint)
	if err != nil {
		return nil, fmt.Errorf("indexer URL: %w", err)
	}
	values := endpoint.Query()
	values.Set("t", "search")
	if query != "" {
		values.Set("q", query)
	}
	values.Set("apikey", ix.key)
	endpoint.RawQuery = values.Encode()
	body, err := ix.get(ctx, endpoint.String())
	if err != nil {
		return nil, err
	}
	var feed torznabFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("indexer returned something other than a Torznab feed: %w", err)
	}
	if feed.XMLName.Local == "error" {
		return nil, fmt.Errorf("indexer: %s", redact(ix.key, feed.Description))
	}
	results := make([]searchResult, 0, len(feed.Items))
	for _, item := range feed.Items {
		result := searchResult{Title: item.Title, GUID: item.GUID, Link: item.Link, Size: item.Size, Indexer: cmp.Or(item.Prowlarr, item.Jackett)}
		if result.Size == 0 {
			result.Size = item.Enclosure.Length
		}
		if result.Link == "" {
			result.Link = item.Enclosure.URL
		}
		for _, attr := range item.Attrs {
			switch attr.Name {
			case "seeders":
				result.Seeders, _ = strconv.Atoi(attr.Value)
			case "infohash":
				result.InfoHash = attr.Value
			case "magneturl":
				result.Link = attr.Value // a magnet needs no download from the indexer
			}
		}
		if result.Title != "" && result.Link != "" {
			results = append(results, result)
		}
	}
	return results, nil
}

// get fetches a URL from the indexer. A redirect to a magnet link returns the
// link itself, prefixed with "magnet:".
func (ix *indexer) get(ctx context.Context, address string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, errors.New("indexer: bad URL")
	}
	response, err := ix.client.Do(request)
	if err != nil {
		return nil, errors.New("indexer: " + redact(ix.key, err.Error()))
	}
	defer response.Body.Close()
	if location := response.Header.Get("Location"); strings.HasPrefix(location, "magnet:") {
		return []byte(location), nil
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("indexer: HTTP %s", response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxIndexerResponse+1))
	if err != nil {
		return nil, errors.New("indexer: " + redact(ix.key, err.Error()))
	}
	if len(body) > maxIndexerResponse {
		return nil, errors.New("indexer: response is too large")
	}
	return body, nil
}

// grab loads a search result into rTorrent and confirms it appeared. Magnet
// links go to rTorrent directly; .torrent downloads are fetched by the bot,
// which knows the indexer's address and key.
func (a *application) grab(ctx context.Context, chatID int64, label string, result searchResult, options rtapi.DotTorrentWithOptions) {
	if strings.HasPrefix(result.Link, "magnet:") {
		a.addLink(ctx, chatID, label, result.Link, result.Title, options)
		return
	}
	data, err := a.indexer.get(ctx, result.Link)
	if err != nil {
		a.send(ctx, chatID, label+": "+err.Error())
		return
	}
	if strings.HasPrefix(string(data), "magnet:") {
		a.addLink(ctx, chatID, label, string(data), result.Title, options)
		return
	}
	options.Name = result.Title
	if failure := a.addData(ctx, chatID, label, result.Title, data, options); failure != "" {
		a.send(ctx, chatID, failure)
	}
}

// find answers /find QUERY with the best results as buttons to add.
func (a *application) find(ctx context.Context, chatID int64, arguments []string) {
	if a.indexer == nil {
		a.send(ctx, chatID, "find: no indexer is configured; set -indexer-url and RT_INDEXER_KEY")
		return
	}
	if len(arguments) == 0 {
		a.send(ctx, chatID, "find: use find QUERY")
		return
	}
	query := strings.Join(arguments, " ")
	// Indexers can take a while, so search off the update loop.
	a.launch(ctx, func(findCtx context.Context) {
		results, err := a.indexer.search(findCtx, query)
		if err != nil {
			a.send(findCtx, chatID, "find: "+err.Error())
			return
		}
		if len(results) == 0 {
			a.send(findCtx, chatID, "find: nothing found for "+query)
			return
		}
		slices.SortStableFunc(results, func(x, y searchResult) int { return cmp.Compare(y.Seeders, x.Seeders) })
		results = results[:min(len(results), maxFindResults)]
		text, keyboard := renderResults(query, results)
		a.sendScreen(findCtx, chatID, text, keyboard, &screen{results: results})
	})
}

func renderResults(query string, results []searchResult) (string, *models.InlineKeyboardMarkup) {
	var text strings.Builder
	fmt.Fprintf(&text, "Results for %s. Tap one to add it.\n", query)
	var rows [][]models.InlineKeyboardButton
	for i, result := range results {
		fmt.Fprintf(&text, "\n%d. %s\n%s, %d seeders", i+1, result.Title, formatBytes(result.Size), result.Seeders)
		if result.Indexer != "" {
			text.WriteString(", " + result.Indexer)
		}
		rows = append(rows, []models.InlineKeyboardButton{button(strconv.Itoa(i+1)+". "+buttonName(result.Title), "fd:"+strconv.Itoa(i))})
	}
	return text.String(), &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// pressFind adds the result a button names.
func (a *application) pressFind(ctx context.Context, key screenKey, scr *screen, choice string) (string, bool) {
	index, err := strconv.Atoi(choice)
	if err != nil || index < 0 || index >= len(scr.results) {
		return "This button no longer applies.", true
	}
	result := scr.results[index]
	a.launch(ctx, func(grabCtx context.Context) {
		a.grab(grabCtx, key.chatID, "find", result, rtapi.DotTorrentWithOptions{})
	})
	return "Adding " + result.Title, false
}

// A watchRule adds new results for a search as they appear.
type watchRule struct {
	Name   string   `json:"name"`
	Query  string   `json:"query"`
	Dir    string   `json:"dir,omitempty"` // already confined to the download root
	Label  string   `json:"label,omitempty"`
	ChatID int64    `json:"chat"`
	Thread int      `json:"thread,omitempty"`
	Seen   []string `json:"seen,omitempty"` // result keys already handled, oldest first
}

// watch answers /watch: list, add NAME QUERY [l=LABEL] [d=DIR], del NAME.
func (a *application) watch(ctx context.Context, chatID int64, arguments []string) {
	const usage = "watch: use watch, watch add NAME QUERY [l=LABEL] [d=DIR], or watch del NAME"
	if len(arguments) == 0 {
		a.send(ctx, chatID, a.watchList())
		return
	}
	switch strings.ToLower(arguments[0]) {
	case "add":
		if len(arguments) < 3 {
			a.send(ctx, chatID, usage)
			return
		}
		a.addWatch(ctx, chatID, arguments[1], arguments[2:])
	case "del", "rm", "remove":
		if len(arguments) != 2 {
			a.send(ctx, chatID, usage)
			return
		}
		var found bool
		err := a.state.update(func(data *stateData) {
			data.Watch = slices.DeleteFunc(data.Watch, func(rule watchRule) bool {
				found = found || strings.EqualFold(rule.Name, arguments[1])
				return strings.EqualFold(rule.Name, arguments[1])
			})
		})
		switch {
		case err != nil:
			a.send(ctx, chatID, "watch: "+err.Error())
		case !found:
			a.send(ctx, chatID, "watch: no rule named "+arguments[1])
		default:
			a.send(ctx, chatID, "Stopped watching "+arguments[1])
		}
	default:
		a.send(ctx, chatID, usage)
	}
}

func (a *application) watchList() string {
	var rules []watchRule
	a.state.read(func(data *stateData) { rules = slices.Clone(data.Watch) })
	if len(rules) == 0 {
		return "No watch rules. Add one with watch add NAME QUERY."
	}
	var text strings.Builder
	text.WriteString("Watch rules:")
	for _, rule := range rules {
		fmt.Fprintf(&text, "\n%s: %s", rule.Name, rule.Query)
		if rule.Label != "" {
			fmt.Fprintf(&text, ", label %s", rule.Label)
		}
		if rule.Dir != "" {
			fmt.Fprintf(&text, ", in %s", rule.Dir)
		}
	}
	return text.String()
}

// addWatch creates a rule. Results the indexer has now are marked seen, so
// only releases that appear later are added.
func (a *application) addWatch(ctx context.Context, chatID int64, name string, terms []string) {
	if a.indexer == nil {
		a.send(ctx, chatID, "watch: no indexer is configured; set -indexer-url and RT_INDEXER_KEY")
		return
	}
	rule := watchRule{Name: name, ChatID: chatID}
	rule.Thread, _ = ctx.Value(messageThreadIDKey{}).(int)
	var query []string
	for _, term := range terms {
		switch {
		case strings.HasPrefix(term, "l="):
			rule.Label = strings.TrimPrefix(term, "l=")
		case strings.HasPrefix(term, "d="):
			dir, err := a.downloadDirectory(ctx, strings.TrimPrefix(term, "d="))
			if err != nil {
				a.send(ctx, chatID, "watch: "+err.Error())
				return
			}
			rule.Dir = dir
		default:
			query = append(query, term)
		}
	}
	rule.Query = strings.Join(query, " ")
	if rule.Query == "" {
		a.send(ctx, chatID, "watch: give a search query")
		return
	}
	results, err := a.indexer.search(ctx, rule.Query)
	if err != nil {
		a.send(ctx, chatID, "watch: "+err.Error())
		return
	}
	for _, result := range results {
		rule.Seen = append(rule.Seen, result.key())
	}
	var exists bool
	err = a.state.update(func(data *stateData) {
		exists = slices.ContainsFunc(data.Watch, func(other watchRule) bool { return strings.EqualFold(other.Name, name) })
		if !exists {
			data.Watch = append(data.Watch, rule)
		}
	})
	switch {
	case err != nil:
		a.send(ctx, chatID, "watch: "+err.Error())
	case exists:
		a.send(ctx, chatID, "watch: a rule named "+name+" already exists")
	default:
		a.send(ctx, chatID, fmt.Sprintf("Watching %s: %s. The %d results there are now are skipped; new ones are added as they appear.",
			name, rule.Query, len(results)))
	}
}

// watchFeeds checks every watch rule each feed interval until ctx ends.
func (a *application) watchFeeds(ctx context.Context) {
	if a.indexer == nil {
		return
	}
	for waitFor(ctx, a.feedInterval) {
		a.checkWatches(ctx)
	}
}

// checkWatches adds new results for each rule, at most a few per check so a
// misbehaving indexer cannot flood rTorrent, and remembers them as seen.
func (a *application) checkWatches(ctx context.Context) {
	var rules []watchRule
	a.state.read(func(data *stateData) { rules = slices.Clone(data.Watch) })
	for _, rule := range rules {
		results, err := a.indexer.search(ctx, rule.Query)
		if err != nil {
			a.logger.Printf("[ERROR] watch %s: %s", rule.Name, err)
			continue
		}
		var fresh []searchResult
		for _, result := range results {
			if !slices.Contains(rule.Seen, result.key()) && len(fresh) < maxWatchAddsPerCheck {
				fresh = append(fresh, result)
			}
		}
		if len(fresh) == 0 {
			continue
		}
		var added []string
		for _, result := range fresh {
			added = append(added, result.key())
		}
		a.state.update(func(data *stateData) {
			for i := range data.Watch {
				if data.Watch[i].Name == rule.Name {
					seen := append(data.Watch[i].Seen, added...)
					data.Watch[i].Seen = seen[max(0, len(seen)-maxWatchSeen):]
				}
			}
		})
		ruleCtx := ctx
		if rule.Thread != 0 {
			ruleCtx = context.WithValue(ctx, messageThreadIDKey{}, rule.Thread)
		}
		for _, result := range fresh {
			a.grab(ruleCtx, rule.ChatID, "watch "+rule.Name, result, rtapi.DotTorrentWithOptions{Dir: rule.Dir, Label: rule.Label})
		}
	}
}

// indexerName names an indexer endpoint in logs without its query string.
func indexerName(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "the indexer"
	}
	return parsed.Host + path.Clean("/"+parsed.Path)
}
