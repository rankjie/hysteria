package trafficlogger

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/apernet/hysteria/core/v2/client"
	coreErrors "github.com/apernet/hysteria/core/v2/errors"
	"github.com/apernet/hysteria/core/v2/server"
)

type fixedIDAuthenticator string

func (a fixedIDAuthenticator) Authenticate(net.Addr, string, uint64) (bool, string) {
	return true, string(a)
}

func testServerCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certificate, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{certificate}, PrivateKey: privateKey}
}

func currentOnlineCount(t *testing.T, stats TrafficStatsServer, authID string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/online", nil)
	recorder := httptest.NewRecorder()
	stats.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("online status = %d, want %d", recorder.Code, http.StatusOK)
	}
	var online map[string]int
	if err := json.NewDecoder(recorder.Body).Decode(&online); err != nil {
		t.Fatal(err)
	}
	return online[authID]
}

func waitForOnlineCount(t *testing.T, stats TrafficStatsServer, authID string, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if got := currentOnlineCount(t, stats, authID); got == want {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("online count for %q = %d, want %d", authID, got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForClientClosed(t *testing.T, c client.Client) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		udp, err := c.UDP()
		if err == nil {
			_ = udp.Close()
		} else {
			var closedError coreErrors.ClosedError
			if errors.As(err, &closedError) {
				return
			}
			t.Fatalf("checking client connection: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("client connection remained open after kick")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestKickClosesAllRealConnectionsForAuthID(t *testing.T) {
	const authID = "shared-user"
	packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stats := NewTrafficStatsServer("")
	hysteriaServer, err := server.NewServer(&server.Config{
		TLSConfig: server.TLSConfig{
			Certificates: []tls.Certificate{testServerCertificate(t)},
		},
		Conn:          packetConn,
		Authenticator: fixedIDAuthenticator(authID),
		TrafficLogger: stats,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hysteriaServer.Close() })
	go func() { _ = hysteriaServer.Serve() }()

	clients := make([]client.Client, 2)
	for i := range clients {
		clients[i], _, err = client.NewClient(&client.Config{
			ServerAddr: packetConn.LocalAddr(),
			Auth:       "same-credential",
			TLSConfig:  client.TLSConfig{InsecureSkipVerify: true},
			QUICConfig: client.QUICConfig{DisableChromeParrot: true},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer clients[i].Close()
	}
	waitForOnlineCount(t, stats, authID, len(clients))

	if status := kickAuthIDs(stats, `["shared-user"]`); status != http.StatusOK {
		t.Fatalf("kick status = %d, want %d", status, http.StatusOK)
	}
	for _, hysteriaClient := range clients {
		waitForClientClosed(t, hysteriaClient)
	}
	waitForOnlineCount(t, stats, authID, 0)
}
