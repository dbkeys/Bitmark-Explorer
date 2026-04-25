// Package zmq provides a ZMQ SUB socket that receives hashblock
// notifications from bitmarkd.  All methods are synchronous and
// single-goroutine safe.
package zmq

import (
	"errors"
	"log"
	"syscall"
	"time"

	"github.com/pebbe/zmq4"
)

const (
	recvTimeout    = 2 * time.Minute // treat silence as a dead/stalled publisher
	reconnectDelay = 5 * time.Second
)

// ErrTimeout is returned when no message has arrived within recvTimeout.
// The subscriber has already reconnected; the caller should log and retry.
var ErrTimeout = errors.New("no hashblock notification within 2 minutes — publisher may be stalled")

// Subscriber wraps a ZMQ SUB socket subscribed to "hashblock".
type Subscriber struct {
	endpoint string
	socket   *zmq4.Socket
}

// NewSubscriber connects a SUB socket to endpoint and subscribes to
// "hashblock".  Returns an error if the socket cannot be created; a
// failed Connect is not fatal because ZMQ queues the connection attempt.
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
		s.socket = nil
	}
	sock, err := zmq4.NewSocket(zmq4.SUB)
	if err != nil {
		return err
	}
	if err := sock.SetRcvtimeo(recvTimeout); err != nil {
		sock.Close()
		return err
	}
	if err := sock.Connect(s.endpoint); err != nil {
		sock.Close()
		return err
	}
	sock.SetSubscribe("hashblock")
	s.socket = sock
	return nil
}

// Close releases the underlying ZMQ socket.
func (s *Subscriber) Close() {
	if s.socket != nil {
		s.socket.Close()
		s.socket = nil
	}
}

// WaitForBlock blocks until a hashblock message arrives.
// On a 2-minute timeout it reconnects and returns ErrTimeout so the
// caller can re-check the RPC tip and try again.
func (s *Subscriber) WaitForBlock() error {
	_, err := s.socket.RecvMessage(0)
	if zmqErr, ok := err.(zmq4.Errno); ok && zmqErr == zmq4.Errno(syscall.EAGAIN) {
		log.Printf("ZMQ: receive timeout — reconnecting to %s", s.endpoint)
		time.Sleep(reconnectDelay)
		if rerr := s.connect(); rerr != nil {
			return rerr
		}
		return ErrTimeout
	}
	return err
}
