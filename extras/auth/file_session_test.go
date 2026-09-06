package auth

import (
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFileSessionsRevokeOnlyChangedCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	writeAuthSnapshot(t, path, map[string]string{"42": "old", "43": "stable"})
	a, err := NewFileAuthenticator(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	var revoked, stable atomic.Int32
	register := func(credential string, count *atomic.Int32) func() {
		t.Helper()
		ok, _, unregister := a.AuthenticateAndRegister(&net.UDPAddr{}, credential, 0, func() { count.Add(1) })
		if !ok || unregister == nil {
			t.Fatal("registration rejected")
		}
		return unregister
	}
	first := register("42_old", &revoked)
	second := register("42_old", &revoked)
	register("43_stable", &stable)
	if _, err := a.Reload(); err != nil {
		t.Fatal(err)
	}
	if revoked.Load() != 0 || stable.Load() != 0 {
		t.Fatal("identical reload revoked sessions")
	}
	if err := os.WriteFile(path, []byte(`{"42":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Reload(); err == nil {
		t.Fatal("invalid snapshot accepted")
	}
	if revoked.Load() != 0 || stable.Load() != 0 {
		t.Fatal("failed reload revoked sessions")
	}
	writeAuthSnapshot(t, path, map[string]string{"42": "new", "43": "stable"})
	if _, err := a.Reload(); err != nil {
		t.Fatal(err)
	}
	if revoked.Load() != 2 || stable.Load() != 0 {
		t.Fatalf("revoked=%d stable=%d", revoked.Load(), stable.Load())
	}
	if ok, _, cleanup := a.AuthenticateAndRegister(nil, "42_old", 0, func() { t.Error("rejected session revoked") }); ok || cleanup != nil {
		t.Fatal("old credential registered")
	}
	newCleanup := register("42_new", &revoked)
	first()
	second()
	first() // Old disconnects must not unregister the new generation.
	writeAuthSnapshot(t, path, map[string]string{})
	if _, err := a.Reload(); err != nil {
		t.Fatal(err)
	}
	if revoked.Load() != 3 || stable.Load() != 1 {
		t.Fatalf("removal revoked=%d stable=%d", revoked.Load(), stable.Load())
	}
	newCleanup()
	if len(a.sessions) != 0 {
		t.Fatal("disconnected session retained")
	}
}

func TestFileSessionRegistrationRacesReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	writeAuthSnapshot(t, path, map[string]string{"42": "old"})
	a, err := NewFileAuthenticator(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for i := 0; i < 50; i++ {
		writeAuthSnapshot(t, path, map[string]string{"42": "old"})
		if _, err := a.Reload(); err != nil {
			t.Fatal(err)
		}
		var accepted bool
		var revokeCount atomic.Int32
		var unregister func()
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			accepted, _, unregister = a.AuthenticateAndRegister(nil, "42_old", 0, func() { revokeCount.Add(1) })
		}()
		writeAuthSnapshot(t, path, map[string]string{"42": "new"})
		if _, err := a.Reload(); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		if accepted && revokeCount.Load() != 1 {
			t.Fatal("accepted old credential missed revocation")
		}
		if unregister != nil {
			unregister()
			unregister()
		}
	}
	// Revocation may synchronously unregister without deadlocking the reload.
	var unregister func()
	_, _, unregister = a.AuthenticateAndRegister(nil, "42_new", 0, func() { unregister() })
	writeAuthSnapshot(t, path, map[string]string{})
	if _, err := a.Reload(); err != nil {
		t.Fatal(err)
	}
}
