package tgbotapi

// ModeMarkdown represents Telegram's markdown parse mode.
const ModeMarkdown = "Markdown"

// ChatTyping represents the chat action for typing.
const ChatTyping = "typing"

// Chattable is implemented by request payloads that can be sent.
type Chattable interface {
	isChattable()
}

// MessageConfig represents a basic message send request.
type MessageConfig struct {
	ChatID                int64
	Text                  string
	ParseMode             string
	DisableWebPagePreview bool
}

func (MessageConfig) isChattable() {}

// ChatActionConfig represents a chat action request.
type ChatActionConfig struct {
	ChatID int64
	Action string
}

func (ChatActionConfig) isChattable() {}

// EditMessageTextConfig represents an edit message request.
type EditMessageTextConfig struct {
	ChatID    int64
	MessageID int
	Text      string
	ParseMode string
}

func (EditMessageTextConfig) isChattable() {}

// NewMessage constructs a message configuration.
func NewMessage(chatID int64, text string) MessageConfig {
	return MessageConfig{ChatID: chatID, Text: text}
}

// NewChatAction constructs a chat action configuration.
func NewChatAction(chatID int64, action string) ChatActionConfig {
	return ChatActionConfig{ChatID: chatID, Action: action}
}

// NewEditMessageText constructs an edit message configuration.
func NewEditMessageText(chatID int64, messageID int, text string) EditMessageTextConfig {
	return EditMessageTextConfig{ChatID: chatID, MessageID: messageID, Text: text}
}

// UpdateConfig configures the update polling request.
type UpdateConfig struct {
	Offset  int
	Timeout int
}

// NewUpdate constructs an update configuration.
func NewUpdate(offset int) UpdateConfig {
	return UpdateConfig{Offset: offset}
}

// User represents a Telegram user.
type User struct {
	UserName string
}

// String returns the username for logging.
func (u *User) String() string {
	if u == nil {
		return ""
	}
	return u.UserName
}

// Chat represents the message chat.
type Chat struct {
	ID int64
}

// Document represents a Telegram document attachment.
type Document struct {
	FileID   string
	FileName string
}

// Message represents a Telegram message.
type Message struct {
	Document  *Document
	Caption   string
	From      *User
	Chat      *Chat
	Text      string
	MessageID int
}

// Update represents an incoming update.
type Update struct {
	Message *Message
}

// FileConfig identifies a file to retrieve.
type FileConfig struct {
	FileID string
}

// File represents file metadata.
type File struct {
	FileID   string
	FilePath string
}

// Link returns a link to the file content.
func (f File) Link(string) string {
	if f.FilePath != "" {
		return f.FilePath
	}
	return f.FileID
}

// BotAPI is a lightweight stub of the Telegram bot client.
type BotAPI struct {
	Self    User
	updates chan Update
	nextID  int
}

// SentPayloads keeps track of the most recent payloads sent through the stub.
var SentPayloads []Chattable

// NewBotAPI constructs a stub bot client.
func NewBotAPI(string) (*BotAPI, error) {
	bot := &BotAPI{updates: make(chan Update)}
	bot.Self.UserName = "stub-bot"
	return bot, nil
}

// GetUpdatesChan returns the update channel.
func (b *BotAPI) GetUpdatesChan(UpdateConfig) (<-chan Update, error) {
	return b.updates, nil
}

// Send simulates sending a chattable payload.
func (b *BotAPI) Send(payload Chattable) (Message, error) {
	SentPayloads = append(SentPayloads, payload)
	b.nextID++
	return Message{MessageID: b.nextID}, nil
}

// GetFile simulates fetching a remote file.
func (b *BotAPI) GetFile(cfg FileConfig) (File, error) {
	return File{FileID: cfg.FileID, FilePath: cfg.FileID}, nil
}
