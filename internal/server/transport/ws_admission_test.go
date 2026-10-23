package transport

import (
	"context"
	"testing"
)

func TestWebSocketUpgradeCannotEnterAReplacementClientsPool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var seat clientSeat
	seat.sit(ctx, func() {}, func() {}, func(ctx context.Context, _ func()) { <-ctx.Done() })
	owner := seat.client()
	// The HTTP upgrade is in flight while another authenticated control
	// channel replaces this client and drains its pool.
	seat.sit(ctx, func() {}, func() {}, func(ctx context.Context, _ func()) { <-ctx.Done() })
	queued := false
	if seat.admitFor(ctx, owner, func(context.Context) bool { queued = true; return true }) || queued {
		t.Fatal("a late upgrade from the previous client entered the new pool")
	}
	if !seat.admitFor(ctx, seat.client(), func(context.Context) bool { queued = true; return true }) || !queued {
		t.Fatal("the replacement client could not add capacity")
	}
	cancel()
	if seat.admitFor(ctx, seat.client(), func(context.Context) bool { t.Error("ended generation admitted a pool connection"); return true }) {
		t.Fatal("ended generation accepted an upgrade")
	}
}
