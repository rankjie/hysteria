package trafficlogger

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apernet/hysteria/core/v2/server"
)

func newConnectionTrackingStats(t *testing.T) (TrafficStatsServer, server.TrafficLoggerConnectionTracker) {
	t.Helper()
	stats := NewTrafficStatsServer("")
	tracker, ok := stats.(server.TrafficLoggerConnectionTracker)
	if !ok {
		t.Fatal("built-in traffic stats server does not support connection tracking")
	}
	return stats, tracker
}

func kickAuthIDs(stats TrafficStatsServer, body string) int {
	req := httptest.NewRequest(http.MethodPost, "/kick", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	stats.ServeHTTP(recorder, req)
	return recorder.Code
}

func TestKickDisconnectsAllConcurrentConnectionsForAuthID(t *testing.T) {
	const authID = "shared-user"
	stats, tracker := newConnectionTrackingStats(t)
	var sharedDisconnects atomic.Int32
	var otherDisconnects atomic.Int32
	untrackFirst := tracker.TrackConnection(authID, func() { sharedDisconnects.Add(1) })
	untrackSecond := tracker.TrackConnection(authID, func() { sharedDisconnects.Add(1) })
	untrackOther := tracker.TrackConnection("other-user", func() { otherDisconnects.Add(1) })
	defer untrackFirst()
	defer untrackSecond()
	defer untrackOther()

	if status := kickAuthIDs(stats, `["shared-user"]`); status != http.StatusOK {
		t.Fatalf("kick status = %d, want %d", status, http.StatusOK)
	}
	if got := sharedDisconnects.Load(); got != 2 {
		t.Fatalf("kick disconnected %d of 2 active connections", got)
	}
	if got := otherDisconnects.Load(); got != 0 {
		t.Fatalf("kick disconnected %d connections for a different auth ID", got)
	}

	if status := kickAuthIDs(stats, `["shared-user"]`); status != http.StatusOK {
		t.Fatalf("repeated kick status = %d, want %d", status, http.StatusOK)
	}
	if got := sharedDisconnects.Load(); got != 2 {
		t.Fatalf("repeated kick called disconnect callbacks %d times, want 2", got)
	}
}

func TestUntrackedConnectionIsNotKicked(t *testing.T) {
	stats, tracker := newConnectionTrackingStats(t)
	var disconnects atomic.Int32
	untrack := tracker.TrackConnection("former-user", func() { disconnects.Add(1) })
	untrack()
	untrack()

	if status := kickAuthIDs(stats, `["former-user"]`); status != http.StatusOK {
		t.Fatalf("kick status = %d, want %d", status, http.StatusOK)
	}
	if got := disconnects.Load(); got != 0 {
		t.Fatalf("kick disconnected an untracked connection %d times", got)
	}
}

func TestKickRunsDisconnectCallbacksOutsideTrackerLock(t *testing.T) {
	const authID = "shared-user"
	stats, tracker := newConnectionTrackingStats(t)
	disconnectStarted := make(chan struct{})
	releaseDisconnect := make(chan struct{})
	tracker.TrackConnection(authID, func() {
		close(disconnectStarted)
		<-releaseDisconnect
	})

	kickDone := make(chan int, 1)
	go func() {
		kickDone <- kickAuthIDs(stats, `["shared-user"]`)
	}()
	select {
	case <-disconnectStarted:
	case <-time.After(time.Second):
		t.Fatal("kick did not invoke the existing connection callback")
	}

	var replacementDisconnects atomic.Int32
	replacementTracked := make(chan struct{})
	go func() {
		tracker.TrackConnection(authID, func() { replacementDisconnects.Add(1) })
		close(replacementTracked)
	}()
	select {
	case <-replacementTracked:
	case <-time.After(time.Second):
		close(releaseDisconnect)
		<-kickDone
		t.Fatal("tracking a new connection blocked behind a disconnect callback")
	}
	close(releaseDisconnect)

	if status := <-kickDone; status != http.StatusOK {
		t.Fatalf("kick status = %d, want %d", status, http.StatusOK)
	}
	if got := replacementDisconnects.Load(); got != 0 {
		t.Fatalf("kick disconnected %d connections registered after its snapshot", got)
	}
	if status := kickAuthIDs(stats, `["shared-user"]`); status != http.StatusOK {
		t.Fatalf("second kick status = %d, want %d", status, http.StatusOK)
	}
	if got := replacementDisconnects.Load(); got != 1 {
		t.Fatalf("second kick disconnected replacement connection %d times, want 1", got)
	}
}
