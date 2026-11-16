package bridge

import (
	"context"
	"net"
	"sync"
)

// DialUnix supports session socket paths longer than sockaddr_un on Linux.
func DialUnix(ctx context.Context, path string) (net.Conn, error) {
	address, release, err := unixAddress(path)
	if err != nil {
		return nil, err
	}
	defer release()
	return (&net.Dialer{}).DialContext(ctx, "unix", address)
}

func listenUnix(path string) (net.Listener, error) {
	address, release, err := unixAddress(path)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", address)
	if err != nil {
		release()
		return nil, err
	}
	// Keep the directory handle alive until net.UnixListener has unlinked the
	// socket. Closing it earlier could leave the socket behind or reuse its FD.
	return &unixListener{Listener: listener, release: release}, nil
}

type unixListener struct {
	net.Listener
	release func()
	once    sync.Once
	err     error
}

func (l *unixListener) Close() error {
	l.once.Do(func() {
		l.err = l.Listener.Close()
		l.release()
	})
	return l.err
}
