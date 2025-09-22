package rtapi

import "fmt"

// State represents the torrent state.
type State string

const (
	Hashing  State = "Hashing"
	Leeching State = "Leeching"
	Seeding  State = "Seeding"
	Complete State = "Complete"
	Stopped  State = "Stopped"
	Error    State = "Error"
)

// SortMode represents the sorting mode for torrents.
type SortMode int

const (
	ByName SortMode = iota
	ByNameRev
	ByDownRate
	ByDownRateRev
	ByUpRate
	ByUpRateRev
	BySize
	BySizeRev
	ByRatio
	ByRatioRev
	ByAge
	ByAgeRev
	ByUpTotal
	ByUpTotalRev
)

// CurrentSorting mimics the package level sorting configuration in the real library.
var CurrentSorting SortMode

// Tracker represents torrent tracker information.
type Tracker struct {
	host string
}

// Hostname returns the tracker's hostname.
func (t *Tracker) Hostname() string {
	if t == nil {
		return ""
	}
	return t.host
}

// Ratio wraps a floating point ratio value and implements the fmt interfaces
// required by the main package's formatted strings.
type Ratio struct {
	Value float64
}

// String returns the ratio formatted with two decimal places.
func (r Ratio) String() string {
	return fmt.Sprintf("%.2f", r.Value)
}

// Format allows the ratio to be used with %f and %s verbs without vet warnings.
func (r Ratio) Format(state fmt.State, verb rune) {
	switch verb {
	case 'f', 'F':
		fmt.Fprintf(state, "%f", r.Value)
	case 's', 'v':
		fmt.Fprintf(state, "%s", r.String())
	default:
		fmt.Fprintf(state, "%f", r.Value)
	}
}

// Torrent is a simplified representation of a torrent record.
type Torrent struct {
	Name      string
	State     State
	Percent   string
	Completed uint64
	DownRate  uint64
	UpRate    uint64
	UpTotal   uint64
	Ratio     Ratio
	Age       int
	ETA       int
	Message   string
	Tracker   *Tracker
	Hash      string
}

// Torrents represents a slice of torrents.
type Torrents []*Torrent

// Sort is a no-op helper to satisfy calls from the main package.
func (t Torrents) Sort(_ SortMode) {}

// Stats describes rTorrent statistics.
type Stats struct {
	ThrottleUp   uint64
	ThrottleDown uint64
	Port         string
	Directory    string
	TotalUp      uint64
	TotalDown    uint64
}

// DotTorrentWithOptions holds information for adding torrents with options.
type DotTorrentWithOptions struct {
	Link  string
	Name  string
	Dir   string
	Label string
}

// Rtorrent is a lightweight stubbed client.
type Rtorrent struct {
	Version  string
	torrents Torrents
	stats    Stats
	speeds   struct {
		down uint64
		up   uint64
	}
}

// NewRtorrent constructs a stubbed client instance.
func NewRtorrent(string) (*Rtorrent, error) {
	return &Rtorrent{Version: "stub"}, nil
}

// Torrents returns the stored torrents slice.
func (r *Rtorrent) Torrents() (Torrents, error) {
	return r.torrents, nil
}

// Start is a stubbed torrent start call.
func (r *Rtorrent) Start(_ ...*Torrent) error { return nil }

// Stop is a stubbed torrent stop call.
func (r *Rtorrent) Stop(_ ...*Torrent) error { return nil }

// Check is a stubbed torrent check call.
func (r *Rtorrent) Check(_ ...*Torrent) error { return nil }

// Delete is a stubbed torrent delete call.
func (r *Rtorrent) Delete(_ bool, _ ...*Torrent) error { return nil }

// Download is a stubbed download call.
func (r *Rtorrent) Download(_ string) error { return nil }

// DownloadWithOptions is a stubbed download with options call.
func (r *Rtorrent) DownloadWithOptions(_ *DotTorrentWithOptions) error { return nil }

// Stats returns stubbed statistics information.
func (r *Rtorrent) Stats() (*Stats, error) {
	return &r.stats, nil
}

// Speeds returns stubbed download and upload speeds.
func (r *Rtorrent) Speeds() (uint64, uint64) {
	return r.speeds.down, r.speeds.up
}

// GetTorrent returns a stub torrent entry.
func (r *Rtorrent) GetTorrent(string) (*Torrent, error) {
	if len(r.torrents) > 0 {
		return r.torrents[0], nil
	}
	return &Torrent{Tracker: &Tracker{}}, nil
}
