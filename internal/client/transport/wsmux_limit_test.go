package transport

import "testing"

func TestWSMuxPhysicalSessionBudget(t *testing.T) {
	for _, tc := range []struct{ pool, receive, explicit, want int }{
		{4, 8 << 20, 0, 128}, {16, 16 << 20, 0, 512}, {64, 32 << 20, 0, 2048},
		{4, 8 << 20, 8, 8}, {4, 8 << 20, 64, 64},
	} {
		c := &WsMuxTransport{config: &WsMuxConfig{ConnPoolSize: tc.pool, MaxReceiveBuffer: tc.receive, MaxSessions: tc.explicit}}
		if got := c.poolLimit(); got != tc.want {
			t.Fatalf("%+v: limit %d", tc, got)
		}
	}
}
