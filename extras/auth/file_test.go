package auth

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeAuthSnapshot(t *testing.T, path string, users map[string]string) {
	t.Helper()
	data := []byte("{")
	first := true
	for id, password := range users {
		if !first {
			data = append(data, ',')
		}
		first = false
		data = append(data, []byte(`"`+id+`":"`+password+`"`)...)
	}
	data = append(data, '}')
	tempPath := path + ".tmp"
	if err := os.WriteFile(tempPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		t.Fatal(err)
	}
}

func TestFileAuthenticatorLoadsAndReloadsCanonicalCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hy2_auth.json")
	writeAuthSnapshot(t, path, map[string]string{"42": "old-password"})
	authenticator, err := NewFileAuthenticator(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer authenticator.Close()

	if ok, id := authenticator.Authenticate(&net.UDPAddr{}, "42_old-password", 0); !ok || id != "42" {
		t.Fatalf("initial credential returned ok=%v id=%q", ok, id)
	}
	if ok, _ := authenticator.Authenticate(&net.UDPAddr{}, "42:old-password", 0); ok {
		t.Fatal("non-canonical credential was accepted")
	}

	writeAuthSnapshot(t, path, map[string]string{"42": "new-password", "43": "added-password"})
	if count, err := authenticator.Reload(); err != nil || count != 2 {
		t.Fatalf("Reload() returned count=%d error=%v", count, err)
	}
	if ok, _ := authenticator.Authenticate(&net.UDPAddr{}, "42_new-password", 0); !ok {
		t.Fatal("replacement credential was not accepted immediately")
	}
	if ok, _ := authenticator.Authenticate(&net.UDPAddr{}, "42_old-password", 0); ok {
		t.Fatal("replaced credential remained valid")
	}
	if ok, _ := authenticator.Authenticate(&net.UDPAddr{}, "43_added-password", 0); !ok {
		t.Fatal("added credential was not accepted")
	}
}

func TestFileAuthenticatorDoesNotTouchDiskDuringAuthentication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hy2_auth.json")
	writeAuthSnapshot(t, path, map[string]string{"42": "password"})
	authenticator, err := NewFileAuthenticator(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer authenticator.Close()

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if ok, id := authenticator.Authenticate(&net.UDPAddr{}, "42_password", 0); !ok || id != "42" {
		t.Fatalf("cached credential returned ok=%v id=%q", ok, id)
	}
	if _, err := authenticator.Reload(); err == nil {
		t.Fatal("reload of a missing snapshot succeeded")
	}
	if ok, id := authenticator.Authenticate(&net.UDPAddr{}, "42_password", 0); !ok || id != "42" {
		t.Fatalf("last good credential returned ok=%v id=%q", ok, id)
	}
}

func TestFileAuthenticatorWatcherReloadsAnAtomicReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hy2_auth.json")
	writeAuthSnapshot(t, path, map[string]string{"42": "old-password"})
	authenticator, err := NewFileAuthenticator(path, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer authenticator.Close()

	writeAuthSnapshot(t, path, map[string]string{"42": "new-password"})
	deadline := time.Now().Add(time.Second)
	for {
		ok, _ := authenticator.Authenticate(&net.UDPAddr{}, "42_new-password", 0)
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("watcher did not load the replacement snapshot")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ok, _ := authenticator.Authenticate(&net.UDPAddr{}, "42_old-password", 0); ok {
		t.Fatal("watcher kept the replaced credential valid")
	}
}

func TestFileAuthenticatorKeepsLastGoodSnapshotAfterInvalidUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hy2_auth.json")
	writeAuthSnapshot(t, path, map[string]string{"42": "password"})
	authenticator, err := NewFileAuthenticator(path, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer authenticator.Close()

	if err := os.WriteFile(path, []byte(`{"42":`), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	if ok, id := authenticator.Authenticate(&net.UDPAddr{}, "42_password", 0); !ok || id != "42" {
		t.Fatalf("last good credential returned ok=%v id=%q", ok, id)
	}
}

func TestFileAuthenticatorRejectsInvalidInitialStateAndAllowsExplicitEmptySnapshot(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewFileAuthenticator(filepath.Join(dir, "missing.json"), time.Second); err == nil {
		t.Fatal("missing initial auth file was accepted")
	}

	invalidPath := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(invalidPath, []byte(`{"0":"password"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileAuthenticator(invalidPath, time.Second); err == nil {
		t.Fatal("invalid user id was accepted")
	}
	if err := os.WriteFile(invalidPath, []byte(`{"042":"password"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileAuthenticator(invalidPath, time.Second); err == nil {
		t.Fatal("non-canonical user id was accepted")
	}

	emptyPath := filepath.Join(dir, "empty.json")
	writeAuthSnapshot(t, emptyPath, map[string]string{})
	authenticator, err := NewFileAuthenticator(emptyPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer authenticator.Close()
	if ok, _ := authenticator.Authenticate(&net.UDPAddr{}, "42_password", 0); ok {
		t.Fatal("explicit empty snapshot accepted a credential")
	}
}
