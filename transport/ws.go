package transport

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"

	"github.com/ghettovoice/gosip/log"
	"github.com/ghettovoice/gosip/sip"
)

var (
	wsSubProtocol = "sip"
)

type wsConn struct {
	net.Conn
	client bool
}

func (wc *wsConn) Read(b []byte) (n int, err error) {
	var msg []byte
	var op ws.OpCode
	if wc.client {
		msg, op, err = wsutil.ReadServerData(wc.Conn)
	} else {
		msg, op, err = wsutil.ReadClientData(wc.Conn)
	}
	if err != nil {
		// handle error
		return n, err
	}
	if op == ws.OpClose {
		return n, io.EOF
	}
	copy(b, msg)
	return len(msg), err
}

func (wc *wsConn) Write(b []byte) (n int, err error) {
	if wc.client {
		err = wsutil.WriteClientMessage(wc.Conn, ws.OpText, b)
	} else {
		err = wsutil.WriteServerMessage(wc.Conn, ws.OpText, b)
	}
	if err != nil {
		// handle error
		return n, err
	}
	return len(b), nil
}

type wsListener struct {
	net.Listener
	log log.Logger
	u   ws.Upgrader
}

func NewWsListener(listener net.Listener, log log.Logger) *wsListener {
	l := &wsListener{
		Listener: listener,
		log:      log,
	}
	l.u.Protocol = func(val []byte) bool {
		return string(val) == wsSubProtocol
	}
	return l
}

func (l *wsListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, fmt.Errorf("accept new connection: %w", err)
	}
	if _, err = l.u.Upgrade(conn); err == nil {
		conn = &wsConn{
			Conn:   conn,
			client: false,
		}
	} else {
		l.log.Warnf("fallback to simple TCP connection due to WS upgrade error: %s", err)
		err = nil
	}
	return conn, err
}

type wsProtocol struct {
	protocol
	listeners   ListenerPool
	connections ConnectionPool
	conns       chan Connection
	dialer      ws.Dialer
}

func NewWsProtocol(
	output chan<- sip.Message,
	errs chan<- error,
	cancel <-chan struct{},
	msgMapper sip.MessageMapper,
	logger log.Logger,
) Protocol {
	p := new(wsProtocol)
	p.network = "ws"
	p.reliable = true
	p.streamed = true
	p.conns = make(chan Connection)
	p.log = logger.
		WithPrefix("transport.Protocol").
		WithFields(log.Fields{
			"protocol_ptr": fmt.Sprintf("%p", p),
		})
	//TODO: add separate errs chan to listen errors from pool for reconnection?
	p.listeners = NewListenerPool(p.conns, errs, cancel, p.Log())
	p.connections = NewConnectionPool(output, errs, cancel, msgMapper, p.Log())
	p.dialer.Protocols = []string{wsSubProtocol}
	p.dialer.Timeout = time.Minute
	//pipe listener and connection pools
	go p.pipePools()

	return p
}

func (p *wsProtocol) Done() <-chan struct{} {
	return p.connections.Done()
}

//piping new connections to connection pool for serving
func (p *wsProtocol) pipePools() {
	defer close(p.conns)

	p.Log().Debug("start pipe pools")
	defer p.Log().Debug("stop pipe pools")

	for {
		select {
		case <-p.listeners.Done():
			return
		case conn := <-p.conns:
			logger := log.AddFieldsFrom(p.Log(), conn)

			if err := p.connections.Put(conn, sockTTL); err != nil {
				// TODO should it be passed up to UA?
				logger.Errorf("put new %s connection failed: %s", p.Network(), err)

				conn.Close()

				continue
			}
		}
	}
}

func (p *wsProtocol) Listen(target *Target) error {
	target = FillTargetHostAndPort(p.Network(), target)
	listener, err := p.listen(target)
	if err != nil {
		return &ProtocolError{
			err,
			fmt.Sprintf("listen on %s %s address", p.Network(), target.Addr()),
			fmt.Sprintf("%p", p),
		}
	}

	p.Log().Debugf("begin listening on %s %s", p.Network(), target.Addr())

	//index listeners by local address
	// should live infinitely
	key := ListenerKey(fmt.Sprintf("%s:0.0.0.0:%d", p.network, target.Port))
	err = p.listeners.Put(key, NewWsListener(listener, p.Log()))

	return err //should be nil here
}

func (p *wsProtocol) listen(target *Target) (net.Listener, error) {
	//resolve local TCP endpoint
	laddr, err := p.resolveTarget(target)
	if err != nil {
		return nil, err
	}
	//create tcp listener
	return net.ListenTCP("tcp", laddr)
}

func (p *wsProtocol) Send(target *Target, msg sip.Message) error {
	target = FillTargetHostAndPort(p.Network(), target)

	//validate remote address
	if target.Host == "" {
		return &ProtocolError{
			fmt.Errorf("empty remote target host"),
			fmt.Sprintf("send SIP message to %s %s", p.Network(), target.Addr()),
			fmt.Sprintf("%p", p),
		}
	}
	//resolve remote address
	raddr, err := p.resolveTarget(target)
	if err != nil {
		return err
	}

	//find or create connection
	conn, err := p.getOrCreateConnection(raddr)
	if err != nil {
		return err
	}

	logger := log.AddFieldsFrom(p.Log(), conn, msg)
	logger.Tracef("writing SIP message to %s %s", p.Network(), raddr)

	//send message
	_, err = conn.Write([]byte(msg.String()))

	return err
}

func (p *wsProtocol) resolveTarget(target *Target) (*net.TCPAddr, error) {
	addr := target.Addr()
	//resolve remote address
	raddr, err := net.ResolveTCPAddr(p.network, addr)
	if err != nil {
		return nil, &ProtocolError{
			err,
			fmt.Sprintf("resolve target address %s %s", p.Network(), addr),
			fmt.Sprintf("%p", p),
		}
	}

	return raddr, nil
}

func (p *wsProtocol) getOrCreateConnection(raddr *net.TCPAddr) (Connection, error) {
	key := ConnectionKey(p.network + ":" + raddr.String())
	conn, err := p.connections.Get(key)
	if err != nil {
		p.Log().Debugf("connection for address %s %s not found; create a new one", p.Network(), raddr)

		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		url := fmt.Sprintf("%s://%s", p.network, raddr)
		baseConn, _, _, err := p.dialer.Dial(ctx, url)
		if err == nil {
			baseConn = &wsConn{
				Conn:   baseConn,
				client: true,
			}
		} else {
			if baseConn == nil {
				return nil, &ProtocolError{
					err,
					fmt.Sprintf("connect to %s %s address", p.Network(), raddr),
					fmt.Sprintf("%p", p),
				}
			}

			p.Log().Warnf("fallback to TCP connection due to WS upgrade error: %s", err)
		}

		conn = NewConnection(baseConn, key, p.Log())

		if err := p.connections.Put(conn, sockTTL); err != nil {
			return conn, err
		}
	}

	return conn, nil
}
