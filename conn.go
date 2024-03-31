// Package astm provides a basic interface for handling ASTM communications
// over serial port or TCP.
package astm

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"
)

var (
	ErrLineContention  = errors.New("line contention")
	ErrNotAcknowledged = errors.New("frame not acknowledged")
	defaultTimeout     = 10 * time.Second
)

type TransferState string

const (
	Idle             TransferState = "idle"
	LineBusy         TransferState = "line_busy"
	Establishing     TransferState = "establishing"
	Receiving        TransferState = "receiving"
	Processing       TransferState = "processing"
	Sending          TransferState = "sending"
	AwaitingResponse TransferState = "awaiting_response"
)

type Conn struct {
	// Mediator will return to an idle state if nothing is received from the instrument for this
	// duration. The timeout is reset with every communication sent or received.
	Timeout time.Duration

	// Optional error log for logging unexpected errors
	ErrorLog *log.Logger
	state    TransferState

	conn io.ReadWriteCloser
	// signals pending write transactions to give control to instrument
	releaseCh chan struct{}
	idleCh    chan struct{}
	ackCh     chan struct{}
	nakCh     chan struct{}
	enqCh     chan struct{}
	eotCh     chan struct{}
	stxCh     chan []byte
	mu        sync.Mutex
}

// Listen creates a new Mediator that listens on conn.
func Listen(conn io.ReadWriteCloser) *Conn {
	c := &Conn{}
	c.conn = conn
	c.idleCh = make(chan struct{})
	c.enqCh = make(chan struct{})
	c.ackCh = make(chan struct{})
	c.nakCh = make(chan struct{})
	c.eotCh = make(chan struct{})
	c.releaseCh = make(chan struct{})
	c.stxCh = make(chan []byte)
	c.ErrorLog = log.Default()

	if c.Timeout == 0 {
		c.Timeout = 15 * time.Second
	}

	next := c.idle
	go func() {
		defer close(c.enqCh)

		ctx := &stateContext{
			conn: bufio.NewReader(c.conn),
		}

		var err error
		for {
			next, err = next(ctx)
			if err != nil {
				c.ErrorLog.Println(err)
				break
			}
		}
	}()

	return c
}

func (c *Conn) State() TransferState {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.state
}

type stateFunc func(*stateContext) (stateFunc, error)
type stateContext struct {
	buf  bytes.Buffer
	conn *bufio.Reader
}

func (c *Conn) idle(sc *stateContext) (stateFunc, error) {
	c.mu.Lock()
	c.state = Idle
	c.mu.Unlock()

	select {
	case c.idleCh <- struct{}{}:
	default:
	}

	for {
		b, err := sc.conn.ReadByte()
		if err != nil {
			return c.idle, err
		}
		switch b {
		case ENQ:
			return c.establishing, nil
		case ACK:
			return c.notify(c.ackCh, defaultTimeout, c.sending), nil
		case NAK:
			return c.notify(c.nakCh, defaultTimeout, c.idle), nil
		default:
			err := c.writeByte(NAK)
			return c.idle, err
		}
	}
}

func (c *Conn) notify(ch chan struct{}, timeout time.Duration, next stateFunc) stateFunc {
	return func(sc *stateContext) (stateFunc, error) {
		select {
		case ch <- struct{}{}:
		case <-time.After(timeout):
			if err := c.writeByte(NAK); err != nil {
				return c.idle, nil
			}
		}
		return next, nil
	}
}

func (c *Conn) sending(sc *stateContext) (stateFunc, error) {
	c.mu.Lock()
	c.state = Sending
	c.mu.Unlock()

	b, err := sc.conn.ReadByte()
	if err != nil {
		return c.idle, err
	}
	switch b {
	case ACK:
		return c.notify(c.ackCh, defaultTimeout, c.sending), nil
	case NAK:
		return c.notify(c.nakCh, defaultTimeout, c.sending), nil
	default:
		err := c.writeByte(NAK)
		return c.idle, err
	}
}

func (c *Conn) establishing(sc *stateContext) (stateFunc, error) {
	c.mu.Lock()
	c.state = Establishing
	c.mu.Unlock()

	select {
	case c.releaseCh <- struct{}{}:
	default:
	}

	select {
	case c.enqCh <- struct{}{}:
		return c.receiving, c.writeByte(ACK)
	case <-time.After(defaultTimeout):
		err := c.writeByte(NAK)
		return c.idle, err
	}
}

func (c *Conn) receiving(sc *stateContext) (stateFunc, error) {
	c.mu.Lock()
	c.state = Receiving
	c.mu.Unlock()

	b, err := sc.conn.ReadByte()
	if err != nil {
		return c.idle, err
	}
	switch b {
	case EOT:
		return c.notify(c.eotCh, defaultTimeout, c.idle), nil
	case STX:
		return c.processing, nil
	default:
		err := c.writeByte(NAK)
		return c.sending, err
	}
}

func (c *Conn) processing(sc *stateContext) (stateFunc, error) {
	c.mu.Lock()
	c.state = Processing
	c.mu.Unlock()

	var sum uint8
	isPartial := false
	temp := bytes.Buffer{}

	for {
		b, err := sc.conn.ReadByte()
		if err != nil {
			return c.idle, err
		}

		sum += b
		if b == ETX || b == ETB {
			isPartial = b == ETB
			break
		}

		if err = temp.WriteByte(b); err != nil {
			return c.idle, err
		}
	}

	cs, _ := sc.conn.Peek(2)
	got := fmt.Sprintf("%02X", sum)

	// advance to end of frame
	_, err := sc.conn.ReadBytes(LF)
	if err != nil {
		return c.idle, err
	}

	if got != string(cs) {
		sc.buf.Reset()
		err := c.writeByte(NAK)
		return c.receiving, err
	}

	// exclude frame number from output
	if _, err = sc.buf.Write(temp.Bytes()[1:]); err != nil {
		return c.idle, err
	}

	if isPartial {
		err := c.writeByte(ACK)
		return c.receiving, err
	}

	out := make([]byte, sc.buf.Len())
	copy(out, sc.buf.Bytes())
	sc.buf.Reset()

	select {
	case c.stxCh <- out:
		err = c.writeByte(ACK)
	case <-time.After(defaultTimeout):
		err = c.writeByte(NAK)
	}

	return c.receiving, err
}

// Acknowledge waits to receive <ENQ> from the underlying connection, acknowledges
// the request with <ACK>, then returns a *TransactionReader ready to read from
// the underlying connection.
func (c *Conn) Acknowledge() (*TransactionReader, error) {
	_, ok := <-c.enqCh
	if !ok {
		return nil, errors.New("connection closed")
	}

	return &TransactionReader{c: c, timeout: defaultTimeout}, nil
}

// RequestControl attempts to establish control of the connection. If successful,
// RequestControl returns a *TransactionWriter ready to write to the connection.
//
// RequestControl blocks until control is established (<ENQ> received) or rejected
// (<NAK> received), or until the default timeout is reached. To specify a timeout,
// use [RequestControlContext].
//
// In cases of line contention, the instrument is given priority and RequestControl
// returns with ErrLineContention.
func (m *Conn) RequestControl() (*TransactionWriter, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()

	return m.requestControl(ctx)
}

// RequestControlContext attempts to establish control of the connection. If successful,
// RequestControlContext returns a TransactionWriter ready to write to the connection.
//
// RequestControlContext blocks until control is established (<ENQ> received) or rejected
// (<NAK> received), or until the provided context expires.
//
// In cases of line contention, the instrument is given priority and RequestControlContext
// returns with ErrLineContention.
func (m *Conn) RequestControlContext(ctx context.Context) (*TransactionWriter, error) {
	return m.requestControl(ctx)
}

func (m *Conn) requestControl(ctx context.Context) (*TransactionWriter, error) {
	m.mu.Lock()

	for m.state != Idle {
		m.mu.Unlock()
		select {
		case <-m.idleCh:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		m.mu.Lock()
	}

	err := m.writeByte(ENQ)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	m.mu.Unlock()

	select {
	case <-m.ackCh:
		return &TransactionWriter{c: m, timeout: defaultTimeout}, nil
	case <-m.releaseCh:
		return nil, ErrLineContention
	case <-m.nakCh:
		return nil, ErrNotAcknowledged
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close waits for any existing transaction to finish then closes m's underlying ReadWriteCloser.
func (m *Conn) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.conn.Close()
}

func (m *Conn) write(b []byte) (int, error) {
	return m.conn.Write(b)
}

func (m *Conn) writeByte(b byte) error {
	_, err := m.write([]byte{b})
	return err
}

// TransactionReader represents a request by the instrument to send a message
type TransactionReader struct {
	c       *Conn
	timeout time.Duration
	done    bool
}

// Read reads the next record from tr into b. Read blocks until a record is available
// or timeout expires. Read returns io.EOF when <EOT> is received from the instrument.
// Bytes read into b will always constitute a full, valid ASTM record.
func (tr *TransactionReader) Read(b []byte) (n int, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), tr.timeout)
	defer cancel()

	return tr.read(b, ctx)
}

func (tr *TransactionReader) read(b []byte, ctx context.Context) (n int, err error) {
	if tr.done {
		return 0, io.EOF
	}

	defer func() {
		if err != nil {
			tr.done = true
		}
	}()

	select {
	case fr := <-tr.c.stxCh:
		n := copy(b, fr)
		return n, nil
	case <-tr.c.eotCh:
		return 0, io.EOF
	case <-ctx.Done():
		return 0, errors.New("read timeout")
	}
}

// SetReadTimeout sets the maximum amount of time Read should wait for the next
// record.
func (tr *TransactionReader) SetReadTimeout(d time.Duration) {
	tr.timeout = d
}

// A TransactionWriter is a Writer that writes frames to the underlying connection.
type TransactionWriter struct {
	c       *Conn
	closed  bool
	timeout time.Duration
}

// SetWriteTimeout sets the maximum time TransactionWriter should wait for ACK
// after writing to the underlying connection
func (tx *TransactionWriter) SetWriteTimeout(d time.Duration) {
	tx.timeout = d
}

// Write writes b to the underlying ReadWriteCloser and blocks until the frame is
// either acknowledged with ACK, rejected with NAK, or timeout expires. When all
// frames have been written, the caller must close the TransactionWriter with a
// call to End to signal the end of the transfer.
func (tx *TransactionWriter) Write(b []byte) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tx.timeout)
	defer cancel()

	return tx.write(b, ctx)
}

func (tx *TransactionWriter) write(b []byte, ctx context.Context) (int, error) {
	if tx.closed {
		return 0, errors.New("transaction closed")
	}

	tx.c.mu.Lock()
	n, err := tx.c.write(b)
	if err != nil {
		tx.c.mu.Unlock()
		return n, err
	}
	tx.c.mu.Unlock()

	select {
	case <-tx.c.ackCh:
		return n, nil
	case <-tx.c.nakCh:
		return n, ErrNotAcknowledged
	case <-tx.c.releaseCh:
		tx.closed = true
		return n, ErrLineContention
	case <-ctx.Done():
		tx.closed = true

		return n, ctx.Err()
	}
}

// End closes the transaction and writes EOT to the underlying connection
func (tx *TransactionWriter) End() error {
	tx.c.mu.Lock()
	defer tx.c.mu.Unlock()

	tx.closed = true
	return tx.c.writeByte(EOT)
}
