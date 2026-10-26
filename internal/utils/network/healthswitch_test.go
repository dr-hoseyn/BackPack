package network

import (
	"context"
	"testing"
)

func TestHealthSteeringSwitchesIPv4ToIPv6AndBack(t *testing.T) {
	v4, v6 := "192.0.2.1:443", "[2001:db8::1]:443"
	e := NewEndpoints(v4, v6)
	e.steer.Store(true)
	choice := &healthChoice{candidate: -1, incumbent: -1}
	var moves [][2]string
	e.SetHealthSwitch(func(from, to string) {
		if e.Current() != to || e.Next() != to {
			t.Fatal("transport was asked to reconnect before the new address was published")
		}
		moves = append(moves, [2]string{from, to})
	})
	// A still-connected but degraded IPv4 path needs three better samples.
	scores := []EndpointScore{{Reachable: true, Score: 200}, {Reachable: true, Score: 100}}
	for i := 0; i < 2; i++ {
		e.applyHealthScores(context.Background(), choice, scores, nil)
		if e.Current() != v4 || len(moves) != 0 {
			t.Fatal("one or two bad samples switched the tunnel")
		}
	}
	e.applyHealthScores(context.Background(), choice, scores, nil)
	if e.Current() != v6 || len(moves) != 1 || moves[0] != [2]string{v4, v6} {
		t.Fatalf("degraded IPv4 did not move the active tunnel to IPv6: %v", moves)
	}
	// A dead active IPv6 path falls back immediately, without waiting a streak.
	scores[1].Reachable = false
	e.applyHealthScores(context.Background(), choice, scores, nil)
	if e.Current() != v4 || len(moves) != 2 || moves[1] != [2]string{v6, v4} {
		t.Fatalf("dead IPv6 did not move the tunnel back to IPv4: %v", moves)
	}
	// Ordinary dial selection must not restart an already reconnecting runner.
	e.Prefer(v6)
	e.Rotate()
	if len(moves) != 2 {
		t.Fatal("a dial decision triggered an extra restart")
	}
}

func TestHealthSteeringRequiresConsecutiveEvidence(t *testing.T) {
	e := NewEndpoints("v4:443", "v6:443", "other:443")
	e.steer.Store(true)
	choice := &healthChoice{candidate: -1, incumbent: -1}
	good := []EndpointScore{{Reachable: true, Score: 200}, {Reachable: true, Score: 100}, {Reachable: true, Score: 180}}
	check := func(scores []EndpointScore) { e.applyHealthScores(context.Background(), choice, scores, nil) }
	check(good)
	check(good)
	check(make([]EndpointScore, 3)) // all paths down breaks the evidence streak
	check(good)
	if e.Current() != "v4:443" {
		t.Fatal("unreachable measurement did not reset the improvement streak")
	}
	check(good)
	e.Prefer("other:443") // reconnect chose a new incumbent
	check(good)
	if e.Current() != "other:443" {
		t.Fatal("samples against a previous incumbent triggered a switch")
	}
	// A small gain must not cause a disruptive reconnect.
	near := []EndpointScore{{Reachable: true, Score: 200}, {Reachable: true, Score: 170}, {Reachable: true, Score: 180}}
	for i := 0; i < 4; i++ {
		check(near)
	}
	if e.Current() != "other:443" {
		t.Fatal("less than 15% improvement switched the tunnel")
	}
}

func TestHealthSwitchStopsWithContext(t *testing.T) {
	e := NewEndpoints("v4:443", "v6:443")
	e.steer.Store(true)
	e.SetHealthSwitch(func(_, _ string) { t.Fatal("switch callback ran after cancellation") })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.applyHealthScores(ctx, &healthChoice{}, []EndpointScore{{}, {Reachable: true, Score: 1}}, nil)
	if e.Current() != "v4:443" {
		t.Fatal("cancelled scorer changed the active path")
	}
}
