package metrics

import "sync"

// NativeUDPStats distinguishes native datagram health from the TCP control channel.
type NativeUDPStats struct {
	Connected bool   `json:"connected"`
	Flows     int    `json:"flows"`
	Sent      uint64 `json:"sent"`
	Received  uint64 `json:"received"`
	Dropped   uint64 `json:"dropped"`
	BytesIn   uint64 `json:"bytes_in"`
	BytesOut  uint64 `json:"bytes_out"`
}

var nativeUDPState struct {
	sync.Mutex
	next uint64
	id   uint64
	read func() NativeUDPStats
}

func RegisterNativeUDP(read func() NativeUDPStats) func() {
	nativeUDPState.Lock()
	nativeUDPState.next++
	id := nativeUDPState.next
	nativeUDPState.id, nativeUDPState.read = id, read
	nativeUDPState.Unlock()
	return func() {
		nativeUDPState.Lock()
		if nativeUDPState.id == id {
			nativeUDPState.read = nil
		}
		nativeUDPState.Unlock()
	}
}

func nativeUDPSnapshot() *NativeUDPStats {
	nativeUDPState.Lock()
	read := nativeUDPState.read
	nativeUDPState.Unlock()
	if read == nil {
		return nil
	}
	s := read()
	return &s
}
