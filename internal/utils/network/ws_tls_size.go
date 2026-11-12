package network

const tlsRecordPlaintextSize = 16 << 10

// Leave room for a masked RFC 6455 frame header. The resulting payload is
// below 65536 bytes, so even a client needs at most eight header bytes.
func WebSocketTLSReadSize(bufferSize int) int {
	return tlsRecordAlignedSize(bufferSize, 8)
}

// WSMuxFrameSize is a send-side upper bound only; smux's wire and receive
// windows stay unchanged. Include both its eight-byte header and the largest
// WebSocket header in use below 65536 bytes. Plain WS retains its configured
// frame size, as do custom sizes already clear of a TLS record boundary.
func WSMuxFrameSize(configured int, secure bool) int {
	if !secure {
		return configured
	}
	return tlsRecordAlignedSize(configured, smuxHeaderSize+8)
}

func tlsRecordAlignedSize(size, overhead int) int {
	if size < tlsRecordPlaintextSize || size > 65536 {
		return size
	}
	aligned := (size+overhead)/tlsRecordPlaintextSize*tlsRecordPlaintextSize - overhead
	// Trim only header-sized tails. Never halve an arbitrary custom frame
	// size merely to fill a record, and never gather or delay a short write.
	if size-aligned <= overhead {
		return aligned
	}
	return size
}
