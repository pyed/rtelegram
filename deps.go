package main

import (
	"github.com/pyed/rtapi"
	tgbotapi "gopkg.in/telegram-bot-api.v4"
)

type TelegramBot interface {
	Send(tgbotapi.Chattable) (tgbotapi.Message, error)
	GetUpdatesChan(tgbotapi.UpdateConfig) (<-chan tgbotapi.Update, error)
	GetFile(tgbotapi.FileConfig) (tgbotapi.File, error)
}

type RtorrentClient interface {
	Torrents() (rtapi.Torrents, error)
	Start(...*rtapi.Torrent) error
	Stop(...*rtapi.Torrent) error
	Check(...*rtapi.Torrent) error
	Delete(bool, ...*rtapi.Torrent) error
	Download(string) error
	DownloadWithOptions(*rtapi.DotTorrentWithOptions) error
	Stats() (*rtapi.Stats, error)
	Speeds() (uint64, uint64)
	GetTorrent(string) (*rtapi.Torrent, error)
	Version() string
}

type realRtorrent struct {
	client *rtapi.Rtorrent
}

func (r realRtorrent) Torrents() (rtapi.Torrents, error) {
	return r.client.Torrents()
}

func (r realRtorrent) Start(torrents ...*rtapi.Torrent) error {
	return r.client.Start(torrents...)
}

func (r realRtorrent) Stop(torrents ...*rtapi.Torrent) error {
	return r.client.Stop(torrents...)
}

func (r realRtorrent) Check(torrents ...*rtapi.Torrent) error {
	return r.client.Check(torrents...)
}

func (r realRtorrent) Delete(deleteData bool, torrents ...*rtapi.Torrent) error {
	return r.client.Delete(deleteData, torrents...)
}

func (r realRtorrent) Download(uri string) error {
	return r.client.Download(uri)
}

func (r realRtorrent) DownloadWithOptions(opts *rtapi.DotTorrentWithOptions) error {
	return r.client.DownloadWithOptions(opts)
}

func (r realRtorrent) Stats() (*rtapi.Stats, error) {
	return r.client.Stats()
}

func (r realRtorrent) Speeds() (uint64, uint64) {
	return r.client.Speeds()
}

func (r realRtorrent) GetTorrent(hash string) (*rtapi.Torrent, error) {
	return r.client.GetTorrent(hash)
}

func (r realRtorrent) Version() string {
	return r.client.Version
}
