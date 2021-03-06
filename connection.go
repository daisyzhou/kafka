package kafka

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sync"
	"time"

	"github.com/dropbox/kafka/proto"
)

// ErrClosed is returned as result of any request made using closed connection.
var ErrClosed = errors.New("closed")

// Low level abstraction over connection to Kafka.
type connection struct {
	addr      string
	startTime time.Time
	rw        io.ReadWriteCloser
	stop      chan struct{}
	nextID    chan int32

	// mu protects the following members. It must only be accessed by connection methods.
	mu    *sync.Mutex
	respc map[int32]chan []byte

	// stopErr is set if and only if this connection has been closed. If set, it indicates
	// the error that closed the connection.
	stopErr error
}

// newConnection returns new, initialized connection or error
func newTCPConnection(address string, timeout time.Duration) (*connection, error) {
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return nil, err
	}
	c := &connection{
		addr:      address,
		mu:        &sync.Mutex{},
		stop:      make(chan struct{}),
		nextID:    make(chan int32),
		rw:        conn,
		respc:     make(map[int32]chan []byte),
		startTime: time.Now(),
	}
	go c.nextIDLoop()
	go c.readRespLoop()
	return c, nil
}

// nextIDLoop generates correlation IDs, making sure they are always in order
// and within the scope of request-response mapping array.
func (c *connection) nextIDLoop() {
	var id int32 = 1
	for {
		select {
		case <-c.stop:
			close(c.nextID)
			return
		case c.nextID <- id:
			id++
			if id == math.MaxInt32 {
				id = 1
			}
		}
	}
}

// readRespLoop constantly reading response messages from the socket and after
// partial parsing, sends byte representation of the whole message to request
// sending process.
func (c *connection) readRespLoop() {
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()

		for _, cc := range c.respc {
			close(cc)
		}
		c.respc = make(map[int32]chan []byte)
	}()

	rd := bufio.NewReader(c.rw)
	for {
		correlationID, b, err := proto.ReadResp(rd)
		if err != nil {
			c.mu.Lock()
			if c.stopErr == nil {
				c.stopErr = err
				close(c.stop)
			}
			c.mu.Unlock()
			return
		}

		c.mu.Lock()
		rc, ok := c.respc[correlationID]
		delete(c.respc, correlationID)
		c.mu.Unlock()
		if !ok {
			log.Warningf("response to unknown request: %d", correlationID)
			continue
		}

		select {
		case <-c.stop:
			c.mu.Lock()
			if c.stopErr == nil {
				c.stopErr = ErrClosed
			}
			c.mu.Unlock()
		case rc <- b:
		}
		close(rc)
	}
}

// respWaiter register listener to response message with given correlationID
// and return channel that single response message will be pushed to once it
// will arrive.
// After pushing response message, channel is closed.
//
// Upon connection close, all unconsumed channels are closed.
func (c *connection) respWaiter(correlationID int32) (respc chan []byte, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stopErr != nil {
		return nil, c.stopErr
	}
	if _, ok := c.respc[correlationID]; ok {
		log.Errorf("correlation conflict: %d", correlationID)
		return nil, fmt.Errorf("correlation conflict: %d", correlationID)
	}
	respc = make(chan []byte)
	c.respc[correlationID] = respc
	return respc, nil
}

// releaseWaiter removes response channel from waiters pool and close it.
// Calling this method for unknown correlationID has no effect.
func (c *connection) releaseWaiter(correlationID int32) {
	c.mu.Lock()
	defer c.mu.Unlock()

	rc, ok := c.respc[correlationID]
	if ok {
		delete(c.respc, correlationID)
		close(rc)
	}
}

// StartTime returns the time the connection was established.
func (c *connection) StartTime() time.Time {
	return c.startTime
}

// Close close underlying transport connection and cancel all pending response
// waiters.
func (c *connection) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stopErr == nil {
		c.stopErr = ErrClosed
		close(c.stop)
	}
	return c.rw.Close()
}

// IsClosed returns whether or not this connection has been stopped/closed.
func (c *connection) IsClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.stopErr != nil
}

// Metadata sends given metadata request to kafka node and returns related
// metadata response.
// Calling this method on closed connection will always return ErrClosed.
func (c *connection) Metadata(req *proto.MetadataReq) (*proto.MetadataResp, error) {
	var ok bool
	if req.CorrelationID, ok = <-c.nextID; !ok {
		return nil, c.stopErr
	}

	respc, err := c.respWaiter(req.CorrelationID)
	if err != nil {
		log.Errorf("failed waiting for response: %s", err)
		return nil, fmt.Errorf("wait for response: %s", err)
	}

	if _, err := req.WriteTo(c.rw); err != nil {
		log.Errorf("cannot write: %s", err)
		c.releaseWaiter(req.CorrelationID)
		return nil, err
	}
	b, ok := <-respc
	if !ok {
		return nil, c.stopErr
	}
	return proto.ReadMetadataResp(bytes.NewReader(b))
}

// Produce sends given produce request to kafka node and returns related
// response. Sending request with no ACKs flag will result with returning nil
// right after sending request, without waiting for response.
// Calling this method on closed connection will always return ErrClosed.
func (c *connection) Produce(req *proto.ProduceReq) (*proto.ProduceResp, error) {
	var ok bool
	if req.CorrelationID, ok = <-c.nextID; !ok {
		return nil, c.stopErr
	}

	if req.RequiredAcks == proto.RequiredAcksNone {
		_, err := req.WriteTo(c.rw)
		return nil, err
	}

	respc, err := c.respWaiter(req.CorrelationID)
	if err != nil {
		log.Errorf("failed waiting for response: %s", err)
		return nil, fmt.Errorf("wait for response: %s", err)
	}

	if _, err := req.WriteTo(c.rw); err != nil {
		log.Errorf("cannot write: %s", err)
		c.releaseWaiter(req.CorrelationID)
		return nil, err
	}
	b, ok := <-respc
	if !ok {
		return nil, c.stopErr
	}
	return proto.ReadProduceResp(bytes.NewReader(b))
}

// Fetch sends given fetch request to kafka node and returns related response.
// Calling this method on closed connection will always return ErrClosed.
func (c *connection) Fetch(req *proto.FetchReq) (*proto.FetchResp, error) {
	var ok bool
	if req.CorrelationID, ok = <-c.nextID; !ok {
		return nil, c.stopErr
	}

	respc, err := c.respWaiter(req.CorrelationID)
	if err != nil {
		log.Errorf("failed waiting for response: %s", err)
		return nil, fmt.Errorf("wait for response: %s", err)
	}

	if _, err := req.WriteTo(c.rw); err != nil {
		log.Errorf("cannot write: %s", err)
		c.releaseWaiter(req.CorrelationID)
		return nil, err
	}
	b, ok := <-respc
	if !ok {
		return nil, c.stopErr
	}
	resp, err := proto.ReadFetchResp(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}

	// Compressed messages are returned in full batches for efficiency
	// (the broker doesn't need to decompress).
	// This means that it's possible to get some leading messages
	// with a smaller offset than requested. Trim those.
	for ti := range resp.Topics {
		topic := &resp.Topics[ti]
		reqTopic := &req.Topics[ti]
		for pi := range topic.Partitions {
			partition := &topic.Partitions[pi]
			reqPartition := &reqTopic.Partitions[pi]
			i := 0
			for _, msg := range partition.Messages {
				if msg.Offset >= reqPartition.FetchOffset {
					break
				}
				i++
			}
			partition.Messages = partition.Messages[i:]
		}
	}
	return resp, nil
}

// Offset sends given offset request to kafka node and returns related response.
// Calling this method on closed connection will always return ErrClosed.
func (c *connection) Offset(req *proto.OffsetReq) (*proto.OffsetResp, error) {
	var ok bool
	if req.CorrelationID, ok = <-c.nextID; !ok {
		return nil, c.stopErr
	}

	respc, err := c.respWaiter(req.CorrelationID)
	if err != nil {
		log.Errorf("failed waiting for response: %s", err)
		return nil, fmt.Errorf("wait for response: %s", err)
	}

	// TODO(husio) documentation is not mentioning this directly, but I assume
	// -1 is for non node clients
	req.ReplicaID = -1
	if _, err := req.WriteTo(c.rw); err != nil {
		log.Errorf("cannot write: %s", err)
		c.releaseWaiter(req.CorrelationID)
		return nil, err
	}

	b, ok := <-respc
	if !ok {
		return nil, c.stopErr
	}

	return proto.ReadOffsetResp(bytes.NewReader(b))
}

func (c *connection) GroupCoordinator(req *proto.GroupCoordinatorReq) (*proto.GroupCoordinatorResp, error) {
	var ok bool
	if req.CorrelationID, ok = <-c.nextID; !ok {
		return nil, c.stopErr
	}
	respc, err := c.respWaiter(req.CorrelationID)
	if err != nil {
		log.Errorf("failed waiting for response: %s", err)
		return nil, fmt.Errorf("wait for response: %s", err)
	}

	if _, err := req.WriteTo(c.rw); err != nil {
		log.Errorf("cannot write: %s", err)
		c.releaseWaiter(req.CorrelationID)
		return nil, err
	}
	b, ok := <-respc
	if !ok {
		return nil, c.stopErr
	}
	return proto.ReadGroupCoordinatorResp(bytes.NewReader(b))
}

func (c *connection) OffsetCommit(req *proto.OffsetCommitReq) (*proto.OffsetCommitResp, error) {
	var ok bool
	if req.CorrelationID, ok = <-c.nextID; !ok {
		return nil, c.stopErr
	}
	respc, err := c.respWaiter(req.CorrelationID)
	if err != nil {
		log.Errorf("failed waiting for response: %s", err)
		return nil, fmt.Errorf("wait for response: %s", err)
	}

	if _, err := req.WriteTo(c.rw); err != nil {
		log.Errorf("cannot write: %s", err)
		c.releaseWaiter(req.CorrelationID)
		return nil, err
	}
	b, ok := <-respc
	if !ok {
		return nil, c.stopErr
	}
	return proto.ReadOffsetCommitResp(bytes.NewReader(b))
}

func (c *connection) OffsetFetch(req *proto.OffsetFetchReq) (*proto.OffsetFetchResp, error) {
	var ok bool
	if req.CorrelationID, ok = <-c.nextID; !ok {
		return nil, c.stopErr
	}
	respc, err := c.respWaiter(req.CorrelationID)
	if err != nil {
		log.Errorf("failed waiting for response: %s", err)
		return nil, fmt.Errorf("wait for response: %s", err)
	}

	if _, err := req.WriteTo(c.rw); err != nil {
		log.Errorf("cannot write: %s", err)
		c.releaseWaiter(req.CorrelationID)
		return nil, err
	}
	b, ok := <-respc
	if !ok {
		return nil, c.stopErr
	}
	return proto.ReadOffsetFetchResp(bytes.NewReader(b))
}
