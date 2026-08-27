package trafficlogger

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/apernet/hysteria/core/v2/server"
)

const (
	indexHTML = `<!DOCTYPE html><html lang="en"><head> <meta charset="UTF-8"> <meta name="viewport" content="width=device-width, initial-scale=1.0"> <title>Hysteria Traffic Stats API Server</title> <style>body{font-family: Arial, sans-serif; display: flex; justify-content: center; align-items: center; height: 100vh; margin: 0; padding: 0; background-color: #f4f4f4;}.container{padding: 20px; background-color: #fff; box-shadow: 0 4px 6px rgba(0, 0, 0, 0.1); border-radius: 5px;}</style></head><body> <div class="container"> <p>This is a Hysteria Traffic Stats API server.</p><p>Check the documentation for usage.</p></div></body></html>`
)

// TrafficStatsServer implements both server.TrafficLogger and http.Handler
// to provide a simple HTTP API to get the traffic stats per user.
type TrafficStatsServer interface {
	server.TrafficLogger
	http.Handler
	SetAuthReloader(AuthReloader)
}

type AuthReloader interface {
	Reload() (int, error)
}

func NewTrafficStatsServer(secret string) TrafficStatsServer {
	return &trafficStatsServerImpl{
		StatsMap:      make(map[string]*trafficStatsEntry),
		OnlineMap:     make(map[string]int),
		StreamMap:     make(map[server.HyStream]*server.StreamStats),
		ConnectionMap: make(map[string]map[*trackedConnection]struct{}),
		Secret:        secret,
	}
}

type trafficStatsServerImpl struct {
	Mutex         sync.RWMutex
	StatsMap      map[string]*trafficStatsEntry
	OnlineMap     map[string]int
	StreamMap     map[server.HyStream]*server.StreamStats
	ConnectionMap map[string]map[*trackedConnection]struct{}
	Secret        string
	AuthReloader  AuthReloader
}

type trafficStatsEntry struct {
	Tx uint64 `json:"tx"`
	Rx uint64 `json:"rx"`
}

type trackedConnection struct {
	disconnect func()
}

func (s *trafficStatsServerImpl) SetAuthReloader(reloader AuthReloader) {
	s.Mutex.Lock()
	s.AuthReloader = reloader
	s.Mutex.Unlock()
}

func (s *trafficStatsServerImpl) LogTraffic(id string, tx, rx uint64) (ok bool) {
	s.Mutex.Lock()
	defer s.Mutex.Unlock()

	entry, ok := s.StatsMap[id]
	if !ok {
		entry = &trafficStatsEntry{}
		s.StatsMap[id] = entry
	}
	entry.Tx += tx
	entry.Rx += rx

	return true
}

func (s *trafficStatsServerImpl) TrackConnection(id string, disconnect func()) func() {
	connection := &trackedConnection{disconnect: disconnect}
	s.Mutex.Lock()
	connections := s.ConnectionMap[id]
	if connections == nil {
		connections = make(map[*trackedConnection]struct{})
		s.ConnectionMap[id] = connections
	}
	connections[connection] = struct{}{}
	s.Mutex.Unlock()

	return sync.OnceFunc(func() {
		s.Mutex.Lock()
		defer s.Mutex.Unlock()
		connections := s.ConnectionMap[id]
		delete(connections, connection)
		if len(connections) == 0 {
			delete(s.ConnectionMap, id)
		}
	})
}

// LogOnlineState updates the online state to the online map.
func (s *trafficStatsServerImpl) LogOnlineState(id string, online bool) {
	s.Mutex.Lock()
	defer s.Mutex.Unlock()

	if online {
		s.OnlineMap[id]++
	} else {
		s.OnlineMap[id]--
		if s.OnlineMap[id] <= 0 {
			delete(s.OnlineMap, id)
		}
	}
}

func (s *trafficStatsServerImpl) TraceStream(stream server.HyStream, stats *server.StreamStats) {
	s.Mutex.Lock()
	defer s.Mutex.Unlock()

	s.StreamMap[stream] = stats
}

func (s *trafficStatsServerImpl) UntraceStream(stream server.HyStream) {
	s.Mutex.Lock()
	defer s.Mutex.Unlock()

	delete(s.StreamMap, stream)
}

func (s *trafficStatsServerImpl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.Secret != "" && r.Header.Get("Authorization") != s.Secret {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/" {
		_, _ = w.Write([]byte(indexHTML))
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/traffic" {
		s.getTraffic(w, r)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/kick" {
		s.kick(w, r)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/online" {
		s.getOnline(w, r)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/dump/streams" {
		s.getDumpStreams(w, r)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/auth/reload" {
		s.reloadAuth(w)
		return
	}
	http.NotFound(w, r)
}

func (s *trafficStatsServerImpl) reloadAuth(w http.ResponseWriter) {
	s.Mutex.RLock()
	reloader := s.AuthReloader
	s.Mutex.RUnlock()
	if reloader == nil {
		http.Error(w, "file authentication is not configured", http.StatusNotFound)
		return
	}
	count, err := reloader.Reload()
	if err != nil {
		http.Error(w, "failed to reload authentication snapshot", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(struct {
		Users int `json:"users"`
	}{Users: count})
}

func (s *trafficStatsServerImpl) getTraffic(w http.ResponseWriter, r *http.Request) {
	bClear, _ := strconv.ParseBool(r.URL.Query().Get("clear"))
	var jb []byte
	var err error
	if bClear {
		s.Mutex.Lock()
		jb, err = json.Marshal(s.StatsMap)
		s.StatsMap = make(map[string]*trafficStatsEntry)
		s.Mutex.Unlock()
	} else {
		s.Mutex.RLock()
		jb, err = json.Marshal(s.StatsMap)
		s.Mutex.RUnlock()
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(jb)
}

func (s *trafficStatsServerImpl) getOnline(w http.ResponseWriter, r *http.Request) {
	s.Mutex.RLock()
	defer s.Mutex.RUnlock()

	jb, err := json.Marshal(s.OnlineMap)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(jb)
}

type dumpStreamEntry struct {
	State string `json:"state"`

	Auth       string `json:"auth"`
	Connection uint32 `json:"connection"`
	Stream     uint64 `json:"stream"`

	ReqAddr       string `json:"req_addr"`
	HookedReqAddr string `json:"hooked_req_addr"`

	Tx uint64 `json:"tx"`
	Rx uint64 `json:"rx"`

	InitialAt    string `json:"initial_at"`
	LastActiveAt string `json:"last_active_at"`

	// for text/plain output
	initialTime    time.Time
	lastActiveTime time.Time
}

func (e *dumpStreamEntry) fromStreamStats(stream server.HyStream, s *server.StreamStats) {
	e.State = s.State.Load().String()
	e.Auth = s.AuthID
	e.Connection = s.ConnID
	e.Stream = uint64(stream.StreamID())
	e.ReqAddr = s.ReqAddr.Load()
	e.HookedReqAddr = s.HookedReqAddr.Load()
	e.Tx = s.Tx.Load()
	e.Rx = s.Rx.Load()
	e.initialTime = s.InitialTime
	e.lastActiveTime = s.LastActiveTime.Load()
	e.InitialAt = e.initialTime.Format(time.RFC3339Nano)
	e.LastActiveAt = e.lastActiveTime.Format(time.RFC3339Nano)
}

func formatDumpStreamLine(state, auth, connection, stream, reqAddr, hookedReqAddr, tx, rx, lifetime, lastActive string) string {
	return fmt.Sprintf("%-8s %-12s %12s %8s %12s %12s %12s %12s %-16s %s", state, auth, connection, stream, tx, rx, lifetime, lastActive, reqAddr, hookedReqAddr)
}

func (e *dumpStreamEntry) String() string {
	stateText := strings.ToUpper(e.State)
	connectionText := fmt.Sprintf("%08X", e.Connection)
	streamText := strconv.FormatUint(e.Stream, 10)
	reqAddrText := e.ReqAddr
	if reqAddrText == "" {
		reqAddrText = "-"
	}
	hookedReqAddrText := e.HookedReqAddr
	if hookedReqAddrText == "" {
		hookedReqAddrText = "-"
	}
	txText := strconv.FormatUint(e.Tx, 10)
	rxText := strconv.FormatUint(e.Rx, 10)
	lifetime := time.Since(e.initialTime)
	if lifetime < 10*time.Minute {
		lifetime = lifetime.Round(time.Millisecond)
	} else {
		lifetime = lifetime.Round(time.Second)
	}
	lastActive := time.Since(e.lastActiveTime)
	if lastActive < 10*time.Minute {
		lastActive = lastActive.Round(time.Millisecond)
	} else {
		lastActive = lastActive.Round(time.Second)
	}

	return formatDumpStreamLine(stateText, e.Auth, connectionText, streamText, reqAddrText, hookedReqAddrText, txText, rxText, lifetime.String(), lastActive.String())
}

func (s *trafficStatsServerImpl) getDumpStreams(w http.ResponseWriter, r *http.Request) {
	var entries []dumpStreamEntry

	s.Mutex.RLock()
	entries = make([]dumpStreamEntry, len(s.StreamMap))
	index := 0
	for stream, stats := range s.StreamMap {
		entries[index].fromStreamStats(stream, stats)
		index++
	}
	s.Mutex.RUnlock()

	slices.SortFunc(entries, func(lhs, rhs dumpStreamEntry) int {
		if ret := cmp.Compare(lhs.Auth, rhs.Auth); ret != 0 {
			return ret
		}
		if ret := cmp.Compare(lhs.Connection, rhs.Connection); ret != 0 {
			return ret
		}
		if ret := cmp.Compare(lhs.Stream, rhs.Stream); ret != 0 {
			return ret
		}
		return 0
	})

	accept := r.Header.Get("Accept")

	if strings.Contains(accept, "text/plain") {
		// Generate netstat-like output for humans
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")

		// Print table header
		_, _ = fmt.Fprintln(w, formatDumpStreamLine("State", "Auth", "Connection", "Stream", "Req-Addr", "Hooked-Req-Addr", "TX-Bytes", "RX-Bytes", "Lifetime", "Last-Active"))
		for _, entry := range entries {
			_, _ = fmt.Fprintln(w, entry.String())
		}
		return
	}

	// Response with json by default
	wrapper := struct {
		Streams []dumpStreamEntry `json:"streams"`
	}{entries}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	err := json.NewEncoder(w).Encode(&wrapper)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *trafficStatsServerImpl) kick(w http.ResponseWriter, r *http.Request) {
	var ids []string
	err := json.NewDecoder(r.Body).Decode(&ids)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var disconnects []func()
	s.Mutex.Lock()
	for _, id := range ids {
		for connection := range s.ConnectionMap[id] {
			disconnects = append(disconnects, connection.disconnect)
		}
		delete(s.ConnectionMap, id)
	}
	s.Mutex.Unlock()
	for _, disconnect := range disconnects {
		disconnect()
	}

	w.WriteHeader(http.StatusOK)
}
