package transport

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"

	"github.com/ghettovoice/gosip/log"
	"github.com/ghettovoice/gosip/sip"
)

type tlsProtocol struct {
	tcpProtocol
}

func NewTlsProtocol(
	output chan<- sip.Message,
	errs chan<- error,
	cancel <-chan struct{},
	msgMapper sip.MessageMapper,
	logger log.Logger,
) Protocol {
	p := new(tlsProtocol)
	p.network = "tls"
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
	//pipe listener and connection pools
	go p.pipePools()

	return p
}

func (p *tlsProtocol) listen(target *Target) (net.Listener, error) {
	if target.TLSConfig == nil {
		return nil, fmt.Errorf("valid TLSConfig is required to start listener")
	}
	// resolve local TCP endpoint
	laddr, err := p.resolveTarget(target)
	if err != nil {
		return nil, err
	}

	cert, err := tls.LoadX509KeyPair(target.TLSConfig.Cert, target.TLSConfig.Key)
	if err != nil {
		p.Log().Fatal(err)
	}

	return tls.Listen("tcp", laddr.String(), &tls.Config{
		Certificates: []tls.Certificate{cert},
	})
}

func (p *tlsProtocol) dial(addr net.Addr) (net.Conn, error) {
	return tls.Dial(p.network, addr.String(), &tls.Config{
		VerifyPeerCertificate: func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
			return nil
		},
	})
}
