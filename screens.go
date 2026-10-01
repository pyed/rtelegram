package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	telegram "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/pyed/rtapi"
)

// Screens are bot messages with buttons. The bot remembers what each one
// shows, keyed by chat and message, so a button press can redraw it in place.

const (
	listPageSize   = 10
	maxScreens     = 500
	maxButtonRunes = 40
	maxToastRunes  = 200
)

type screenKey struct {
	chatID    int64
	messageID int
}

// A screen is a paged torrent list or a torrent card.
type screen struct {
	list    *listSpec      // a paged list
	page    int            // the list page shown
	hash    string         // the torrent a card shows
	confirm string         // "del" or "deldata" while a card asks for confirmation
	parent  *screen        // the list page a card was opened from
	notify  bool           // the /notify settings
	files   string         // the torrent whose files are shown
	limits  bool           // the /limit presets
	results []searchResult // /find results
}

// screenStore remembers the most recent screens, forgetting the oldest
// beyond maxScreens. Presses on forgotten screens are answered as expired.
type screenStore struct {
	mu      sync.Mutex
	screens map[screenKey]*screen
	order   []screenKey
}

func (s *screenStore) put(key screenKey, scr *screen) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.screens == nil {
		s.screens = make(map[screenKey]*screen)
	}
	if _, ok := s.screens[key]; !ok {
		s.order = append(s.order, key)
		for len(s.order) > maxScreens {
			delete(s.screens, s.order[0])
			s.order = s.order[1:]
		}
	}
	s.screens[key] = scr
}

func (s *screenStore) get(key screenKey) *screen {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.screens[key]
}

func (s *screenStore) forget(key screenKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.screens, key)
	s.order = slices.DeleteFunc(s.order, func(k screenKey) bool { return k == key })
}

// listSpec says which torrents a list shows.
type listSpec struct {
	kind  string // a key of listKinds
	query string // list's tracker filter or search's name filter, lowercase
	count int    // how many torrents latest shows
}

type listKind struct {
	label  string // prefixes errors
	empty  string // the reply when no torrents match
	render func(rtapi.Torrents, map[string]string) string
	keep   func(torrent *rtapi.Torrent, query string) bool // nil for latest
}

func stateIs(state string) func(*rtapi.Torrent, string) bool {
	return func(torrent *rtapi.Torrent, _ string) bool { return torrent.State == state }
}

var listKinds = map[string]listKind{
	"list": {"list", "list: No torrents", renderTorrentListWithPrefixes, func(torrent *rtapi.Torrent, query string) bool {
		return query == "" || strings.Contains(strings.ToLower(trackerHost(torrent.Tracker)), query)
	}},
	"down":     {"down", "No downloads", renderTorrentListWithPrefixes, stateIs(rtapi.Leeching)},
	"seeding":  {"seeding", "No torrents seeding", renderTorrentListWithPrefixes, stateIs(rtapi.Seeding)},
	"checking": {"checking", "No torrents checking", renderTorrentListWithPrefixes, stateIs(rtapi.Hashing)},
	"paused":   {"paused", "No paused torrents", renderTorrentDetailsWithPrefixes, stateIs(rtapi.Stopped)},
	"errors":   {"errors", "No errors", renderTorrentErrors, stateIs(rtapi.Error)},
	"search": {"search", "No matches", renderTorrentListWithPrefixes, func(torrent *rtapi.Torrent, query string) bool {
		return strings.Contains(strings.ToLower(torrent.Name), query)
	}},
	"latest": {"latest", "latest: No torrents", renderTorrentListWithPrefixes, nil},
}

func renderTorrentErrors(torrents rtapi.Torrents, prefixes map[string]string) string {
	var output strings.Builder
	for _, torrent := range torrents {
		fmt.Fprintf(&output, "<%s> %s\n%s\n\n", torrentRef(torrent, prefixes), torrent.Name, torrent.Message)
	}
	return output.String()
}

// listTorrents returns the torrents spec selects, with the hash prefixes of
// every loaded torrent so references stay unique.
func (a *application) listTorrents(ctx context.Context, chatID int64, spec listSpec) (rtapi.Torrents, map[string]string, error) {
	torrents, err := a.torrents(ctx, chatID)
	if err != nil {
		return nil, nil, err
	}
	prefixes := hashPrefixes(torrents)
	kind := listKinds[spec.kind]
	if spec.kind == "list" && spec.query != "" {
		if err := a.rtorrent.TrackersContext(ctx, torrents); err != nil {
			return nil, nil, err
		}
	}
	if kind.keep == nil {
		torrents = slices.Clone(torrents)
		slices.SortStableFunc(torrents, func(x, y *rtapi.Torrent) int { return cmp.Compare(y.Age, x.Age) })
		return torrents[:min(spec.count, len(torrents))], prefixes, nil
	}
	return filterTorrents(torrents, func(torrent *rtapi.Torrent) bool { return kind.keep(torrent, spec.query) }), prefixes, nil
}

func button(text, data string) models.InlineKeyboardButton {
	return models.InlineKeyboardButton{Text: text, CallbackData: data}
}

func buttonName(name string) string {
	if utf8.RuneCountInString(name) <= maxButtonRunes {
		return name
	}
	return string([]rune(name)[:maxButtonRunes-1]) + "…"
}

// renderList renders one page of a list with a button per torrent. A list
// that fits on one page reads exactly as it did before buttons.
func renderList(spec listSpec, torrents rtapi.Torrents, prefixes map[string]string, page int) (string, *models.InlineKeyboardMarkup, int) {
	pages := (len(torrents) + listPageSize - 1) / listPageSize
	page = min(max(page, 0), pages-1)
	shown := torrents[page*listPageSize : min(len(torrents), (page+1)*listPageSize)]
	text := listKinds[spec.kind].render(shown, prefixes)
	var rows [][]models.InlineKeyboardButton
	for _, torrent := range shown {
		rows = append(rows, []models.InlineKeyboardButton{button(buttonName(torrent.Name), "t:"+torrent.Hash)})
	}
	if pages > 1 {
		text = strings.TrimRight(text, "\n") + fmt.Sprintf("\n\nPage %d of %d, %d torrents", page+1, pages, len(torrents))
		var nav []models.InlineKeyboardButton
		if page > 0 {
			nav = append(nav, button("◀", "pg:"+strconv.Itoa(page-1)))
		}
		nav = append(nav, button(fmt.Sprintf("%d/%d", page+1, pages), "noop"))
		if page < pages-1 {
			nav = append(nav, button("▶", "pg:"+strconv.Itoa(page+1)))
		}
		rows = append(rows, append(nav, button("📄 All", "all")))
	}
	return text, &models.InlineKeyboardMarkup{InlineKeyboard: rows}, page
}

// showList answers a list command with the first page of the list.
func (a *application) showList(ctx context.Context, chatID int64, spec listSpec) {
	kind := listKinds[spec.kind]
	torrents, prefixes, err := a.listTorrents(ctx, chatID, spec)
	if err != nil {
		a.send(ctx, chatID, kind.label+": "+err.Error())
		return
	}
	if len(torrents) == 0 {
		a.send(ctx, chatID, kind.empty)
		return
	}
	text, keyboard, page := renderList(spec, torrents, prefixes, 0)
	a.sendScreen(ctx, chatID, text, keyboard, &screen{list: &spec, page: page})
}

// renderCard renders a torrent's details and actions, or the confirmation
// a removal asks for.
func (a *application) renderCard(torrent *rtapi.Torrent, scr *screen) (string, *models.InlineKeyboardMarkup) {
	switch scr.confirm {
	case "del":
		return fmt.Sprintf("Remove %s from rTorrent? Its data stays on disk.", torrent.Name),
			&models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{button("✅ Remove", "y:del"), button("Cancel", "n")}}}
	case "deldata":
		return fmt.Sprintf("Remove %s and delete its data from disk? This cannot be undone.", torrent.Name),
			&models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{button("💣 Delete data", "y:deldata"), button("Cancel", "n")}}}
	}
	toggle := button("⏸ Stop", "a:stop")
	if torrent.State == rtapi.Stopped || torrent.State == rtapi.Complete {
		toggle = button("▶ Start", "a:start")
	}
	removals := []models.InlineKeyboardButton{button("🗑 Remove", "a:del")}
	if a.dataRoot != "" {
		removals = append(removals, button("💣 Remove + data", "a:deldata"))
	}
	last := []models.InlineKeyboardButton{button("📂 Files", "files"), button("🔄 Refresh", "a:refresh")}
	if scr.parent != nil && scr.parent.list != nil {
		last = append(last, button("« Back", "back"))
	}
	return formatTorrentInfo(torrent), &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{
		{toggle, button("🔍 Check", "a:check")}, removals, last,
	}}
}

// sendScreen sends text with buttons and remembers what it shows. Text too
// long for one message is sent without buttons.
func (a *application) sendScreen(ctx context.Context, chatID int64, text string, keyboard *models.InlineKeyboardMarkup, scr *screen) error {
	if utf8.RuneCountInString(text) > maxTelegramMessage {
		_, err := a.send(ctx, chatID, text)
		return err
	}
	messageThreadID, _ := ctx.Value(messageThreadIDKey{}).(int)
	params := &telegram.SendMessageParams{
		ChatID:             chatID,
		MessageThreadID:    messageThreadID,
		Text:               text,
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: telegram.True()},
	}
	if keyboard != nil {
		params.ReplyMarkup = keyboard
	}
	var message *models.Message
	err := a.retryRateLimited(ctx, func() (err error) {
		message, err = a.bot.SendMessage(ctx, params)
		return err
	})
	if err != nil {
		clean := redact(a.token, err.Error())
		a.logger.Printf("[ERROR] Send: %s", clean)
		return errors.New(clean)
	}
	a.screens.put(screenKey{chatID, message.ID}, scr)
	return nil
}

// editScreen replaces a screen's text and buttons; nil keyboard removes them.
func (a *application) editScreen(ctx context.Context, key screenKey, text string, keyboard *models.InlineKeyboardMarkup) error {
	params := &telegram.EditMessageTextParams{
		ChatID:             key.chatID,
		MessageID:          key.messageID,
		Text:               text,
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: telegram.True()},
	}
	if keyboard != nil {
		params.ReplyMarkup = keyboard
	}
	err := a.retryRateLimited(ctx, func() error {
		_, err := a.bot.EditMessageText(ctx, params)
		return err
	})
	if err != nil && strings.Contains(err.Error(), "message is not modified") {
		return nil // a refresh with nothing new
	}
	if err != nil {
		clean := redact(a.token, err.Error())
		a.logger.Printf("[ERROR] Edit: %s", clean)
		return errors.New(clean)
	}
	return nil
}

// handleCallback answers a button press. Only masters can press buttons,
// and every press is answered so the client stops its spinner.
func (a *application) handleCallback(ctx context.Context, query *models.CallbackQuery) {
	text, alert := a.pressButton(ctx, query)
	if runes := []rune(text); len(runes) > maxToastRunes {
		text = string(runes[:maxToastRunes-1]) + "…"
	}
	if _, err := a.bot.AnswerCallbackQuery(ctx, &telegram.AnswerCallbackQueryParams{
		CallbackQueryID: query.ID, Text: text, ShowAlert: alert,
	}); err != nil {
		a.logger.Printf("[ERROR] Answer button: %s", redact(a.token, err.Error()))
	}
}

func (a *application) pressButton(ctx context.Context, query *models.CallbackQuery) (string, bool) {
	if !a.masters.authorized(&query.From) {
		return "Only the bot's owners can use these buttons.", false
	}
	message := query.Message.Message
	if message == nil {
		return "This message is too old to change. Run the command again.", true
	}
	if message.MessageThreadID != 0 {
		ctx = context.WithValue(ctx, messageThreadIDKey{}, message.MessageThreadID)
	}
	key := screenKey{message.Chat.ID, message.ID}
	scr := a.screens.get(key)
	if scr == nil {
		return "These buttons have expired. Run the command again.", true
	}
	verb, arg, _ := strings.Cut(query.Data, ":")
	switch verb {
	case "noop":
		return "", false
	case "pg":
		page, _ := strconv.Atoi(arg)
		if scr.files != "" {
			next := *scr
			next.page = page
			return a.redrawFiles(ctx, key, &next, "")
		}
		if scr.list == nil {
			break
		}
		return a.redrawList(ctx, key, *scr.list, page, "")
	case "all":
		if scr.list == nil {
			break
		}
		torrents, prefixes, err := a.listTorrents(ctx, key.chatID, *scr.list)
		if err != nil {
			return err.Error(), true
		}
		a.send(ctx, key.chatID, listKinds[scr.list.kind].render(torrents, prefixes))
		return "", false
	case "t":
		parent := *scr
		return a.redrawCard(ctx, key, &screen{hash: arg, parent: &parent}, "")
	case "back":
		if scr.files != "" && scr.parent != nil {
			return a.redrawCard(ctx, key, scr.parent, "")
		}
		if scr.parent == nil || scr.parent.list == nil {
			break
		}
		return a.redrawList(ctx, key, *scr.parent.list, scr.parent.page, "")
	case "files":
		if scr.hash == "" {
			break
		}
		card := *scr
		return a.redrawFiles(ctx, key, &screen{files: scr.hash, parent: &card}, "")
	case "fp":
		if scr.files == "" {
			break
		}
		return a.pressFilePriority(ctx, key, scr, arg)
	case "fg":
		if scr.files == "" {
			break
		}
		return a.pressGet(ctx, key, scr, arg)
	case "lm":
		if !scr.limits {
			break
		}
		return a.pressLimit(ctx, key, arg)
	case "fd":
		if scr.results == nil {
			break
		}
		return a.pressFind(ctx, key, scr, arg)
	case "n":
		next := *scr
		next.confirm = ""
		return a.redrawCard(ctx, key, &next, "")
	case "a":
		return a.cardAction(ctx, key, scr, arg)
	case "nt":
		if !scr.notify {
			break
		}
		return a.pressNotify(ctx, key, arg)
	case "y":
		return a.confirmRemoval(ctx, key, scr, arg)
	}
	return "This button no longer applies. Run the command again.", true
}

func (a *application) cardAction(ctx context.Context, key screenKey, scr *screen, action string) (string, bool) {
	if scr.hash == "" {
		return "This button no longer applies.", true
	}
	if action == "del" || action == "deldata" {
		next := *scr
		next.confirm = action
		return a.redrawCard(ctx, key, &next, "")
	}
	torrent, err := a.rtorrent.GetTorrentContext(ctx, scr.hash)
	if err != nil {
		return err.Error(), true
	}
	var toast string
	switch action {
	case "start":
		err, toast = a.rtorrent.StartContext(ctx, torrent), "Started"
	case "stop":
		err, toast = a.rtorrent.StopContext(ctx, torrent), "Stopped"
	case "check":
		err, toast = a.rtorrent.CheckContext(ctx, torrent), "Checking"
	case "refresh":
		toast = "Updated"
	default:
		return "This button no longer applies.", true
	}
	if err != nil {
		return action + ": " + err.Error(), true
	}
	return a.redrawCard(ctx, key, scr, toast)
}

func (a *application) confirmRemoval(ctx context.Context, key screenKey, scr *screen, action string) (string, bool) {
	if scr.hash == "" || scr.confirm != action {
		return "This button no longer applies.", true
	}
	var done string
	switch action {
	case "del":
		torrent, err := a.rtorrent.GetTorrentContext(ctx, scr.hash)
		if err != nil {
			return err.Error(), true
		}
		if err := a.rtorrent.DeleteMetadataContext(ctx, torrent); err != nil {
			return "Remove: " + err.Error(), true
		}
		done = "Removed: " + torrent.Name
	case "deldata":
		message, ok := a.deleteWithData(ctx, key.chatID, scr.hash)
		if !ok {
			next := *scr
			next.confirm = ""
			a.redrawCard(ctx, key, &next, "")
			return message, true
		}
		done = message
	default:
		return "This button no longer applies.", true
	}
	if scr.parent != nil && scr.parent.list != nil {
		return a.redrawList(ctx, key, *scr.parent.list, scr.parent.page, done)
	}
	a.screens.forget(key)
	a.editScreen(ctx, key, done, nil) // logs its own failure
	return done, false
}

// redrawList shows a page of a list in an existing message.
func (a *application) redrawList(ctx context.Context, key screenKey, spec listSpec, page int, toast string) (string, bool) {
	torrents, prefixes, err := a.listTorrents(ctx, key.chatID, spec)
	if err != nil {
		return err.Error(), true
	}
	if len(torrents) == 0 {
		a.screens.forget(key)
		if err := a.editScreen(ctx, key, listKinds[spec.kind].empty, nil); err != nil {
			return err.Error(), true
		}
		return toast, false
	}
	text, keyboard, page := renderList(spec, torrents, prefixes, page)
	if err := a.editScreen(ctx, key, text, keyboard); err != nil {
		return err.Error(), true
	}
	a.screens.put(key, &screen{list: &spec, page: page})
	return toast, false
}

// redrawCard shows a torrent card in an existing message.
func (a *application) redrawCard(ctx context.Context, key screenKey, scr *screen, toast string) (string, bool) {
	torrent, err := a.rtorrent.GetTorrentContext(ctx, scr.hash)
	if err != nil {
		var keyboard *models.InlineKeyboardMarkup
		if scr.parent != nil && scr.parent.list != nil {
			keyboard = &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{button("« Back", "back")}}}
		}
		if editErr := a.editScreen(ctx, key, "This torrent is no longer loaded.", keyboard); editErr != nil {
			return editErr.Error(), true
		}
		a.screens.put(key, &screen{parent: scr.parent})
		return err.Error(), true
	}
	text, keyboard := a.renderCard(torrent, scr)
	if err := a.editScreen(ctx, key, text, keyboard); err != nil {
		return err.Error(), true
	}
	a.screens.put(key, scr)
	return toast, false
}

// sendCard sends a torrent card as a new message.
func (a *application) sendCard(ctx context.Context, chatID int64, torrent *rtapi.Torrent, confirm string) {
	scr := &screen{hash: torrent.Hash, confirm: confirm}
	text, keyboard := a.renderCard(torrent, scr)
	a.sendScreen(ctx, chatID, text, keyboard, scr)
}
