package forwarding

import (
	"io"
	"net"

	"github.com/apernet/hysteria/core/v2/client"
)

type TCPTunnel struct {
	HyClient    client.Client
	Remote      string
	EventLogger TCPEventLogger
}

type TCPEventLogger interface {
	Connect(addr net.Addr)
	Error(addr net.Addr, err error, upload, download uint64)
}

func (t *TCPTunnel) Serve(listener net.Listener) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go t.handle(conn)
	}
}

func (t *TCPTunnel) handle(conn net.Conn) {
	defer conn.Close()

	if t.EventLogger != nil {
		t.EventLogger.Connect(conn.RemoteAddr())
	}
	var closeErr error
	var upload, download uint64
	defer func() {
		if t.EventLogger != nil {
			t.EventLogger.Error(conn.RemoteAddr(), closeErr, upload, download)
		}
	}()

	rc, err := t.HyClient.TCP(t.Remote)
	if err != nil {
		closeErr = err
		return
	}
	defer rc.Close()

	// Start forwarding
	type copyResult struct {
		n   uint64
		err error
	}
	copyErrChan := make(chan copyResult, 2)
	go func() {
		n, copyErr := io.Copy(rc, conn)
		copyErrChan <- copyResult{uint64(n), copyErr}
	}()
	go func() {
		n, copyErr := io.Copy(conn, rc)
		copyErrChan <- copyResult{uint64(n), copyErr}
	}()
	r1 := <-copyErrChan
	r2 := <-copyErrChan
	upload = r1.n
	download = r2.n
	if r1.err != nil {
		closeErr = r1.err
	} else {
		closeErr = r2.err
	}
}
