package tproxy

import (
	"io"
	"net"

	"github.com/apernet/go-tproxy"
	"github.com/apernet/hysteria/core/v2/client"
)

type TCPTProxy struct {
	HyClient    client.Client
	EventLogger TCPEventLogger
}

type TCPEventLogger interface {
	Connect(addr, reqAddr net.Addr)
	Error(addr, reqAddr net.Addr, err error, upload, download uint64)
}

func (r *TCPTProxy) ListenAndServe(laddr *net.TCPAddr) error {
	listener, err := tproxy.ListenTCP("tcp", laddr)
	if err != nil {
		return err
	}
	defer listener.Close()
	for {
		c, err := listener.Accept()
		if err != nil {
			return err
		}
		go r.handle(c)
	}
}

func (r *TCPTProxy) handle(conn net.Conn) {
	defer conn.Close()
	// In TProxy mode, we are masquerading as the remote server.
	// So LocalAddr is actually the target the user is trying to connect to,
	// and RemoteAddr is the local address.
	if r.EventLogger != nil {
		r.EventLogger.Connect(conn.RemoteAddr(), conn.LocalAddr())
	}
	var closeErr error
	var upload, download uint64
	defer func() {
		if r.EventLogger != nil {
			r.EventLogger.Error(conn.RemoteAddr(), conn.LocalAddr(), closeErr, upload, download)
		}
	}()

	rc, err := r.HyClient.TCP(conn.LocalAddr().String())
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
