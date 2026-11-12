package l3

import "net"

// Idle backend sockets must not each pin a maximum-sized datagram buffer.
// Only readers with a ready packet borrow one. Bound retained storage even
// after a burst; excess buffers become collectible as soon as the write ends.
const udpReplyBufferCache = 32

type udpReplyBufferPool struct {
	free chan *[udpBufferSize]byte
}

var udpReplyBuffers = udpReplyBufferPool{free: make(chan *[udpBufferSize]byte, udpReplyBufferCache)}

func (p *udpReplyBufferPool) get() *[udpBufferSize]byte {
	select {
	case buf := <-p.free:
		return buf
	default:
		return new([udpBufferSize]byte)
	}
}

func (p *udpReplyBufferPool) put(buf *[udpBufferSize]byte) {
	select {
	case p.free <- buf:
	default:
	}
}

// Keep the normal connection Read: a bandwidth wrapper must still account
// for these bytes and obey the flow's cancellation and read deadline.
func readUDPReply(waiter *udpReplyWaiter, backend net.Conn, pool *udpReplyBufferPool) (*[udpBufferSize]byte, int, error) {
	if err := waiter.wait(); err != nil {
		return nil, 0, err
	}
	buf := pool.get()
	n, err := backend.Read(buf[:])
	if err != nil {
		pool.put(buf)
		return nil, 0, err
	}
	return buf, n, nil
}
