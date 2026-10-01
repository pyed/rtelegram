package main

import (
	"bufio"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/pyed/rtapi"
)

// fakeRtorrent is an rTorrent stand-in that speaks SCGI and XML-RPC, so
// handlers run against a real *rtapi.Rtorrent. It decodes each call, answers
// from its torrent list, and records the calls and request bodies it saw.
type fakeRtorrent struct {
	t         *testing.T
	mu        sync.Mutex
	torrents  rtapi.Torrents
	directory string // directory.default
	downRate  uint64
	upRate    uint64
	load      func(body string) *rtapi.Torrent
	requests  []string
	calls     []fakeCall
}

type fakeCall struct {
	method string
	args   []string
}

func newFakeRtorrent(t *testing.T, torrents ...*rtapi.Torrent) (*fakeRtorrent, *rtapi.Rtorrent) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeRtorrent{t: t, torrents: torrents, directory: "/downloads"}
	var connections sync.WaitGroup
	connections.Add(1)
	go func() {
		defer connections.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			go func() {
				defer connections.Done()
				defer conn.Close()
				fake.serve(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		connections.Wait()
	})
	client, err := rtapi.NewRtorrent(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return fake, client
}

// onLoad sets the torrent the fake loads for each load request; without it,
// load requests are acknowledged but nothing is loaded.
func (f *fakeRtorrent) onLoad(load func(body string) *rtapi.Torrent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.load = load
}

// set runs change while holding the fake's lock, for tests that change its
// state after requests may have started.
func (f *fakeRtorrent) set(change func(*fakeRtorrent)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

func (f *fakeRtorrent) requestsContaining(text string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var matches []string
	for _, request := range f.requests {
		if strings.Contains(request, text) {
			matches = append(matches, request)
		}
	}
	return matches
}

// called returns the arguments of every call to the methods named.
func (f *fakeRtorrent) called(methods ...string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var matches [][]string
	for _, call := range f.calls {
		for _, method := range methods {
			if call.method == method {
				matches = append(matches, call.args)
			}
		}
	}
	return matches
}

func (f *fakeRtorrent) loadCalls() [][]string {
	return f.called("load.start", "load.start_verbose", "load.normal", "load.verbose",
		"load.raw", "load.raw_start", "load.raw_verbose", "load.raw_start_verbose")
}

func (f *fakeRtorrent) serve(conn net.Conn) {
	body, err := readSCGIBody(conn)
	if err != nil {
		f.t.Errorf("read SCGI request: %v", err)
		return
	}
	if _, err := io.WriteString(conn, f.respond(body)); err != nil {
		f.t.Errorf("write SCGI response: %v", err)
	}
}

type xmlrpcCallValue struct {
	String  *string           `xml:"string"`
	Base64  *string           `xml:"base64"`
	Array   []xmlrpcCallValue `xml:"array>data>value"`
	Members []struct {
		Name  string          `xml:"name"`
		Value xmlrpcCallValue `xml:"value"`
	} `xml:"struct>member"`
	Text string `xml:",chardata"`
}

func (v xmlrpcCallValue) text() string {
	switch {
	case v.String != nil:
		return *v.String
	case v.Base64 != nil:
		return *v.Base64
	}
	return v.Text
}

func (v xmlrpcCallValue) member(name string) xmlrpcCallValue {
	for _, member := range v.Members {
		if member.Name == name {
			return member.Value
		}
	}
	return xmlrpcCallValue{}
}

func texts(values []xmlrpcCallValue) []string {
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = value.text()
	}
	return result
}

func (f *fakeRtorrent) respond(body string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, body)
	var request struct {
		MethodName string            `xml:"methodName"`
		Params     []xmlrpcCallValue `xml:"params>param>value"`
	}
	if err := xml.Unmarshal([]byte(body), &request); err != nil {
		f.t.Errorf("decode XML-RPC request: %v", err)
		return ""
	}
	if request.MethodName != "system.multicall" {
		value, fault := f.call(request.MethodName, texts(request.Params), body)
		if fault {
			return "Status: 200 OK\r\n\r\n<methodResponse><fault><value>" + value + "</value></fault></methodResponse>"
		}
		return xmlrpcResponse(value)
	}
	if len(request.Params) == 0 {
		f.t.Errorf("system.multicall without calls: %s", body)
		return ""
	}
	results := make([]string, 0, len(request.Params[0].Array))
	for _, call := range request.Params[0].Array {
		value, fault := f.call(call.member("methodName").text(), texts(call.member("params").Array), body)
		if !fault {
			value = xmlrpcArray(value) // system.multicall wraps each result
		}
		results = append(results, value)
	}
	return xmlrpcResponse(xmlrpcArray(results...))
}

// call answers one XML-RPC call and reports whether the answer is a fault.
func (f *fakeRtorrent) call(method string, args []string, body string) (string, bool) {
	f.calls = append(f.calls, fakeCall{method: method, args: args})
	switch method {
	case "system.client_version":
		return xmlrpcString("0.9.8"), false
	case "system.library_version":
		return xmlrpcString("0.13.8"), false
	case "d.multicall2":
		rows := make([]string, len(f.torrents))
		for i, torrent := range f.torrents {
			fields := make([]string, 0, len(args))
			for _, field := range args[min(2, len(args)):] {
				fields = append(fields, f.field(torrent, strings.TrimSuffix(field, "=")))
			}
			rows[i] = xmlrpcArray(fields...)
		}
		return xmlrpcArray(rows...), false
	case "load.start", "load.start_verbose", "load.normal", "load.verbose",
		"load.raw", "load.raw_start", "load.raw_verbose", "load.raw_start_verbose":
		if f.load != nil {
			if torrent := f.load(body); torrent != nil {
				f.torrents = append(f.torrents, torrent)
			}
		}
		return xmlrpcInt(0), false
	case "throttle.global_down.rate":
		return xmlrpcInt(int(f.downRate)), false
	case "throttle.global_up.rate":
		return xmlrpcInt(int(f.upRate)), false
	case "throttle.up.max", "throttle.down.max":
		return xmlrpcInt(0), false
	case "throttle.global_up.total":
		return xmlrpcInt(3 << 30), false
	case "throttle.global_down.total":
		return xmlrpcInt(5 << 30), false
	case "network.listen.port":
		return xmlrpcInt(6890), false
	case "directory.default":
		return xmlrpcString(f.directory), false
	}
	if len(args) == 0 {
		f.t.Errorf("unexpected rTorrent call %s()", method)
		return xmlrpcFault(-500, "unexpected call"), true
	}
	// The remaining calls target one torrent, as HASH or HASH:t0.
	hash, _, _ := strings.Cut(args[0], ":")
	index := -1
	for i, torrent := range f.torrents {
		if strings.EqualFold(torrent.Hash, hash) {
			index = i
		}
	}
	if index < 0 {
		return xmlrpcFault(-501, "Could not find info-hash."), true
	}
	torrent := f.torrents[index]
	switch method {
	case "t.url":
		if torrent.Tracker == nil {
			return xmlrpcFault(-501, "Could not find tracker."), true
		}
		return xmlrpcString(torrent.Tracker.String()), false
	case "d.start", "d.stop", "d.check_hash":
		return xmlrpcInt(0), false
	case "d.erase":
		f.torrents = append(f.torrents[:index:index], f.torrents[index+1:]...)
		return xmlrpcInt(0), false
	}
	return f.field(torrent, method), false
}

// field renders the d.* getter rtapi uses for each Torrent field. States are
// rendered as the flags rtapi derives them from.
func (f *fakeRtorrent) field(torrent *rtapi.Torrent, name string) string {
	flag := func(on bool) string {
		if on {
			return xmlrpcInt(1)
		}
		return xmlrpcInt(0)
	}
	state := torrent.State
	switch name {
	case "d.name":
		return xmlrpcString(torrent.Name)
	case "d.hash":
		return xmlrpcString(torrent.Hash)
	case "d.down.rate":
		return xmlrpcInt(int(torrent.DownRate))
	case "d.up.rate":
		return xmlrpcInt(int(torrent.UpRate))
	case "d.size_bytes":
		return xmlrpcInt(int(torrent.Size))
	case "d.completed_bytes":
		return xmlrpcInt(int(torrent.Completed))
	case "d.ratio":
		return xmlrpcInt(int(torrent.Ratio * 1000))
	case "d.up.total":
		return xmlrpcInt(int(torrent.UpTotal))
	case "d.load_date":
		return xmlrpcInt(int(torrent.Age))
	case "d.message":
		return xmlrpcString(torrent.Message)
	case "d.base_path":
		return xmlrpcString(torrent.Path)
	case "d.is_active":
		return flag(state == rtapi.Leeching || state == rtapi.Seeding || state == rtapi.Error)
	case "d.connection_current":
		if state == rtapi.Seeding {
			return xmlrpcString("seed")
		}
		return xmlrpcString("leech")
	case "d.complete":
		return flag(state == rtapi.Seeding || state == rtapi.Complete)
	case "d.hashing":
		return flag(state == rtapi.Hashing)
	case "d.custom1":
		return xmlrpcString(torrent.Label)
	case "d.directory":
		return xmlrpcString(torrent.Directory)
	case "d.is_multi_file":
		return flag(torrent.MultiFile)
	}
	f.t.Errorf("unexpected torrent field %s", name)
	return xmlrpcString("")
}

func readSCGIBody(r io.Reader) (string, error) {
	reader := bufio.NewReader(r)
	lengthText, err := reader.ReadString(':')
	if err != nil {
		return "", err
	}
	length, err := strconv.Atoi(strings.TrimSuffix(lengthText, ":"))
	if err != nil {
		return "", err
	}
	headers := make([]byte, length+1) // the netstring ends with a comma
	if _, err := io.ReadFull(reader, headers); err != nil {
		return "", err
	}
	fields := strings.Split(string(headers[:length]), "\x00")
	if len(fields) < 2 || fields[0] != "CONTENT_LENGTH" {
		return "", fmt.Errorf("SCGI headers must start with CONTENT_LENGTH: %q", fields)
	}
	size, err := strconv.Atoi(fields[1])
	if err != nil {
		return "", err
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(reader, body); err != nil {
		return "", err
	}
	return string(body), nil
}

func xmlrpcResponse(value string) string {
	return "Status: 200 OK\r\nContent-Type: text/xml\r\n\r\n" +
		`<?xml version="1.0"?><methodResponse><params><param><value>` + value +
		`</value></param></params></methodResponse>`
}

func xmlrpcArray(values ...string) string {
	var body strings.Builder
	body.WriteString("<array><data>")
	for _, value := range values {
		body.WriteString("<value>" + value + "</value>")
	}
	body.WriteString("</data></array>")
	return body.String()
}

func xmlrpcFault(code int, message string) string {
	return fmt.Sprintf(`<struct><member><name>faultCode</name><value><i4>%d</i4></value></member>`+
		`<member><name>faultString</name><value>%s</value></member></struct>`, code, xmlrpcString(message))
}

func xmlrpcString(text string) string {
	var escaped strings.Builder
	_ = xml.EscapeText(&escaped, []byte(text))
	return "<string>" + escaped.String() + "</string>"
}

func xmlrpcInt(n int) string { return fmt.Sprintf("<i8>%d</i8>", n) }
