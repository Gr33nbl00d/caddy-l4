package layer4

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
	"net"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

func init() {
	caddy.RegisterModule(ListenerWrapper{})
}

// ListenerWrapper is a Caddy module that wraps App as a listener wrapper, it doesn't support udp.
type ListenerWrapper struct {
	// Routes express composable logic for handling byte streams.
	Routes RouteList `json:"routes,omitempty"`

	compiledRoute Handler

	logger *zap.Logger
	ctx    caddy.Context
}

// CaddyModule returns the Caddy module information.
func (ListenerWrapper) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "caddy.listeners.layer4",
		New: func() caddy.Module { return new(ListenerWrapper) },
	}
}

// Provision sets up the ListenerWrapper.
func (lw *ListenerWrapper) Provision(ctx caddy.Context) error {
	lw.ctx = ctx
	lw.logger = ctx.Logger()

	err := lw.Routes.Provision(ctx)
	if err != nil {
		return err
	}
	lw.compiledRoute = lw.Routes.Compile(listenerHandler{}, lw.logger)

	return nil
}

func (lw *ListenerWrapper) WrapListener(l net.Listener) net.Listener {
	// TODO make channel capacity configurable
	connChan := make(chan net.Conn, runtime.GOMAXPROCS(0))
	li := &listener{
		Listener:      l,
		logger:        lw.logger,
		compiledRoute: lw.compiledRoute,
		connChan:      connChan,
		wg:            new(sync.WaitGroup),
	}
	go li.loop()
	return li
}

type listener struct {
	net.Listener
	logger        *zap.Logger
	compiledRoute Handler

	// closed when there is a non-recoverable error and all handle goroutines are done
	connChan chan net.Conn
	err      error

	// count running handles
	wg *sync.WaitGroup
}

type tcpConnection interface {
	SetKeepAlivePeriod(d time.Duration) error
	SetKeepAlive(bool) error
}

// loop accept connection from underlying listener and pipe the connection if there are any
func (l *listener) loop() {
	for {
		conn, err := l.Listener.Accept()
		if nerr, ok := err.(net.Error); ok && nerr.Temporary() {
			l.logger.Error("temporary error accepting connection", zap.Error(err))
			continue
		}
		if err != nil {
			l.err = err
			break
		} else {
			if tconn, ok := conn.(tcpConnection); ok {
				err = setKeepAliveWorkarround(tconn)
				if err != nil {
					l.logger.Warn("unable to set keepalive for new connection:", zap.Error(err))
				}
			}
		}

		l.wg.Add(1)
		go l.handle(conn)
	}

	// closing remaining conns in channel to release resources
	go func() {
		l.wg.Wait()
		close(l.connChan)
	}()
	for conn := range l.connChan {
		conn.Close()
	}
}

// errHijacked is used when a handler takes over the connection, it's lifetime is not managed by handle
var errHijacked = errors.New("hijacked connection")

func (l *listener) handle(conn net.Conn) {
	var err error
	defer func() {
		l.wg.Done()
		if err != errHijacked {
			conn.Close()
		}
	}()

	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)

	cx := WrapConnection(conn, buf, l.logger)
	cx.Context = context.WithValue(cx.Context, listenerCtxKey, l)

	start := time.Now()
	err = l.compiledRoute.Handle(cx)
	duration := time.Since(start)
	if err != nil && err != errHijacked {
		l.logger.Error("handling connection", zap.Error(err))
	}

	l.logger.Debug("connection stats",
		zap.String("remote", cx.RemoteAddr().String()),
		zap.Uint64("read", cx.bytesRead),
		zap.Uint64("written", cx.bytesWritten),
		zap.Duration("duration", duration),
	)
}

func (l *listener) Accept() (net.Conn, error) {
	for conn := range l.connChan {
		return conn, nil
	}
	return nil, l.err

}

func (l *listener) pipeConnection(conn *Connection) error {
	// can't use l4tls.GetConnectionStates because of import cycle
	// TODO export tls_connection_states as a special constant
	var connectionStates []*tls.ConnectionState
	if val := conn.GetVar("tls_connection_states"); val != nil {
		connectionStates = val.([]*tls.ConnectionState)
	}
	if len(connectionStates) > 0 {
		l.connChan <- &tlsConnection{
			Conn:      conn,
			connState: connectionStates[len(connectionStates)-1],
		}
	} else {
		l.connChan <- conn
	}
	return errHijacked
}

// tlsConnection implements ConnectionState interface to use it with h2
type tlsConnection struct {
	net.Conn
	connState *tls.ConnectionState
}

func (tc *tlsConnection) ConnectionState() tls.ConnectionState {
	return *tc.connState
}

func setKeepAliveWorkarround(conn tcpConnection) error {
	rawConn, err := conn.(*net.TCPConn).SyscallConn()
	if err != nil {
		return err
	}

	keepaliveParams := syscall.TCPKeepalive{
		OnOff:    1,
		Time:     120000,
		Interval: 15000,
	}
	ret := uint32(0)
	err = rawConn.Control(func(fd uintptr) {
		err := syscall.WSAIoctl(
			syscall.Handle(fd),
			syscall.SIO_KEEPALIVE_VALS,
			(*byte)(unsafe.Pointer(&keepaliveParams)),
			uint32(unsafe.Sizeof(keepaliveParams)),
			nil,
			0,
			&ret,
			nil,
			0,
		)
		if err != nil {
			fmt.Println("WSAIoctl error:", err)
		}
	})
	return err
}

// Interface guards
var (
	_ caddy.Module          = (*ListenerWrapper)(nil)
	_ caddy.ListenerWrapper = (*ListenerWrapper)(nil)
)
