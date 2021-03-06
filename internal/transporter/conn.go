package transporter

import (
	"context"
	"net"
	"net/http"
	"sync"

	"github.com/Ehco1996/ehco/internal/constant"
	"github.com/Ehco1996/ehco/internal/logger"
	mytls "github.com/Ehco1996/ehco/internal/tls"
	"github.com/gobwas/ws"
	"github.com/xtaci/smux"
)

type muxConn struct {
	net.Conn
	stream *smux.Stream
}

func newMuxConn(conn net.Conn, stream *smux.Stream) *muxConn {
	return &muxConn{Conn: conn, stream: stream}
}

func (c *muxConn) Read(b []byte) (n int, err error) {
	return c.stream.Read(b)
}

func (c *muxConn) Write(b []byte) (n int, err error) {
	return c.stream.Write(b)
}

func (c *muxConn) Close() error {
	return c.stream.Close()
}

type muxSession struct {
	conn         net.Conn
	session      *smux.Session
	maxStreamCnt int
}

func (session *muxSession) GetConn() (net.Conn, error) {
	stream, err := session.session.OpenStream()
	if err != nil {
		return nil, err
	}
	return newMuxConn(session.conn, stream), nil
}

func (session *muxSession) Close() error {
	if session.session == nil {
		return nil
	}
	session.conn.Close()
	return session.session.Close()
}

func (session *muxSession) IsClosed() bool {
	if session.session == nil {
		return true
	}
	return session.session.IsClosed()
}

func (session *muxSession) NumStreams() int {
	if session.session != nil {
		return session.session.NumStreams()
	}
	return 0
}

type mwssTransporter struct {
	sessions     map[string][]*muxSession
	sessionMutex sync.Mutex
	dialer       ws.Dialer
}

func NewMWSSTransporter() *mwssTransporter {
	return &mwssTransporter{
		sessions: make(map[string][]*muxSession),
		dialer: ws.Dialer{
			TLSConfig: mytls.DefaultTLSConfig,
			Timeout:   constant.DialTimeOut},
	}
}

func (tr *mwssTransporter) Dial(addr string) (conn net.Conn, err error) {
	tr.sessionMutex.Lock()
	defer tr.sessionMutex.Unlock()

	var session *muxSession
	var sessionIndex int
	var sessions []*muxSession
	var ok bool

	sessions, ok = tr.sessions[addr]
	// 找到可以用的session
	for sessionIndex, session = range sessions {
		if session.NumStreams() >= session.maxStreamCnt {
			ok = false
		} else {
			ok = true
			break
		}
	}

	// 删除已经关闭的session
	if session != nil && session.IsClosed() {
		logger.Logger.Infof("find closed session %v idx: %d", session, sessionIndex)
		sessions = append(sessions[:sessionIndex], sessions[sessionIndex+1:]...)
		ok = false
	}

	// 创建新的session
	if !ok {
		session, err = tr.initSession(addr)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	} else {
		if len(sessions) > 1 {
			// close last not used session, but we keep one conn in session pool
			if lastSession := sessions[len(sessions)-1]; lastSession.NumStreams() == 0 {
				lastSession.Close()
			}
		}
	}
	cc, err := session.GetConn()
	if err != nil {
		session.Close()
		return nil, err
	}
	tr.sessions[addr] = sessions
	return cc, nil
}

func (tr *mwssTransporter) initSession(addr string) (*muxSession, error) {
	rc, _, _, err := tr.dialer.Dial(context.TODO(), addr)
	if err != nil {
		return nil, err
	}
	// stream multiplex
	smuxConfig := smux.DefaultConfig()
	session, err := smux.Client(rc, smuxConfig)
	if err != nil {
		return nil, err
	}
	logger.Logger.Infof("[mwss] Init new session to: %s", rc.RemoteAddr())
	return &muxSession{conn: rc, session: session, maxStreamCnt: constant.MaxMWSSStreamCnt}, nil
}

type MWSSServer struct {
	Server   *http.Server
	ConnChan chan net.Conn
	ErrChan  chan error
}

func (s *MWSSServer) Upgrade(w http.ResponseWriter, r *http.Request) {
	conn, _, _, err := ws.UpgradeHTTP(r, w)
	if err != nil {
		logger.Logger.Info(err)
		return
	}
	s.mux(conn)
}

func (s *MWSSServer) mux(conn net.Conn) {
	defer conn.Close()

	smuxConfig := smux.DefaultConfig()
	mux, err := smux.Server(conn, smuxConfig)
	if err != nil {
		logger.Logger.Infof("[mwss server err] %s - %s : %s", conn.RemoteAddr(), s.Server.Addr, err)
		return
	}
	defer mux.Close()

	logger.Logger.Infof("[mwss server init] %s  %s", conn.RemoteAddr(), s.Server.Addr)
	defer logger.Logger.Infof("[mwss server close] %s >-< %s", conn.RemoteAddr(), s.Server.Addr)

	for {
		stream, err := mux.AcceptStream()
		if err != nil {
			logger.Logger.Infof("[mwss] accept stream err: %s", err)
			break
		}
		cc := newMuxConn(conn, stream)
		select {
		case s.ConnChan <- cc:
		default:
			cc.Close()
			logger.Logger.Infof("[mwss] %s - %s: connection queue is full", conn.RemoteAddr(), conn.LocalAddr())
		}
	}
}

func (s *MWSSServer) Accept() (conn net.Conn, err error) {
	select {
	case conn = <-s.ConnChan:
	case err = <-s.ErrChan:
	}
	return
}

func (s *MWSSServer) Close() error {
	return s.Server.Close()
}
