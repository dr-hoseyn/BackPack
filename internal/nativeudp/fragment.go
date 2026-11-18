package nativeudp

import (
	"encoding/binary"
	"time"
)

// Helper-native UDP has a smaller effective payload than a loopback UDP socket.
// Bound each authenticated fragment below the minimum QUIC datagram budget.
// Reassembly is unordered and unreliable: no ACK, retry or cross-message wait.
const fragmentSize = 1000
const fragmentHeader = 17 // kind + message ID + index + count + original size
const assemblyLimit = 64
const assemblyLife = 3 * time.Second

type fragmentKey struct {
	session [16]byte
	message uint64
}
type assembly struct {
	created time.Time
	body    []byte
	seen    uint64
	count   uint16
	key     fragmentKey
	prev    *assembly
	next    *assembly
}

// Assembly order tracks the first authenticated fragment of each datagram.
// It lets the receiver prefer new traffic over incomplete old traffic under
// pressure while keeping both the memory limit and eviction work bounded.
func (l *Link) addAssembly(key fragmentKey, size int, count uint16) *assembly {
	var a *assembly
	if len(l.assemblies) == assemblyLimit {
		a = l.oldestAssembly
		l.discardAssembly(a)
	} else {
		a = &assembly{}
	}
	body := a.body
	if len(body) != size {
		body = make([]byte, size)
	}
	*a = assembly{created: time.Now(), body: body, count: count, key: key, prev: l.newestAssembly}
	if l.newestAssembly == nil {
		l.oldestAssembly = a
	} else {
		l.newestAssembly.next = a
	}
	l.newestAssembly = a
	l.assemblies[key] = a
	return a
}

func (l *Link) removeAssembly(a *assembly) {
	delete(l.assemblies, a.key)
	if a.prev == nil {
		l.oldestAssembly = a.next
	} else {
		a.prev.next = a.next
	}
	if a.next == nil {
		l.newestAssembly = a.prev
	} else {
		a.next.prev = a.prev
	}
}

func (l *Link) discardAssembly(a *assembly) {
	l.removeAssembly(a)
	l.dropped.Add(1)
}

// assemble runs under Link.mu, only after AEAD and replay validation.
func (l *Link) assemble(session [16]byte, p []byte) []byte {
	if len(p) < fragmentHeader {
		return nil
	}
	id := binary.BigEndian.Uint64(p[1:9])
	index, count := binary.BigEndian.Uint16(p[9:11]), binary.BigEndian.Uint16(p[11:13])
	size := int(binary.BigEndian.Uint32(p[13:17]))
	if id == 0 || size <= fragmentSize || size > maxPayload || int(count) != (size+fragmentSize-1)/fragmentSize || index >= count {
		return nil
	}
	offset := int(index) * fragmentSize
	if len(p)-fragmentHeader != min(fragmentSize, size-offset) {
		return nil
	}
	key := fragmentKey{session, id}
	a := l.assemblies[key]
	if a == nil {
		a = l.addAssembly(key, size, count)
	} else if time.Since(a.created) > assemblyLife || len(a.body) != size || a.count != count {
		l.discardAssembly(a)
		return nil
	}
	bit := uint64(1) << index
	if a.seen&bit != 0 {
		return nil
	}
	copy(a.body[offset:], p[fragmentHeader:])
	a.seen |= bit
	if a.seen != (uint64(1)<<count)-1 {
		return nil
	}
	l.removeAssembly(a)
	return a.body
}

func (l *Link) sendMessage(b []byte) error {
	// Reassembly is scoped to a Noise session. Rekey may publish a new key
	// during WriteTo; finish this datagram with the old key, which the peer
	// keeps as its previous session, then adopt the new one for the next call.
	l.wmu.Lock()
	defer l.wmu.Unlock()
	k, peer := l.sendKey()
	var prefix [fragmentHeader]byte
	if len(b) <= fragmentSize {
		prefix[0] = 2
		return l.sendPartsLocked(k, peer, prefix[:1], b)
	}
	id := l.messageSeq.Add(1)
	count := (len(b) + fragmentSize - 1) / fragmentSize
	for index := 0; index < count; index++ {
		offset := index * fragmentSize
		p := prefix[:]
		p[0] = 3
		binary.BigEndian.PutUint64(p[1:9], id)
		binary.BigEndian.PutUint16(p[9:11], uint16(index))
		binary.BigEndian.PutUint16(p[11:13], uint16(count))
		binary.BigEndian.PutUint32(p[13:17], uint32(len(b)))
		if err := l.sendPartsLocked(k, peer, p, b[offset:offset+min(fragmentSize, len(b)-offset)]); err != nil {
			return err
		}
	}
	return nil
}
