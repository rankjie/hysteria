package server

import (
	"errors"
	"io"
	"time"
)

var errDisconnect = errors.New("traffic logger requested disconnect")

func copyBufferLog(dst io.Writer, src io.Reader, log func(n uint64) bool) error {
	buf := make([]byte, 32*1024)
	for {
		nr, er := src.Read(buf)
		if nr > 0 {
			if !log(uint64(nr)) {
				// Log returns false, which means that the client should be disconnected
				return errDisconnect
			}
			_, ew := dst.Write(buf[0:nr])
			if ew != nil {
				return ew
			}
		}
		if er != nil {
			if er == io.EOF {
				// EOF should not be considered as an error
				return nil
			}
			return er
		}
	}
}

func copyTwoWayEx(id string, serverRw, remoteRw io.ReadWriter, l TrafficLogger, stats *StreamStats) error {
	errChan := make(chan error, 2)
	go func() {
		errChan <- copyBufferLog(serverRw, remoteRw, func(n uint64) bool {
			stats.LastActiveTime.Store(time.Now())
			stats.Rx.Add(n)
			return l.LogTraffic(id, 0, n)
		})
	}()
	go func() {
		errChan <- copyBufferLog(remoteRw, serverRw, func(n uint64) bool {
			stats.LastActiveTime.Store(time.Now())
			stats.Tx.Add(n)
			return l.LogTraffic(id, n, 0)
		})
	}()
	// Block until one of the two goroutines returns
	return <-errChan
}

// copyTwoWay is the "fast-path" version of copyTwoWayEx that does not log traffic but still updates stream stats.
// It uses the built-in io.Copy instead of our own copyBufferLog.
func copyTwoWay(serverRw, remoteRw io.ReadWriter, stats *StreamStats) error {
	errChan := make(chan error, 2)
	go func() {
		n, err := io.Copy(serverRw, remoteRw)
		if stats != nil && n > 0 {
			stats.LastActiveTime.Store(time.Now())
			stats.Rx.Add(uint64(n))
		}
		errChan <- err
	}()
	go func() {
		n, err := io.Copy(remoteRw, serverRw)
		if stats != nil && n > 0 {
			stats.LastActiveTime.Store(time.Now())
			stats.Tx.Add(uint64(n))
		}
		errChan <- err
	}()
	// Block until one of the two goroutines returns
	return <-errChan
}
