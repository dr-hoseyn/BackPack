package transport

// Called under the flow table's lock, which protects the queue from close and
// serializes its producers. Check capacity before copying: select evaluates
// its send value even when default wins, otherwise a full queue keeps allocating
// and collecting a payload-sized object for every discarded packet.
func enqueueUDPPayload(queue chan []byte, packet []byte) bool {
	if cap(queue) > 0 && len(queue) == cap(queue) {
		return false
	}
	select {
	case queue <- append([]byte(nil), packet...):
		return true
	default:
		return false
	}
}
