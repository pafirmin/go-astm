package astm

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"time"
)

var (
	ErrLineContention  = errors.New("line contention")
	ErrNotAcknowledged = errors.New("frame not acknowledged")

	defaultTimeout = 10 * time.Second
)

type Conn struct {
	// Conn will send <NAK> and return to a waiting state if messages from the analyzer
	// are not handled before this timeout expires. Defaults to 10 seconds.
	Timeout time.Duration

	// Optional error log for logging unexpected errors. Defaults to defalt logger.
	ErrorLog *log.Logger

	conn io.ReadWriteCloser
	// signals pending write transactions to give control to instrument
	releaseCh chan struct{}
	ackCh     chan struct{}
	nakCh     chan struct{}
	enqCh     chan struct{}
	eotCh     chan struct{}
	stxCh     chan []byte
}

type stateFunc func(*stateContext) (stateFunc, error)
type stateContext struct {
	in *bufio.Reader
}

// Listen returns a *Conn that wraps rwc and starts listening for
// messages from the connection analyzer.
func Listen(rwc io.ReadWriteCloser) *Conn {
	c := &Conn{}
	c.conn = rwc
	c.enqCh = make(chan struct{})
	c.ackCh = make(chan struct{})
	c.nakCh = make(chan struct{})
	c.eotCh = make(chan struct{})
	c.stxCh = make(chan []byte)
	c.releaseCh = make(chan struct{})
	c.ErrorLog = log.Default()
	c.Timeout = defaultTimeout

	next := c.waiting

	go func() {
		defer close(c.enqCh)
		ctx := &stateContext{
			in: bufio.NewReader(c.conn),
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

func (c *Conn) waiting(sc *stateContext) (stateFunc, error) {
	for {
		b, err := sc.in.ReadByte()
		if err != nil {
			return c.waiting, err
		}
		switch b {
		case ENQ:
			return c.establishing, nil
		case ACK:
			return c.notify(c.ackCh, c.Timeout, c.waiting), nil
		case NAK:
			return c.notify(c.nakCh, c.Timeout, c.waiting), nil
		default:
			err := c.writeByte(NAK)
			return c.waiting, err
		}
	}
}

func (c *Conn) notify(ch chan struct{}, timeout time.Duration, next stateFunc) stateFunc {
	return func(sc *stateContext) (stateFunc, error) {
		select {
		case ch <- struct{}{}:
		case <-time.After(timeout):
			if err := c.writeByte(NAK); err != nil {
				return c.waiting, nil
			}
		}
		return next, nil
	}
}

func (c *Conn) establishing(sc *stateContext) (stateFunc, error) {
	select {
	case c.releaseCh <- struct{}{}:
	default:
	}

	select {
	case c.enqCh <- struct{}{}:
		return c.receiving, c.writeByte(ACK)
	case <-time.After(c.Timeout):
		err := c.writeByte(NAK)
		return c.waiting, err
	}
}

func (c *Conn) receiving(sc *stateContext) (stateFunc, error) {
	b, err := sc.in.ReadByte()
	if err != nil {
		return c.waiting, err
	}
	switch b {
	case EOT:
		return c.notify(c.eotCh, c.Timeout, c.waiting), nil
	case STX:
		return c.processing, nil
	default:
		err := c.writeByte(NAK)
		return c.waiting, err
	}
}

func (c *Conn) processing(sc *stateContext) (stateFunc, error) {
	var sum uint8
	record := bytes.Buffer{}

	for {
		b, err := sc.in.ReadByte()
		if err != nil {
			return c.waiting, err
		}

		sum += b
		if b == ETX || b == ETB {
			break
		}

		if err = record.WriteByte(b); err != nil {
			return c.waiting, err
		}
	}

	// advance to end of frame
	rest, err := sc.in.ReadBytes(LF)
	if err != nil {
		return c.waiting, err
	}

	got := fmt.Sprintf("%02X", sum)
	cs := rest[0:2]

	if got != string(cs) {
		err := c.writeByte(NAK)
		return c.receiving, err
	}

	out := make([]byte, record.Len()-1)
	copy(out, record.Bytes()[1:])

	select {
	case c.stxCh <- out:
		err = c.writeByte(ACK)
	case <-time.After(c.Timeout):
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

	return &TransactionReader{c: c, timeout: c.Timeout}, nil
}

// RequestControl attempts to establish control of the connection. If successful,
// RequestControl returns a *TransactionWriter ready to write to the connection.
//
// RequestControl blocks until control is established (<ACK> received) or rejected
// (<NAK> received), or until the default timeout is reached. To specify a timeout,
// use [RequestControlContext].
//
// In cases of line contention (<ENQ> received), the instrument is given priority and
// RequestControl returns with ErrLineContention.
//
// It is up to the caller to avoid attempting to establish control during an active
// transaction.
func (c *Conn) RequestControl() (*TransactionWriter, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.Timeout)
	defer cancel()

	return c.requestControl(ctx)
}

// RequestControlContext attempts to establish control of the connection. If successful,
// RequestControlContext returns a TransactionWriter ready to write to the connection.
//
// RequestControlContext blocks until control is established (<ACK> received) or rejected
// (<NAK> received), or until the provided context expires.
//
// In cases of line contention (<ENQ> received), the instrument is given priority and
// RequestControlContext returns with ErrLineContention.
//
// It is up to the caller to avoid attempting to establish control during an active
// transaction.
func (c *Conn) RequestControlContext(ctx context.Context) (*TransactionWriter, error) {
	return c.requestControl(ctx)
}

func (c *Conn) requestControl(ctx context.Context) (*TransactionWriter, error) {
	err := c.writeByte(ENQ)
	if err != nil {
		return nil, err
	}

	select {
	case <-c.ackCh:
		return &TransactionWriter{c: c, timeout: c.Timeout}, nil
	case <-c.releaseCh:
		return nil, ErrLineContention
	case <-c.nakCh:
		return nil, ErrNotAcknowledged
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Close closes m's underlying ReadWriteCloser.
func (c *Conn) Close() error {
	return c.conn.Close()
}

func (c *Conn) write(b []byte) (int, error) {
	return c.conn.Write(b)
}

func (c *Conn) writeByte(b byte) error {
	_, err := c.write([]byte{b})
	return err
}

// TransactionReader represents a request by the instrument to send a message
type TransactionReader struct {
	c       *Conn
	timeout time.Duration
	closed  bool
}

// Read reads the next record from tr into b. Read blocks until a record is available
// or timeout expires. A successful read will automatically trigger an <ACK> to be sent
// on the underlying connection. Read returns io.EOF when <EOT> is received from the
// instrument.
func (tr *TransactionReader) Read(b []byte) (n int, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), tr.timeout)
	defer cancel()

	return tr.read(b, ctx)
}

func (tr *TransactionReader) read(b []byte, ctx context.Context) (n int, err error) {
	if tr.closed {
		return 0, io.EOF
	}

	defer func() {
		if err != nil {
			tr.closed = true
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
// record. Defaults to the same timeout as tr's Conn.
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
// after writing to the underlying connection. Defaults to the same timeout as tr's Conn.
func (tr *TransactionWriter) SetWriteTimeout(d time.Duration) {
	tr.timeout = d
}

// Write writes b to the underlying ReadWriteCloser and blocks until the frame is
// either acknowledged with ACK, rejected with NAK, or timeout expires. When all
// frames have been written, the caller must close the TransactionWriter with a
// call to End to signal the end of the transfer.
func (tr *TransactionWriter) Write(b []byte) (int, error) {
	if tr.closed {
		return 0, errors.New("transaction closed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), tr.timeout)
	defer cancel()

	return tr.write(b, ctx)
}

func (tr *TransactionWriter) write(b []byte, ctx context.Context) (int, error) {
	n, err := tr.c.write(b)
	if err != nil {
		return n, err
	}

	select {
	case <-tr.c.ackCh:
		return n, nil
	case <-tr.c.nakCh:
		return n, ErrNotAcknowledged
	case <-tr.c.releaseCh:
		tr.closed = true
		return n, ErrLineContention
	case <-ctx.Done():
		tr.closed = true

		return n, ctx.Err()
	}
}

// Close closes the transaction and writes EOT to the underlying connection
func (tr *TransactionWriter) Close() error {
	if tr.closed {
		return errors.New("close of closed transaction")
	}

	tr.closed = true
	return tr.c.writeByte(EOT)
}
