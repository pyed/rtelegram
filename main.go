package main

import (
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

const (
	defaultRtorrentAddress = "localhost:5000"
	defaultMaxResponseMiB  = 64
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

// principals are the Telegram users the bot answers, by user ID.
type principals struct {
	ids map[int64]struct{}
}

// setupMaster is the placeholder master the setup steps start the bot with,
// to learn the user's real ID. No Telegram account has it.
const setupMaster = 1

// setup reports whether the only master is the setup placeholder.
func (p principals) setup() bool {
	_, ok := p.ids[setupMaster]
	return ok && len(p.ids) == 1
}

// dataRootOff is the -data-root value that turns /get and deldata off.
const dataRootOff = "off"

type config struct {
	token           string
	masters         principals
	rtorrentAddress string
	maxResponseMiB  int64
	addStopped      bool
	logFile         string
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
	configPath      string // -config
	install         bool
	uninstall       bool
	// settings are the options given by flag, config file, or environment,
	// by flag name, which -install saves to the config file.
	settings map[string]string
	// rewriteConfig reports whether -install must write the config file:
	// settings came from somewhere other than an existing config file.
	rewriteConfig bool
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
	// trafficSaved is when the traffic count was last saved, and transfers
	// each torrent's totals at the last look; trafficMu guards both.
	trafficMu    sync.Mutex
	trafficSaved time.Time
	transfers    map[string]rtapi.Transfer
	// uploadMu sends files one at a time.
	uploadMu sync.Mutex
	// quietMu keeps quiet hours and limit changes from crossing; quietError
	// is the last error the quiet hours check logged, which it does not repeat.
	quietMu    sync.Mutex
	quietError string
	ignored      map[int64]struct{}
	wg           sync.WaitGroup
}

func main() {
	if runAsService() {
		return
	}
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
	if cfg.uninstall {
		return uninstallService(newServiceHost(stdout))
	}
	if cfg.install {
		return installService(ctx, cfg, newServiceHost(stdout))
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

	httpClient := telegramClient{client: &http.Client{}, request: telegramTimeout, upload: uploadTimeout}
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
	dataRoot, dataRootNote := chooseDataRoot(ctx, cfg, rtorrent)

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
		dataRoot:        dataRoot,
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
	logger.Printf("[INFO] %s", dataRootNote)
	if cfg.masters.setup() {
		logger.Printf("[INFO] Setup: the only master is %d, a placeholder. Send @%s a private message from your Telegram account: "+
			"it replies with your user ID, which also appears in this log. Then restart with RT_MASTERS (or -masters) set to that ID.",
			setupMaster, me.Username)
	}
	app.launch(ctx, app.registerCommands)
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
	// Save what was counted since the last save, such as traffic.
	if err := appState.update(func(*stateData) {}); err != nil {
		logger.Printf("[ERROR] save state: %s", err)
	}
	return nil
}

func parseConfig(args []string, getenv func(string) string, stderr io.Writer) (config, error) {
	var cfg config
	var mastersText string
	fs := flag.NewFlagSet("rtelegram", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&cfg.token, "token", "", "Telegram bot token (or RT_TOKEN)")
	fs.StringVar(&mastersText, "masters", "", "Comma-separated numeric Telegram user IDs (or RT_MASTERS)")
	fs.StringVar(&cfg.rtorrentAddress, "url", "", "rTorrent address: an SCGI socket path or host:port, or an http(s):// XML-RPC URL (or RT_URL; default "+defaultRtorrentAddress+")")
	fs.Int64Var(&cfg.maxResponseMiB, "max-response-mib", defaultMaxResponseMiB, "Largest rTorrent response to accept, in MiB; raise it for very large libraries")
	fs.BoolVar(&cfg.addStopped, "add-stopped", false, "Add torrents without starting them")
	fs.StringVar(&cfg.logFile, "logfile", "", "Send logs to a file")
	fs.DurationVar(&cfg.watchInterval, "watch-interval", defaultWatchInterval, "How often to check rTorrent for notifications")
	fs.DurationVar(&cfg.stallAfter, "stall-after", defaultStallAfter, "Report a download as stalled after this long without progress (0 disables)")
	lowDisk := fs.String("low-disk", "5G", "Report low disk space below this much free space where rTorrent saves data (0 disables)")
	fs.StringVar(&cfg.indexerURL, "indexer-url", "", "Torznab endpoint of Prowlarr or Jackett, for find and watch (or RT_INDEXER_URL)")
	fs.StringVar(&cfg.indexerKey, "indexer-key", "", "API key of the indexer (or RT_INDEXER_KEY)")
	fs.DurationVar(&cfg.feedInterval, "feed-interval", defaultFeedInterval, "How often watch rules search for new releases")
	fs.StringVar(&cfg.dataRoot, "data-root", "", "Local directory beneath which /get and deldata may read and delete data "+
		"(default: rTorrent's download directory, when rTorrent runs on this machine; off turns them off)")
	fs.StringVar(&cfg.downloadRoot, "download-root", "", "Absolute rTorrent directory that upload captions may choose download directories under (default: rTorrent's default directory)")
	fs.StringVar(&cfg.statePath, "state", "", "File where rtelegram keeps settings such as sort orders (default: rtelegram/state.json in the user's config directory)")
	fs.BoolVar(&cfg.noLive, "no-live", false, "Do not edit messages with live updates")
	fs.BoolVar(&cfg.showVersion, "version", false, "Print the rtelegram version and exit")
	fs.StringVar(&cfg.configPath, "config", "", "Read settings from this file: a flag name and value per line, such as token = 123:abc")
	fs.BoolVar(&cfg.install, "install", false, "Save these settings to the config file and start rtelegram with the system")
	fs.BoolVar(&cfg.uninstall, "uninstall", false, "Stop starting rtelegram with the system")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if len(fs.Args()) != 0 {
		return config{}, unexpectedArguments(fs.Args())
	}
	if cfg.showVersion {
		return cfg, nil
	}
	if cfg.install && cfg.uninstall {
		return config{}, errors.New("-install and -uninstall cannot be used together")
	}
	if cfg.uninstall {
		return cfg, nil
	}
	cfg.rewriteConfig = cfg.configPath == ""
	fs.Visit(func(f *flag.Flag) { cfg.rewriteConfig = cfg.rewriteConfig || !nonSettings[f.Name] })
	if cfg.configPath != "" {
		if err := applyConfigFile(fs, cfg.configPath); err != nil {
			return config{}, err
		}
	}
	cfg.settings = make(map[string]string)
	fs.Visit(func(f *flag.Flag) {
		if !nonSettings[f.Name] {
			cfg.settings[f.Name] = f.Value.String()
		}
	})
	fromEnvironment := func(name, variable string, value *string) {
		if *value == "" {
			if *value = getenv(variable); *value != "" {
				cfg.settings[name] = *value
				cfg.rewriteConfig = true
			}
		}
	}
	fromEnvironment("token", "RT_TOKEN", &cfg.token)
	if strings.TrimSpace(cfg.token) == "" {
		return config{}, errors.New("telegram token is missing")
	}
	fromEnvironment("masters", "RT_MASTERS", &mastersText)
	masters, err := parsePrincipals(mastersText)
	if err != nil {
		return config{}, err
	}
	cfg.masters = masters
	fromEnvironment("url", "RT_URL", &cfg.rtorrentAddress)
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
	if cfg.watchInterval < 5*time.Second {
		return config{}, errors.New("-watch-interval must be at least 5s")
	}
	if cfg.stallAfter < 0 {
		return config{}, errors.New("-stall-after must not be negative")
	}
	if cfg.lowDisk, err = parseSize(*lowDisk); err != nil {
		return config{}, fmt.Errorf("-low-disk: %w", err)
	}
	fromEnvironment("indexer-url", "RT_INDEXER_URL", &cfg.indexerURL)
	fromEnvironment("indexer-key", "RT_INDEXER_KEY", &cfg.indexerKey)
	if cfg.indexerURL != "" {
		if parsed, err := url.Parse(cfg.indexerURL); err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return config{}, errors.New("-indexer-url must be an http:// or https:// URL")
		}
	}
	if cfg.feedInterval < 5*time.Minute {
		return config{}, errors.New("-feed-interval must be at least 5m")
	}
	if cfg.dataRoot != "" && cfg.dataRoot != dataRootOff {
		if !filepath.IsAbs(cfg.dataRoot) {
			return config{}, errors.New("-data-root must be an absolute path, or off")
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

// parsePrincipals reads the comma-separated user IDs of the masters.
// Usernames are refused: they can be changed and then taken by someone else.
func parsePrincipals(text string) (principals, error) {
	p := principals{ids: make(map[int64]struct{})}
	if strings.TrimSpace(text) == "" {
		return p, errors.New("at least one Telegram master is required")
	}
	for _, raw := range strings.Split(text, ",") {
		value := strings.TrimSpace(raw)
		if value == "" {
			return principals{}, errors.New("telegram masters must not contain empty entries")
		}
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return principals{}, fmt.Errorf("telegram masters must be numeric user IDs, and %q is not; usernames are refused because they can change hands. "+
				"To find your ID, start rtelegram with RT_MASTERS=1 and message the bot: its log shows the ID of each user it ignores", value)
		}
		if id <= 0 {
			return principals{}, fmt.Errorf("invalid Telegram user ID %q", value)
		}
		p.ids[id] = struct{}{}
	}
	return p, nil
}

func (p principals) authorized(user *models.User) bool {
	if user == nil {
		return false
	}
	_, ok := p.ids[user.ID]
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
		a.ignore(ctx, message)
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
	case "checking", "ch", "hashing", "ha":
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

// ignore handles a message from someone who is not a master. The numeric ID
// of each such user who messages the bot privately is logged once, so an
// operator can find the ID to authorize; group members are not logged. While
// the only master is the setup placeholder, the bot also tells them their ID,
// since there is nothing yet that a stranger could learn or control.
func (a *application) ignore(ctx context.Context, message *models.Message) {
	if message.From == nil || message.Chat.Type != models.ChatTypePrivate {
		return
	}
	id := message.From.ID
	if a.masters.setup() {
		a.send(ctx, message.Chat.ID, fmt.Sprintf("Your Telegram user ID is %d. Restart rtelegram with RT_MASTERS=%d (or -masters %d), and this account can use the bot.", id, id, id))
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
	a.logger.Printf("[WARN] Telegram user ID %d%s is not a master, so the bot ignores them. To let them in, add %d to RT_MASTERS (or -masters) and restart.",
		id, username, id)
}

// unexpectedArguments explains arguments that are not flags. rtelegram takes
// none, so they are usually a mistyped flag; a lone "-", for one, ends the
// flags, and what follows it is not read as flags.
func unexpectedArguments(args []string) error {
	message := "unexpected arguments: " + strings.Join(args, " ")
	rest := strings.Join(args[1:], " ")
	switch {
	case args[0] == "-" && rest != "":
		message += fmt.Sprintf(`; a lone "-" ends the flags, so %s was not read. Remove the "-"`, rest)
	case strings.HasPrefix(args[0], "–") || strings.HasPrefix(args[0], "—"):
		message += fmt.Sprintf("; %s starts with a long dash rather than a hyphen. Type flags with -, as in -data-root=/path", args[0])
	default:
		message += "; rtelegram takes only flags, such as -data-root=/path"
	}
	return errors.New(message)
}

func (a *application) whoami(ctx context.Context, chatID int64, user *models.User) {
	username := ""
	if user.Username != "" {
		username = " (@" + user.Username + ")"
	}
	a.send(ctx, chatID, fmt.Sprintf("User ID: %d%s\nChat ID: %d\n\nMasters are set by user ID, in RT_MASTERS. Send /notify in a chat to choose its notifications.",
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
