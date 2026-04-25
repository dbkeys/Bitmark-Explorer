package zmq

import "errors"

// ErrTimeout is returned by WaitForBlock when no message has arrived
// within recvTimeout.  The subscriber has already reconnected; the
// caller should log the event and call WaitForBlock again.
var ErrTimeout = errors.New("ZMQ: no block notification received within timeout — publisher may be stalled")
