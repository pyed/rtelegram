package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	telegram "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/pyed/rtapi"
)

var version = "dev"

const helpText = `Commands:
list (li) [tracker] - list torrents
head (he) [count] - list the first torrents
tail (ta) [count] - list the last torrents
down (dl), seeding (sd), paused (pa), checking (ch), active (ac), errors (er)
sort (so) [rev] name|downrate|uprate|size|ratio|age|upload
trackers (tr), search (se) QUERY, latest (la) [count]
add (ad) URL..., info (in) HASH..., files (fi) HASH, get HASH [N]
stop (sp), start (st), check (ck) HASH...|all
del HASH..., deldata HASH [confirm]
stats (sa), speed (ss), count (co), notify [on|off], whoami, help, version
limit [down N] [up N]|off, quiet HH:MM-HH:MM down N [up N]|off
find QUERY, watch [add NAME QUERY|del NAME], digest HH:MM|now|off

Tap the buttons under lists, or name torrents by the hash prefixes lists show.
In groups, commands must start with /.`

const (
	defaultRtorrentAddress = "localhost:5000"
	defaultMaxResponseMiB  = 16
	defaultLiveInterval    = 3 * time.Second
	defaultLiveUpdates     = 5
	defaultAddTimeout      = 15 * time.Second
	maxTelegramMessage     = 4096
	maxTorrentFileSize     = 16 << 20
	// Replies longer than this many messages are attached as a text file.
	maxMessageChunks    = 3
	maxTelegramAttempts = 4
	// Longer rate-limit waits are not worth holding the bot for.
	maxRetryAfter = 2 * time.Minute
	// Unauthorized users are logged once each, up to this many.
	maxIgnoredUsers = 1000
)

type principals struct {
	ids       map[int64]struct{}
	usernames map[string]struct{}
}

type config struct {
	token           string
	masters         principals
	rtorrentAddress string
	maxResponseMiB  int64
	addStopped      bool
	logFile         string
	completedLog    string // removed in v3
	notifyChatID    int64  // removed in v3
	watchInterval   time.Duration
	stallAfter      time.Duration
	lowDisk         uint64
	indexerURL      string
	indexerKey      string
	feedInterval    time.Duration
	dataRoot        string
	downloadRoot    string
	statePath       string
	noLive          bool
	showVersion     bool
	legacyUsernames []string
}

type messageThreadIDKey struct{}

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type application struct {
	bot           *telegram.Bot
	rtorrent      *rtapi.Rtorrent
	httpClient    httpDoer
	logger        *log.Logger
	token         string
	botUsername   string
	masters       principals
	watchInterval time.Duration
	stallAfter    time.Duration
	lowDisk       uint64           // warn below this many free bytes; 0 disables
	now           func() time.Time // the clock; nil means time.Now
	indexer       *indexer         // nil without -indexer-url
	feedInterval  time.Duration
	dataRoot      string
	downloadRoot  string
	addStopped    bool
	noLive        bool
	interval      time.Duration
	duration      int
	// addTimeout bounds how long an add waits for rTorrent to load the torrent.
	addTimeout      time.Duration
	addPollInterval time.Duration

	state     *state
	screens   screenStore
	ignoredMu sync.Mutex
	ignored   map[int64]struct{}
	wg        sync.WaitGroup
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "rtelegram:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	cfg, err := parseConfig(args, getenv, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if cfg.showVersion {
		_, err := fmt.Fprintln(stdout, version)
		return err
	}

	appState, err := loadState(cfg.statePath)
	if err != nil {
		return err
	}

	logger := log.New(stdout, "", log.LstdFlags)
	var logFile *os.File
	if cfg.logFile != "" {
		logFile, err = os.OpenFile(cfg.logFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return fmt.Errorf("open log file: %w", err)
		}
		defer logFile.Close()
		logger.SetOutput(logFile)
	}
	for _, name := range cfg.legacyUsernames {
		logger.Printf("[WARN] legacy username master @%s is mutable; prefer a numeric Telegram user ID (send /whoami to the bot to see it)", name)
	}

	httpClient := &http.Client{Timeout: 70 * time.Second}
	var app *application
	b, err := telegram.New(cfg.token,
		telegram.WithSkipGetMe(),
		telegram.WithHTTPClient(60*time.Second, httpClient),
		telegram.WithAllowedUpdates(telegram.AllowedUpdates{models.AllowedUpdateMessage, models.AllowedUpdateCallbackQuery}),
		telegram.WithNotAsyncHandlers(),
		telegram.WithErrorsHandler(func(err error) {
			logger.Printf("[ERROR] Telegram: %s", redact(cfg.token, err.Error()))
		}),
		telegram.WithDefaultHandler(func(handlerCtx context.Context, _ *telegram.Bot, update *models.Update) {
			app.handle(handlerCtx, update)
		}),
	)
	if err != nil {
		return fmt.Errorf("telegram: %s", redact(cfg.token, err.Error()))
	}

	startupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	me, err := b.GetMe(startupCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("telegram authorization: %s", redact(cfg.token, err.Error()))
	}
	rtorrent, err := rtapi.NewRtorrentContext(ctx, cfg.rtorrentAddress)
	if err != nil {
		return fmt.Errorf("rTorrent: %w", err)
	}
	rtorrent.MaxResponseSize = cfg.maxResponseMiB << 20

	app = &application{
		bot:             b,
		rtorrent:        rtorrent,
		httpClient:      httpClient,
		logger:          logger,
		token:           cfg.token,
		botUsername:     me.Username,
		masters:         cfg.masters,
		watchInterval:   cfg.watchInterval,
		stallAfter:      cfg.stallAfter,
		lowDisk:         cfg.lowDisk,
		feedInterval:    cfg.feedInterval,
		dataRoot:        cfg.dataRoot,
		downloadRoot:    cfg.downloadRoot,
		addStopped:      cfg.addStopped,
		noLive:          cfg.noLive,
		interval:        defaultLiveInterval,
		duration:        defaultLiveUpdates,
		addTimeout:      defaultAddTimeout,
		addPollInterval: time.Second,
		state:           appState,
	}
	logger.Printf("[INFO] Authorized as @%s; rTorrent=%s", me.Username, redactAddress(cfg.rtorrentAddress))
	app.launch(ctx, app.watchEvents)
	app.launch(ctx, app.watchQuiet)
	app.launch(ctx, app.watchDigest)
	if cfg.indexerURL != "" {
		app.indexer = newIndexer(cfg.indexerURL, cfg.indexerKey)
		logger.Printf("[INFO] Indexer: %s", indexerName(cfg.indexerURL))
		app.launch(ctx, app.watchFeeds)
	}
	b.Start(ctx)
	app.wg.Wait()
	return nil
}

func parseConfig(args []string, getenv func(string) string, stderr io.Writer) (config, error) {
	var cfg config
	var mastersText string
	fs := flag.NewFlagSet("rtelegram", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&cfg.token, "token", "", "Telegram bot token (or RT_TOKEN)")
	fs.StringVar(&mastersText, "masters", "", "Comma-separated Telegram user IDs or legacy usernames (or RT_MASTERS)")
	fs.StringVar(&cfg.rtorrentAddress, "url", "", "rTorrent address: an SCGI socket path or host:port, or an http(s):// XML-RPC URL (or RT_URL; default "+defaultRtorrentAddress+")")
	fs.Int64Var(&cfg.maxResponseMiB, "max-response-mib", defaultMaxResponseMiB, "Largest rTorrent response to accept, in MiB; raise it for very large libraries")
	fs.BoolVar(&cfg.addStopped, "add-stopped", false, "Add torrents without starting them")
	fs.StringVar(&cfg.logFile, "logfile", "", "Send logs to a file")
	fs.StringVar(&cfg.completedLog, "completed-torrents-logfile", "", "Removed in v3; send /notify instead")
	fs.Int64Var(&cfg.notifyChatID, "notify-chat-id", 0, "Removed in v3; send /notify instead")
	fs.DurationVar(&cfg.watchInterval, "watch-interval", defaultWatchInterval, "How often to check rTorrent for notifications")
	fs.DurationVar(&cfg.stallAfter, "stall-after", defaultStallAfter, "Report a download as stalled after this long without progress (0 disables)")
	lowDisk := fs.String("low-disk", "5G", "Report low disk space below this much free space where rTorrent saves data (0 disables)")
	fs.StringVar(&cfg.indexerURL, "indexer-url", "", "Torznab endpoint of Prowlarr or Jackett, for find and watch (or RT_INDEXER_URL)")
	fs.StringVar(&cfg.indexerKey, "indexer-key", "", "API key of the indexer (or RT_INDEXER_KEY)")
	fs.DurationVar(&cfg.feedInterval, "feed-interval", defaultFeedInterval, "How often watch rules search for new releases")
	fs.StringVar(&cfg.dataRoot, "data-root", "", "Absolute local root allowed for deldata")
	fs.StringVar(&cfg.downloadRoot, "download-root", "", "Absolute rTorrent directory that upload captions may choose download directories under (default: rTorrent's default directory)")
	fs.StringVar(&cfg.statePath, "state", "", "File where rtelegram keeps settings such as sort orders (default: rtelegram/state.json in the user's config directory)")
	fs.BoolVar(&cfg.noLive, "no-live", false, "Do not edit messages with live updates")
	fs.BoolVar(&cfg.showVersion, "version", false, "Print the rtelegram version and exit")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if len(fs.Args()) != 0 {
		return config{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if cfg.showVersion {
		return cfg, nil
	}
	if cfg.token == "" {
		cfg.token = getenv("RT_TOKEN")
	}
	if strings.TrimSpace(cfg.token) == "" {
		return config{}, errors.New("telegram token is missing")
	}
	if mastersText == "" {
		mastersText = getenv("RT_MASTERS")
	}
	masters, legacy, err := parsePrincipals(mastersText)
	if err != nil {
		return config{}, err
	}
	cfg.masters = masters
	cfg.legacyUsernames = legacy
	if cfg.rtorrentAddress == "" {
		cfg.rtorrentAddress = getenv("RT_URL")
	}
	if strings.TrimSpace(cfg.rtorrentAddress) == "" {
		cfg.rtorrentAddress = defaultRtorrentAddress
	}
	if cfg.statePath == "" {
		if cfg.statePath, err = defaultStatePath(); err != nil {
			return config{}, err
		}
	}
	if cfg.maxResponseMiB < 1 || cfg.maxResponseMiB > 1<<16 {
		return config{}, errors.New("-max-response-mib must be between 1 and 65536")
	}
	if cfg.completedLog != "" || cfg.notifyChatID != 0 {
		return config{}, errors.New("-completed-torrents-logfile and -notify-chat-id were removed in v3: rtelegram now notices completed torrents itself, so send /notify in the chat that should be told")
	}
	if cfg.watchInterval < 5*time.Second {
		return config{}, errors.New("-watch-interval must be at least 5s")
	}
	if cfg.stallAfter < 0 {
		return config{}, errors.New("-stall-after must not be negative")
	}
	if cfg.lowDisk, err = parseSize(*lowDisk); err != nil {
		return config{}, fmt.Errorf("-low-disk: %w", err)
	}
	cfg.indexerURL = cmp.Or(cfg.indexerURL, getenv("RT_INDEXER_URL"))
	cfg.indexerKey = cmp.Or(cfg.indexerKey, getenv("RT_INDEXER_KEY"))
	if cfg.indexerURL != "" {
		if parsed, err := url.Parse(cfg.indexerURL); err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return config{}, errors.New("-indexer-url must be an http:// or https:// URL")
		}
	}
	if cfg.feedInterval < 5*time.Minute {
		return config{}, errors.New("-feed-interval must be at least 5m")
	}
	if cfg.dataRoot != "" {
		if !filepath.IsAbs(cfg.dataRoot) {
			return config{}, errors.New("-data-root must be an absolute path")
		}
		cfg.dataRoot = filepath.Clean(cfg.dataRoot)
	}
	if cfg.downloadRoot != "" {
		// rTorrent interprets download directories on its own host, so this is
		// a slash-separated path whatever the local OS.
		if !path.IsAbs(cfg.downloadRoot) {
			return config{}, errors.New("-download-root must be an absolute path")
		}
		cfg.downloadRoot = path.Clean(cfg.downloadRoot)
	}
	return cfg, nil
}

func parsePrincipals(text string) (principals, []string, error) {
	p := principals{ids: make(map[int64]struct{}), usernames: make(map[string]struct{})}
	if strings.TrimSpace(text) == "" {
		return p, nil, errors.New("at least one Telegram master is required")
	}
	var legacy []string
	for _, raw := range strings.Split(text, ",") {
		value := strings.TrimSpace(raw)
		if value == "" || value == "@" {
			return principals{}, nil, errors.New("telegram masters must not contain empty entries")
		}
		if id, err := strconv.ParseInt(value, 10, 64); err == nil {
			if id <= 0 {
				return principals{}, nil, fmt.Errorf("invalid Telegram user ID %q", value)
			}
			p.ids[id] = struct{}{}
			continue
		}
		name := strings.ToLower(strings.TrimPrefix(value, "@"))
		if name == "" || strings.ContainsAny(name, "@ ") {
			return principals{}, nil, fmt.Errorf("invalid legacy Telegram username %q", value)
		}
		p.usernames[name] = struct{}{}
		legacy = append(legacy, name)
	}
	slices.Sort(legacy)
	legacy = slices.Compact(legacy)
	return p, legacy, nil
}

func (p principals) authorized(user *models.User) bool {
	if user == nil {
		return false
	}
	if _, ok := p.ids[user.ID]; ok {
		return true
	}
	if user.Username == "" {
		return false
	}
	_, ok := p.usernames[strings.ToLower(user.Username)]
	return ok
}

func parseCommand(message *models.Message, botUsername string) (string, []string, bool) {
	if message == nil {
		return "", nil, false
	}
	fields := strings.Fields(message.Text)
	if len(fields) == 0 {
		return "", nil, false
	}
	first := fields[0]
	slashed := strings.HasPrefix(first, "/")
	if message.Chat.Type != models.ChatTypePrivate && !slashed {
		return "", nil, false
	}
	first = strings.TrimPrefix(first, "/")
	command, suffix, hasSuffix := strings.Cut(first, "@")
	if hasSuffix && (suffix == "" || !strings.EqualFold(suffix, botUsername)) {
		return "", nil, false
	}
	if command == "" {
		return "", nil, false
	}
	return strings.ToLower(command), fields[1:], true
}

func documentOptions(message *models.Message, botUsername string) (string, bool) {
	if message == nil || message.Document == nil {
		return "", false
	}
	if message.Chat.Type == models.ChatTypePrivate {
		return message.Caption, true
	}
	caption := *message
	caption.Text = message.Caption
	command, arguments, ok := parseCommand(&caption, botUsername)
	if !ok || (command != "add" && command != "ad") {
		return "", false
	}
	return strings.Join(arguments, " "), true
}

func (a *application) handle(ctx context.Context, update *models.Update) {
	if update != nil && update.CallbackQuery != nil {
		a.handleCallback(ctx, update.CallbackQuery)
		return
	}
	if update == nil || update.Message == nil {
		return
	}
	message := update.Message
	if !a.masters.authorized(message.From) {
		a.logIgnored(message)
		return
	}
	if message.MessageThreadID != 0 {
		ctx = context.WithValue(ctx, messageThreadIDKey{}, message.MessageThreadID)
	}
	chatID := message.Chat.ID
	if message.Document != nil {
		options, ok := documentOptions(message, a.botUsername)
		if !ok {
			return
		}
		a.receiveTorrent(ctx, chatID, message, options)
		return
	}
	command, args, ok := parseCommand(message, a.botUsername)
	if !ok {
		return
	}

	switch command {
	case "list", "li":
		a.list(ctx, chatID, args)
	case "head", "he":
		a.head(ctx, chatID, args)
	case "tail", "ta":
		a.tail(ctx, chatID, args)
	case "down", "dl":
		a.downs(ctx, chatID)
	case "seeding", "sd":
		a.seeding(ctx, chatID)
	case "paused", "pa":
		a.paused(ctx, chatID)
	case "hashing", "ha", "checking", "ch":
		a.hashing(ctx, chatID)
	case "active", "ac":
		a.active(ctx, chatID)
	case "errors", "er":
		a.errors(ctx, chatID)
	case "sort", "so":
		a.sort(ctx, chatID, args)
	case "trackers", "tr":
		a.trackers(ctx, chatID)
	case "add", "ad":
		a.add(ctx, chatID, args)
	case "search", "se":
		a.search(ctx, chatID, args)
	case "latest", "la":
		a.latest(ctx, chatID, args)
	case "info", "in":
		a.info(ctx, chatID, args)
	case "stop", "sp":
		a.stop(ctx, chatID, args)
	case "start", "st":
		a.start(ctx, chatID, args)
	case "check", "ck":
		a.check(ctx, chatID, args)
	case "stats", "sa":
		a.stats(ctx, chatID)
	case "speed", "ss":
		a.speed(ctx, chatID)
	case "count", "co":
		a.count(ctx, chatID)
	case "del":
		a.del(ctx, chatID, args)
	case "deldata":
		a.deldata(ctx, chatID, args)
	case "files", "fi":
		a.files(ctx, chatID, args)
	case "get":
		a.get(ctx, chatID, args)
	case "limit":
		a.limit(ctx, chatID, args)
	case "quiet":
		a.quiet(ctx, chatID, args)
	case "digest":
		a.digest(ctx, chatID, args)
	case "find":
		a.find(ctx, chatID, args)
	case "watch":
		a.watch(ctx, chatID, args)
	case "notify":
		a.notify(ctx, chatID, args)
	case "whoami":
		a.whoami(ctx, chatID, message.From)
	case "help":
		a.send(ctx, chatID, helpText)
	case "version":
		a.getVersion(ctx, chatID)
	default:
		a.send(ctx, chatID, "no such command, try /help")
	}
}

// logIgnored logs the numeric ID of each unauthorized user who messages the
// bot privately, once, so an operator can find the ID to authorize. Group
// members are not logged.
func (a *application) logIgnored(message *models.Message) {
	if message.From == nil || message.Chat.Type != models.ChatTypePrivate {
		return
	}
	a.ignoredMu.Lock()
	defer a.ignoredMu.Unlock()
	if _, seen := a.ignored[message.From.ID]; seen || len(a.ignored) >= maxIgnoredUsers {
		return
	}
	if a.ignored == nil {
		a.ignored = make(map[int64]struct{})
	}
	a.ignored[message.From.ID] = struct{}{}
	username := ""
	if message.From.Username != "" {
		username = " (@" + message.From.Username + ")"
	}
	a.logger.Printf("[WARN] Ignored a private message from unauthorized Telegram user ID %d%s", message.From.ID, username)
}

func (a *application) whoami(ctx context.Context, chatID int64, user *models.User) {
	username := ""
	if user.Username != "" {
		username = " (@" + user.Username + ")"
	}
	a.send(ctx, chatID, fmt.Sprintf("User ID: %d%s\nChat ID: %d\n\nUse the user ID in RT_MASTERS, and the chat ID with -notify-chat-id.",
		user.ID, username, chatID))
}

// redactAddress hides the password in an http(s) rTorrent address.
func redactAddress(address string) string {
	if parsed, err := url.Parse(address); err == nil && parsed.User != nil {
		return parsed.Redacted()
	}
	return address
}

func redact(secret, text string) string {
	if secret == "" {
		return text
	}
	return strings.ReplaceAll(text, secret, "[REDACTED]")
}
