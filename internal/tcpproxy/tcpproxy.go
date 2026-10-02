// Package tcpproxy relays TCP connections between a client and an upstream,
// optionally letting a Tap observe the bytes flowing in each direction.
package tcpproxy

import (
	"io"
	"log/slog"
	"net"
	"time"
)

// Tap observes the bytes of one proxied connection. The writers are fed
// synchronously before bytes are forwarded, so implementations must be fast,
// must not block, and must never return an error (that would abort the copy).
type Tap interface {
	ClientToServer() io.Writer
	ServerToClient() io.Writer
	Close()
}

// TapFactory returns a fresh Tap for each accepted connection.
type TapFactory func() Tap

// Pipe copies bytes between client and upstream in both directions until
// either side closes. The caller owns both connections and closes them.
func Pipe(client, upstream net.Conn, tap Tap) {
	var c2s, s2c io.Reader = client, upstream
	if tap != nil {
		defer tap.Close()
		c2s = io.TeeReader(client, tap.ClientToServer())
		s2c = io.TeeReader(upstream, tap.ServerToClient())
	}

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, c2s); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, s2c); done <- struct{}{} }()
	<-done
}

// Relay listens on an ephemeral 127.0.0.1 port and forwards every accepted
// connection to a fixed upstream address.
type Relay struct {
	listener net.Listener
	upstream string
	tap      TapFactory
	logger   *slog.Logger
}

// Listen opens the local listener and starts accepting in the background.
// tap may be nil.
func Listen(upstream string, tap TapFactory, logger *slog.Logger) (*Relay, error) {
	if logger == nil {
		logger = slog.Default()
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	r := &Relay{listener: l, upstream: upstream, tap: tap, logger: logger}
	go r.acceptLoop()
	return r, nil
}

func (r *Relay) LocalPort() int { return r.listener.Addr().(*net.TCPAddr).Port }

func (r *Relay) Close() error { return r.listener.Close() }

func (r *Relay) acceptLoop() {
	for {
		client, err := r.listener.Accept()
		if err != nil {
			return // listener closed
		}
		go r.handleConn(client)
	}
}

func (r *Relay) handleConn(client net.Conn) {
	defer client.Close()

	upstream, err := net.DialTimeout("tcp", r.upstream, 10*time.Second)
	if err != nil {
		r.logger.Warn("dial upstream failed", "upstream", r.upstream, "err", err)
		return
	}
	defer upstream.Close()

	var tap Tap
	if r.tap != nil {
		tap = r.tap()
	}
	Pipe(client, upstream, tap)
}
