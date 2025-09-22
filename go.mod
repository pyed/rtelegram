module github.com/pyed/rtelegram

go 1.20

require (
	github.com/pyed/go-humanize v0.0.0
	github.com/pyed/rtapi v0.0.0
	github.com/pyed/tailer v0.0.0
	gopkg.in/telegram-bot-api.v4 v4.0.0
)

replace github.com/pyed/go-humanize => ./stubs/go-humanize

replace github.com/pyed/rtapi => ./stubs/rtapi

replace github.com/pyed/tailer => ./stubs/tailer

replace gopkg.in/telegram-bot-api.v4 => ./stubs/telegram
