package zmq

import (
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/pebbe/zmq4"
)

const (
	defaultRecvTimeout    = 2 * time.Minute
	defaultReconnectDelay = 5 * time.Second
)

func envSeconds(key string, fallback time.Duration) time.Duration {
	if s := os.Getenv(key); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return fallback
}

type Subscriber struct {
	endpoint string
	socket   *zmq4.Socket
}

func NewSubscriber(endpoint string) (*Subscriber, error) {
	s := &Subscriber{endpoint: endpoint}
	if err := s.connect(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Subscriber) connect() error {
	if s.socket != nil {
		s.socket.Close()
	}
	socket, err := zmq4.NewSocket(zmq4.SUB)
	if err != nil {
		return err
	}
	// If no message arrives within recvTimeout, RecvMessage returns
	// zmq4.EAGAIN so the caller can detect a stalled/dead publisher
	// and reconnect rather than blocking forever.
	recvTimeout := envSeconds("ZMQ_RECV_TIMEOUT", defaultRecvTimeout)
	if err := socket.SetRcvtimeo(recvTimeout); err != nil {
		socket.Close()
		return err
	}
	if err := socket.Connect(s.endpoint); err != nil {
		socket.Close()
		return err
	}
	socket.SetSubscribe("hashblock")
	s.socket = socket
	return nil
}

// WaitForBlock blocks until a hashblock message arrives.
// If the publisher is silent for recvTimeout it reconnects and returns
// ErrTimeout so the caller can log the stall and try again.
func (s *Subscriber) WaitForBlock() error {
	_, err := s.socket.RecvMessage(0)
	if zmq4.AsErrno(err) == zmq4.Errno(syscall.EAGAIN) {
		// Timeout — publisher silent; reconnect so we get a fresh TCP session.
		time.Sleep(envSeconds("ZMQ_RECONNECT_DELAY", defaultReconnectDelay))
		if rerr := s.connect(); rerr != nil {
			return rerr
		}
		return ErrTimeout
	}
	return err
}
