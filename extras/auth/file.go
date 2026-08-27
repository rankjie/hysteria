package auth

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apernet/hysteria/core/v2/server"
)

const defaultFileAuthRefreshInterval = time.Second

var _ server.Authenticator = &FileAuthenticator{}

type fileAuthSnapshot struct {
	users map[string]string
}

type FileAuthenticator struct {
	path            string
	refreshInterval time.Duration
	snapshot        atomic.Pointer[fileAuthSnapshot]
	reloadMu        sync.Mutex
	lastFileInfo    os.FileInfo
	lastReloadError string
	stop            chan struct{}
	done            chan struct{}
	closeOnce       sync.Once
}

func NewFileAuthenticator(path string, refreshInterval time.Duration) (*FileAuthenticator, error) {
	if path == "" {
		return nil, errorsNewFileAuth("empty path")
	}
	if refreshInterval <= 0 {
		refreshInterval = defaultFileAuthRefreshInterval
	}
	snapshot, info, err := loadFileAuthSnapshot(path)
	if err != nil {
		return nil, err
	}
	a := &FileAuthenticator{
		path:            path,
		refreshInterval: refreshInterval,
		lastFileInfo:    info,
		stop:            make(chan struct{}),
		done:            make(chan struct{}),
	}
	a.snapshot.Store(snapshot)
	go a.watch()
	return a, nil
}

func errorsNewFileAuth(reason string) error {
	return fmt.Errorf("file auth: %s", reason)
}

func loadFileAuthSnapshot(path string) (*fileAuthSnapshot, os.FileInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("file auth: read %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("file auth: stat %s: %w", path, err)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, info, fmt.Errorf("file auth: read %s: %w", path, err)
	}
	var users map[string]string
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&users); err != nil {
		return nil, info, fmt.Errorf("file auth: decode %s: %w", path, err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, info, fmt.Errorf("file auth: decode %s: %w", path, err)
	}
	if users == nil {
		return nil, info, errorsNewFileAuth("snapshot must be a JSON object")
	}
	for id, password := range users {
		parsedID, err := strconv.ParseUint(id, 10, 64)
		if err != nil || parsedID == 0 || strconv.FormatUint(parsedID, 10) != id {
			return nil, info, fmt.Errorf("file auth: invalid user id %q", id)
		}
		if password == "" {
			return nil, info, fmt.Errorf("file auth: empty password for user %q", id)
		}
	}
	return &fileAuthSnapshot{users: users}, info, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errorsNewFileAuth("snapshot has trailing JSON data")
		}
		return err
	}
	return nil
}

func sameFileVersion(previous, current os.FileInfo) bool {
	return previous != nil && current != nil && os.SameFile(previous, current) &&
		previous.Size() == current.Size() && previous.ModTime() == current.ModTime()
}

func (a *FileAuthenticator) watch() {
	ticker := time.NewTicker(a.refreshInterval)
	defer ticker.Stop()
	defer close(a.done)
	for {
		select {
		case <-ticker.C:
			a.reloadIfChanged()
		case <-a.stop:
			return
		}
	}
}

func (a *FileAuthenticator) reloadIfChanged() {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()

	info, err := os.Stat(a.path)
	if err != nil {
		a.logReloadFailure(err)
		return
	}
	if sameFileVersion(a.lastFileInfo, info) {
		return
	}
	if _, err := a.reloadLocked(); err != nil {
		a.logReloadFailure(err)
	}
}

func (a *FileAuthenticator) reloadLocked() (int, error) {
	snapshot, loadedInfo, err := loadFileAuthSnapshot(a.path)
	if err != nil {
		return 0, err
	}
	a.lastFileInfo = loadedInfo
	a.lastReloadError = ""
	a.snapshot.Store(snapshot)
	log.Printf("file auth: reloaded %d users from %s", len(snapshot.users), a.path)
	return len(snapshot.users), nil
}

// Reload replaces the active snapshot only after the whole file has been
// validated. Authentication remains lock-free while a reload is in progress.
func (a *FileAuthenticator) Reload() (int, error) {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()

	count, err := a.reloadLocked()
	if err != nil {
		a.logReloadFailure(err)
		return 0, err
	}
	return count, nil
}

func (a *FileAuthenticator) logReloadFailure(err error) {
	message := err.Error()
	if message == a.lastReloadError {
		return
	}
	a.lastReloadError = message
	log.Printf("file auth: keeping last good snapshot after reload failure: %v", err)
}

func (a *FileAuthenticator) Authenticate(_ net.Addr, credential string, _ uint64) (ok bool, id string) {
	id, password, found := strings.Cut(credential, "_")
	if !found || id == "" || password == "" {
		return false, ""
	}
	parsedID, err := strconv.ParseUint(id, 10, 64)
	if err != nil || parsedID == 0 {
		return false, ""
	}
	snapshot := a.snapshot.Load()
	if snapshot == nil {
		return false, ""
	}
	expected, found := snapshot.users[id]
	if !found || len(expected) != len(password) {
		return false, ""
	}
	if subtle.ConstantTimeCompare([]byte(expected), []byte(password)) != 1 {
		return false, ""
	}
	return true, id
}

func (a *FileAuthenticator) Close() {
	a.closeOnce.Do(func() {
		close(a.stop)
		<-a.done
	})
}
