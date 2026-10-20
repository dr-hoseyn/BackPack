package transport

import (
	"sync/atomic"
	"testing"

	"github.com/sirupsen/logrus"
)

func TestMuxRequestsSessionBeforeAdmissionCapacityIsExhausted(t *testing.T) {
	var streams int32
	sessions := int32(4)
	requests := make(chan struct{}, 8)
	queued := muxRequest(&streams, &sessions, 8, requests, logrus.New())
	for i := 0; i < 23; i++ {
		queued()
	}
	if len(requests) != 0 {
		t.Fatal("requested a session despite more than one session of reserve")
	}
	queued()
	if len(requests) != 1 || atomic.LoadInt32(&streams) != 24 {
		t.Fatal("did not prefetch before the last session's slots filled")
	}
	// A replacement arriving satisfies the reserve; do not keep dialing merely
	// because an earlier sample reached the threshold.
	atomic.StoreInt32(&sessions, 8)
	queued()
	if len(requests) != 1 {
		t.Fatal("continued requesting after reserve was restored")
	}
}
