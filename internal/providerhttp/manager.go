// Package providerhttp owns a provider's shared HTTP transport lifetime, separate
// from request contexts, retry budgets, credentials and protocol handling.
package providerhttp

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"syscall"
)

// ErrClosed means the provider's connection owner has permanently shut down.
var ErrClosed = errors.New("provider HTTP connections are closed")

// ErrResetUnsupported means the supplied transport cannot retire its connections
// without changing its dialing or pooling policy. Ordinary requests still work.
var ErrResetUnsupported = errors.New("provider HTTP transport does not support connection reset")

// Manager owns admission, active exchanges and transport generations for one
// configured provider. Share its client among all provider users, but give each
// operation its own context and retry budget. It starts no goroutines.
//
// Standard transports are cloned; the caller's transport and pools remain owned
// by the caller. Custom RoundTrippers retain their own socket ownership: they
// must honor request cancellation, and Close calls CloseIdleConnections if
// available. Reset is supported for them only when that method is available.
// Standard transports with custom TLS dialers do not support Reset, because
// wrapping their returned connections would change net/http's TLS/ALPN behavior.
// Their Close cancels exchanges and closes idle connections, but socket teardown
// remains subject to the custom dialer's cancellation and cleanup behavior.
// All methods are safe for concurrent use. A Manager must be constructed by New.
type Manager struct {
	lifecycle sync.Mutex // Serialize teardown without holding the admission lock.
	mu        sync.Mutex
	base      http.RoundTripper
	template  *http.Transport // Immutable policy, without our raw-dial wrapper.
	sockets   *sockets
	active    map[*exchange]struct{}
	canReset  bool
	closed    bool
}

// New copies the client's timeout, redirects, cookie jar and transport policy.
// A nil client selects http.Client defaults. The returned client uses the manager
// as its transport; CloseIdleConnections does not permanently close the manager.
func New(client *http.Client) (*http.Client, *Manager) {
	var copied http.Client
	if client != nil {
		copied = *client
	}
	base := copied.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	m := &Manager{base: base, active: make(map[*exchange]struct{})}
	if standard, ok := base.(*http.Transport); ok {
		// Keep the original ALPN policy when installing a custom raw dial hook.
		autoHTTP2 := standard.Protocols == nil && standard.TLSNextProto == nil &&
			standard.TLSClientConfig == nil && standard.DialContext == nil && standard.Dial == nil
		cloned := standard.Clone()
		if standard.DialTLS == nil && standard.DialTLSContext == nil {
			m.template = cloned
			if autoHTTP2 {
				m.template.ForceAttemptHTTP2 = true
			}
			m.base, m.sockets = generation(m.template)
			m.canReset = true
		} else {
			m.base = cloned
		}
	} else {
		_, m.canReset = base.(interface{ CloseIdleConnections() })
	}
	copied.Transport = m
	return &copied, m
}

// Reset abandons current exchanges with ECONNABORTED, a retryable transport
// failure, without canceling their parent contexts. Future requests use a fresh
// standard pool. It does not probe connectivity, retry requests or wait for their
// consumers. Unsupported transports return ErrResetUnsupported without disruption;
// a closed manager returns ErrClosed. Raw socket retirement includes late dials
// and HTTP/2 connections whose stream cleanup is still asynchronous.
func (m *Manager) Reset() error {
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	if !m.canReset {
		m.mu.Unlock()
		return ErrResetUnsupported
	}
	old, sockets := m.base, m.sockets
	if m.template != nil {
		m.base, m.sockets = generation(m.template)
	}
	m.abort(syscall.ECONNABORTED)
	m.mu.Unlock()
	if sockets != nil {
		sockets.retire()
	}
	closeIdleConnections(old)
	return nil
}

// Close permanently rejects future requests and resets with ErrClosed, cancels
// active exchanges without canceling parent contexts, and closes owned active
// and idle sockets. It is idempotent. Custom transport cleanup has the limitations
// described on Manager. Close does not wait for request consumers or retry loops.
func (m *Manager) Close() error {
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.abort(ErrClosed)
	m.mu.Unlock()
	if m.sockets != nil {
		m.sockets.retire()
	}
	closeIdleConnections(m.base)
	return nil
}

// abort runs under m.mu. Retired exchanges no longer need manager bookkeeping;
// their bodies retain their cancellation cause until their consumers close them.
func (m *Manager) abort(cause error) {
	for x := range m.active {
		x.cancel(cause)
		delete(m.active, x)
	}
}

func closeIdleConnections(transport http.RoundTripper) {
	if closer, ok := transport.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

// CloseIdleConnections closes currently idle pooled connections, without
// canceling active requests or preventing future requests.
func (m *Manager) CloseIdleConnections() {
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	m.mu.Lock()
	base := m.base
	m.mu.Unlock()
	closeIdleConnections(base)
}

// RoundTrip admits one exchange until its response body is closed. Reset and
// Close are linearized against admission; they never alter the request itself.
func (m *Manager) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancelCause(req.Context())
	x := &exchange{owner: m, parent: req.Context(), ctx: ctx, cancel: cancel}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel(ErrClosed)
		return nil, ErrClosed
	}
	m.active[x] = struct{}{}
	base := m.base
	m.mu.Unlock()
	resp, err := base.RoundTrip(req.WithContext(ctx))
	if err = x.failure(err); err != nil {
		x.release()
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		return nil, err
	}
	if resp == nil || resp.Body == nil {
		x.release()
	} else {
		resp.Body = &body{ReadCloser: resp.Body, exchange: x}
	}
	return resp, nil
}

// generation wraps raw dialing below TLS, so net/http receives its own *tls.Conn
// and preserves ALPN. Always clone the template, never a retired generation.
func generation(template *http.Transport) (*http.Transport, *sockets) {
	transport := template.Clone()
	s := &sockets{connections: make(map[*connection]struct{})}
	dial := transport.DialContext
	if dial == nil {
		if legacy := transport.Dial; legacy != nil {
			dial = func(_ context.Context, network, addr string) (net.Conn, error) { return legacy(network, addr) }
		} else {
			dial = (&net.Dialer{}).DialContext
		}
	}
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		tracked := &connection{Conn: conn, sockets: s}
		s.mu.Lock()
		if s.retired {
			s.mu.Unlock()
			conn.Close()
			return nil, syscall.ECONNABORTED
		}
		s.connections[tracked] = struct{}{}
		s.mu.Unlock()
		return tracked, nil
	}
	return transport, s
}

type sockets struct {
	mu          sync.Mutex
	connections map[*connection]struct{} // Only open raw sockets; Close removes entries.
	retired     bool
}

func (s *sockets) retire() {
	s.mu.Lock()
	s.retired = true
	connections := make([]*connection, 0, len(s.connections))
	for conn := range s.connections {
		connections = append(connections, conn)
	}
	s.mu.Unlock()
	for _, conn := range connections {
		// Raw sockets only: tls.Conn.Close can block sending close_notify.
		conn.Close()
	}
}

type connection struct {
	net.Conn
	sockets  *sockets
	once     sync.Once
	closeErr error
}

func (c *connection) Close() error {
	c.once.Do(func() {
		c.sockets.mu.Lock()
		delete(c.sockets.connections, c)
		c.sockets.mu.Unlock()
		c.closeErr = c.Conn.Close()
	})
	return c.closeErr
}

type exchange struct {
	owner  *Manager
	parent context.Context
	ctx    context.Context
	cancel context.CancelCauseFunc
	once   sync.Once
}

func (x *exchange) failure(err error) error {
	if parentErr := x.parent.Err(); parentErr != nil {
		return parentErr
	}
	cause := context.Cause(x.ctx)
	if cause == syscall.ECONNABORTED || cause == ErrClosed {
		// A cancellation error produced by our child context is not a caller
		// cancellation. Retain other underlying I/O failures for errors.Is/As.
		if err == nil || errors.Is(err, context.Canceled) {
			return cause
		}
		return errors.Join(cause, err)
	}
	return err
}

func (x *exchange) release() {
	x.once.Do(func() {
		x.owner.mu.Lock()
		delete(x.owner.active, x)
		x.owner.mu.Unlock()
		x.cancel(nil)
	})
}

type body struct {
	io.ReadCloser
	exchange *exchange
}

func (b *body) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	return n, b.exchange.failure(err)
}

func (b *body) Close() error {
	err := b.ReadCloser.Close()
	b.exchange.release()
	return err
}
