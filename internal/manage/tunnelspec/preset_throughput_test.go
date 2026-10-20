package tunnelspec

import "testing"

// The Throughput profile exists to make the opposite trade from the three
// gaming ones, on the same transport. If any of these assertions stop holding it
// has quietly become another gaming preset, which is the one way it can fail
// while still looking configured.
func TestThroughputPresetTradesLatencyForBandwidth(t *testing.T) {
	var thr, agg Spec
	ApplyPreset(&thr, PresetThroughput)
	ApplyPreset(&agg, PresetAggressive)

	if thr.Preset != PresetThroughput {
		t.Fatalf("preset not recorded: %q", thr.Preset)
	}

	// Batched ACKs. Immediate ACKs are what a game wants and what very nearly
	// doubles the packet count on a saturated transfer.
	if thr.KCPAckNoDelay {
		t.Error("AckNoDelay is on — that is the gaming behaviour, not the throughput one")
	}
	if !agg.KCPAckNoDelay {
		t.Error("the gaming preset lost its immediate ACKs — this test is comparing the wrong thing")
	}

	// A slower tick: fewer walks of a much larger send queue per second.
	if thr.KCPInterval <= agg.KCPInterval {
		t.Errorf("interval %d ms is not slower than the gaming preset's %d ms",
			thr.KCPInterval, agg.KCPInterval)
	}

	// The window is the ceiling on a long path, so it must exceed the gaming
	// preset's or the profile buys nothing where it matters most.
	if thr.KCPSndWnd <= agg.KCPSndWnd || thr.KCPRcvWnd <= agg.KCPRcvWnd {
		t.Errorf("window %d/%d does not exceed the gaming preset's %d/%d",
			thr.KCPSndWnd, thr.KCPRcvWnd, agg.KCPSndWnd, agg.KCPRcvWnd)
	}

	// Parity is a straight tax on bandwidth, so it must be lighter — but still
	// present, because this rides on the "udp + kcp + fec" transport.
	if thr.KCPParityShards >= agg.KCPParityShards {
		t.Errorf("parity %d:%d is not lighter than the gaming preset's %d:%d",
			thr.KCPDataShards, thr.KCPParityShards, agg.KCPDataShards, agg.KCPParityShards)
	}
	if thr.KCPDataShards <= 0 || thr.KCPParityShards <= 0 {
		t.Errorf("FEC is off (%d:%d) — this preset is for udp+kcp+fec and must keep some parity",
			thr.KCPDataShards, thr.KCPParityShards)
	}

	// A single stream's flow-control ceiling is what a speed test actually
	// measures, so it must beat the gaming preset's.
	if thr.MuxStreamBuffer <= agg.MuxStreamBuffer {
		t.Errorf("stream buffer %d does not exceed the gaming preset's %d",
			thr.MuxStreamBuffer, agg.MuxStreamBuffer)
	}

	// Tight-loop pool refills are an idle-CPU cost that buys connect latency,
	// which a long transfer does not care about.
	if thr.AggressivePool {
		t.Error("aggressive pool refill is on — it costs idle CPU for no throughput gain")
	}
}

// Each reverse transport consumes the pool, buffers or KCP part of this profile.
func TestThroughputPresetIsOfferedOnEveryReverseTransport(t *testing.T) {
	if !PresetSuitsTransport(PresetThroughput, "kcp") {
		t.Error("throughput should be allowed on kcp")
	}
	if !hasPreset(PresetOptionsFor("kcp"), PresetThroughput) {
		t.Error("throughput missing from the menu for kcp")
	}
	for _, transport := range []string{
		"xdi", "pck",
		"tcp", "tcpmux", "ws", "wss", "wsmux", "wssmux", "udp", "quic", "stealth",
	} {
		if !PresetSuitsTransport(PresetThroughput, transport) {
			t.Errorf("throughput should be allowed on %q", transport)
		}
		if !hasPreset(PresetOptionsFor(transport), PresetThroughput) {
			t.Errorf("throughput missing from the menu for %q", transport)
		}
	}
	if PresetSuitsTransport(PresetThroughput, "unknown") {
		t.Error("unknown transport accepted")
	}

	// The gaming presets stay available everywhere.
	for _, transport := range []string{"tcp", "kcp", "wss"} {
		for _, p := range []string{PresetBalance, PresetTurbo, PresetAggressive} {
			if !hasPreset(PresetOptionsFor(transport), p) {
				t.Errorf("%s went missing from the menu for %q", p, transport)
			}
		}
	}
}

func TestBulkWindowsCover500MbitAt200Milliseconds(t *testing.T) {
	var s Spec
	ApplyPreset(&s, PresetThroughput)
	const requiredBytes = 500_000_000 / 8 / 5 // bandwidth-delay product, 200 ms
	if s.MuxStreamBuffer < requiredBytes || s.MuxRecvBuffer < s.MuxStreamBuffer {
		t.Fatalf("mux flow control is too small: stream=%d session=%d", s.MuxStreamBuffer, s.MuxRecvBuffer)
	}
	// KCP bulk traffic spreads across physical sessions. Avoid a single huge
	// ARQ queue: ACK scans and bursts can reduce aggregate throughput. Allow
	// 100 bytes for KCP, encryption and FEC framing in each segment.
	parallel := min(s.ConnectionPool, 4)
	if parallel*s.KCPSndWnd*(s.KCPMTU-100) < requiredBytes || parallel*s.KCPRcvWnd*(s.KCPMTU-100) < requiredBytes {
		t.Fatal("parallel KCP windows cannot cover the aggregate target bandwidth-delay product")
	}
	if s.ConnectionPool*s.MuxCon < 16384 {
		t.Fatalf("bulk profile has only %d concurrent stream slots", s.ConnectionPool*s.MuxCon)
	}
}

// A config that already names the preset must keep loading regardless of
// transport, or an update would refuse to read a file it wrote itself.
func TestThroughputPresetStaysAValidPresetName(t *testing.T) {
	if !ValidPreset(PresetThroughput) {
		t.Fatal("throughput is not a valid preset name")
	}
	if presetLabel(PresetThroughput) != "Throughput" {
		t.Errorf("label is %q", presetLabel(PresetThroughput))
	}
}

// Adding a preset must not disturb what the existing three produce.
func TestGamingKCPSettingsAreUnchangedByTheNewOne(t *testing.T) {
	type want struct{ interval, snd, par int }
	for preset, w := range map[string]want{
		PresetBalance:    {10, 512, 2},
		PresetTurbo:      {10, 1024, 3},
		PresetAggressive: {10, 2048, 4},
	} {
		var s Spec
		ApplyPreset(&s, preset)
		if s.KCPInterval != w.interval || s.KCPSndWnd != w.snd || s.KCPParityShards != w.par {
			t.Errorf("%s changed: interval=%d sndwnd=%d parity=%d, want %d/%d/%d",
				preset, s.KCPInterval, s.KCPSndWnd, s.KCPParityShards, w.interval, w.snd, w.par)
		}
		if !s.KCPAckNoDelay {
			t.Errorf("%s lost its immediate ACKs", preset)
		}
	}
}

func hasPreset(options []PresetOption, value string) bool {
	for _, o := range options {
		if o.Value == value {
			return true
		}
	}
	return false
}

// The clamp describes the path, not the performance profile, so a preset change
// must leave it alone. If it did not, changing preset would silently undo the
// fix and hand back the stalled tunnel it was applied to cure.
func TestPresetDoesNotTouchTheClamp(t *testing.T) {
	for _, p := range []string{PresetBalance, PresetTurbo, PresetAggressive} {
		s := Spec{MSS: 1208}
		ApplyPreset(&s, p)
		if s.MSS != 1208 {
			t.Fatalf("%s preset changed the MSS clamp to %d", p, s.MSS)
		}
	}
}
