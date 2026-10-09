package nfsgw

import (
	"net"
	"sync"
)

// trackedListener remembers the connections it accepted so Serve can close them when
// the gateway stops (I-100).
type trackedListener struct {
	net.Listener
	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

func (t *trackedListener) Accept() (net.Conn, error) {
	c, err := t.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tc := &trackedConn{Conn: c, l: t}
	t.mu.Lock()
	t.conns[tc] = struct{}{}
	t.mu.Unlock()
	return tc, nil
}

func (t *trackedListener) forget(c net.Conn) {
	t.mu.Lock()
	delete(t.conns, c)
	t.mu.Unlock()
}

// closeConns closes every connection still open.
func (t *trackedListener) closeConns() {
	t.mu.Lock()
	open := make([]net.Conn, 0, len(t.conns))
	for c := range t.conns {
		open = append(open, c)
	}
	t.mu.Unlock()
	for _, c := range open {
		_ = c.Close()
	}
}

type trackedConn struct {
	net.Conn
	l    *trackedListener
	once sync.Once
}

func (c *trackedConn) Close() error {
	c.once.Do(func() { c.l.forget(c) })
	return c.Conn.Close()
}
