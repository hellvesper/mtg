package proxy

import (
	"context"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/9seconds/mtg/obfuscated2"
	"github.com/juju/errors"
	uuid "github.com/satori/go.uuid"
	"go.uber.org/zap"
)

// Server is an insgtance of MTPROTO proxy.
type Server struct {
	ip           net.IP
	port         int
	secret       []byte
	logger       *zap.SugaredLogger
	ctx          context.Context
	readTimeout  time.Duration
	writeTimeout time.Duration
	stats        *Stats
	ipv6         bool
}

// Serve does MTPROTO proxying.
func (s *Server) Serve() error {
	addr := net.JoinHostPort(s.ip.String(), strconv.Itoa(s.port))
	lsock, err := net.Listen("tcp", addr)
	if err != nil {
		return errors.Annotate(err, "Cannot create listen socket")
	}

	for {
		if conn, err := lsock.Accept(); err != nil {
			s.logger.Warn("Cannot allocate incoming connection", "error", err)
		} else {
			go s.accept(conn)
		}
	}
}

func (s *Server) accept(conn net.Conn) {
	defer func() {
		s.stats.closeConnection()
		conn.Close() // nolint: errcheck

		if r := recover(); r != nil {
			s.logger.Errorw("Crash of accept handler", "error", r)
		}
	}()

	s.stats.newConnection()
	ctx, cancel := context.WithCancel(context.Background())
	socketID := s.makeSocketID()

	s.logger.Debugw("Client connected",
		"secret", s.secret,
		"addr", conn.RemoteAddr().String(),
		"socketid", socketID,
	)

	clientConn, dc, err := s.getClientStream(ctx, cancel, conn, socketID)
	if err != nil {
		s.logger.Warnw("Cannot initialize client connection",
			"secret", s.secret,
			"addr", conn.RemoteAddr().String(),
			"socketid", socketID,
			"error", err,
		)
		return
	}
	defer clientConn.Close() // nolint: errcheck

	tgConn, err := s.getTelegramStream(ctx, cancel, dc, socketID)
	if err != nil {
		s.logger.Warnw("Cannot initialize Telegram connection",
			"socketid", socketID,
			"error", err,
		)
		return
	}
	defer tgConn.Close() // nolint: errcheck

	wait := &sync.WaitGroup{}
	wait.Add(2)
	go func() {
		defer wait.Done()
		io.Copy(clientConn, tgConn) // nolint: errcheck
	}()
	go func() {
		defer wait.Done()
		io.Copy(tgConn, clientConn) // nolint: errcheck
	}()
	<-ctx.Done()
	wait.Wait()

	s.logger.Debugw("Client disconnected",
		"secret", s.secret,
		"addr", conn.RemoteAddr().String(),
		"socketid", socketID,
	)
}

func (s *Server) makeSocketID() string {
	return uuid.NewV4().String()
}

func (s *Server) getClientStream(ctx context.Context, cancel context.CancelFunc, conn net.Conn, socketID string) (io.ReadWriteCloser, int16, error) {
	wConn := newTimeoutReadWriteCloser(conn, s.readTimeout, s.writeTimeout)
	wConn = newTrafficReadWriteCloser(wConn, s.stats.addIncomingTraffic, s.stats.addOutgoingTraffic)
	frame, err := obfuscated2.ExtractFrame(wConn)
	if err != nil {
		return nil, 0, errors.Annotate(err, "Cannot create client stream")
	}

	obfs2, dc, err := obfuscated2.ParseObfuscated2ClientFrame(s.secret, frame)
	if err != nil {
		return nil, 0, errors.Annotate(err, "Cannot create client stream")
	}

	wConn = newLogReadWriteCloser(wConn, s.logger, socketID, "client")
	wConn = newCipherReadWriteCloser(wConn, obfs2)
	wConn = newCtxReadWriteCloser(ctx, cancel, wConn)

	return wConn, dc, nil
}

func (s *Server) getTelegramStream(ctx context.Context, cancel context.CancelFunc, dc int16, socketID string) (io.ReadWriteCloser, error) {
	socket, err := dialToTelegram(s.ipv6, dc, s.readTimeout)
	if err != nil {
		return nil, errors.Annotate(err, "Cannot dial")
	}
	wConn := newTimeoutReadWriteCloser(socket, s.readTimeout, s.writeTimeout)
	wConn = newTrafficReadWriteCloser(wConn, s.stats.addIncomingTraffic, s.stats.addOutgoingTraffic)

	obfs2, frame := obfuscated2.MakeTelegramObfuscated2Frame()
	if n, err := socket.Write(frame); err != nil || n != len(frame) {
		return nil, errors.Annotate(err, "Cannot write hadnshake frame")
	}

	wConn = newLogReadWriteCloser(wConn, s.logger, socketID, "telegram")
	wConn = newCipherReadWriteCloser(wConn, obfs2)
	wConn = newCtxReadWriteCloser(ctx, cancel, wConn)

	return wConn, nil
}

// NewServer creates new instance of MTPROTO proxy.
func NewServer(ip net.IP, port int, secret []byte, logger *zap.SugaredLogger,
	readTimeout, writeTimeout time.Duration, ipv6 bool, stat *Stats) *Server {
	return &Server{
		ip:           ip,
		port:         port,
		secret:       secret,
		ctx:          context.Background(),
		logger:       logger,
		readTimeout:  readTimeout,
		writeTimeout: writeTimeout,
		stats:        stat,
		ipv6:         ipv6,
	}
}
