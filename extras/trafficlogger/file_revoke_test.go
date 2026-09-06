package trafficlogger

import (
	"crypto/tls"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/apernet/hysteria/core/v2/client"
	"github.com/apernet/hysteria/core/v2/server"
	"github.com/apernet/hysteria/extras/v2/auth"
)

type pausedFileAuthenticator struct {
	auth             *auth.FileAuthenticator
	entered, release chan struct{}
	once             sync.Once
}

func (a *pausedFileAuthenticator) Authenticate(addr net.Addr, credential string, tx uint64) (bool, string) {
	ok, id := a.auth.Authenticate(addr, credential, tx)
	if ok {
		a.once.Do(func() { close(a.entered); <-a.release })
	}
	return ok, id
}

func (a *pausedFileAuthenticator) AuthenticateAndRegister(addr net.Addr, credential string, tx uint64, revoke func()) (bool, string, func()) {
	ok, id, unregister := a.auth.AuthenticateAndRegister(addr, credential, tx, revoke)
	if ok {
		a.once.Do(func() { close(a.entered); <-a.release })
	}
	return ok, id, unregister
}

func TestFileReloadRevokesAuthenticationBeforeTrafficRegistration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(`{"42":"old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := auth.NewFileAuthenticator(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	paused := &pausedFileAuthenticator{auth: file, entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(paused.release) })
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stats := NewTrafficStatsServer("")
	srv, err := server.NewServer(&server.Config{TLSConfig: server.TLSConfig{Certificates: []tls.Certificate{testServerCertificate(t)}}, Conn: pc, Authenticator: paused, TrafficLogger: stats})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go srv.Serve()
	type result struct {
		client client.Client
		err    error
	}
	done := make(chan result, 1)
	go func() {
		c, _, err := client.NewClient(&client.Config{ServerAddr: pc.LocalAddr(), Auth: "42_old", TLSConfig: client.TLSConfig{InsecureSkipVerify: true}, QUICConfig: client.QUICConfig{DisableChromeParrot: true}})
		done <- result{c, err}
	}()
	select {
	case <-paused.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("auth not reached")
	}
	if err := os.WriteFile(path, []byte(`{"42":"new"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = file.Reload(); err != nil {
		t.Fatal(err)
	}
	if status := kickAuthIDs(stats, `["42"]`); status != 200 {
		t.Fatal(status)
	}
	releaseOnce.Do(func() { close(paused.release) })
	var connected result
	select {
	case connected = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("client not completed")
	}
	if connected.err != nil {
		return
	}
	defer connected.client.Close()
	waitForClientClosed(t, connected.client)
	return
}

func TestFileReloadClosesEstablishedConnectionsWithoutTrafficLogger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"42":"old","43":"stable"}`)
	file, err := auth.NewFileAuthenticator(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.NewServer(&server.Config{TLSConfig: server.TLSConfig{Certificates: []tls.Certificate{testServerCertificate(t)}}, Conn: pc, Authenticator: file})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go srv.Serve()
	connect := func(credential string) client.Client {
		t.Helper()
		c, _, err := client.NewClient(&client.Config{ServerAddr: pc.LocalAddr(), Auth: credential, TLSConfig: client.TLSConfig{InsecureSkipVerify: true}, QUICConfig: client.QUICConfig{DisableChromeParrot: true}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	old := []client.Client{connect("42_old"), connect("42_old")}
	stable := connect("43_stable")
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		for {
			conn, err := target.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); io.Copy(conn, conn) }()
		}
	}()
	stream := func(c client.Client) net.Conn {
		t.Helper()
		conn, err := c.TCP(target.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	echo := func(conn net.Conn) {
		t.Helper()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := conn.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		var response [5]byte
		if _, err := io.ReadFull(conn, response[:]); err != nil {
			t.Fatal(err)
		}
		if string(response[:]) != "hello" {
			t.Fatal("echo mismatch")
		}
	}
	oldStream := stream(old[0])
	stableStream := stream(stable)
	echo(oldStream)
	echo(stableStream)
	if _, err := file.Reload(); err != nil {
		t.Fatal(err)
	}
	echo(oldStream)
	write(`{"42":"new","43":"stable"}`)
	if _, err := file.Reload(); err != nil {
		t.Fatal(err)
	}
	for _, c := range old {
		waitForClientClosed(t, c)
	}
	oldStream.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	if _, err := oldStream.Read(b[:]); err == nil {
		t.Fatal("revoked TCP stream stayed open")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("revoked stream only timed out")
	}
	echo(stableStream)
	replacement := connect("42_new")
	echo(stream(replacement))
	if c, _, err := client.NewClient(&client.Config{ServerAddr: pc.LocalAddr(), Auth: "42_old", TLSConfig: client.TLSConfig{InsecureSkipVerify: true}, QUICConfig: client.QUICConfig{DisableChromeParrot: true}}); err == nil {
		c.Close()
		t.Fatal("old credentials reconnected")
	}
	write(`{}`)
	if _, err := file.Reload(); err != nil {
		t.Fatal(err)
	}
	waitForClientClosed(t, stable)
	waitForClientClosed(t, replacement)
}
