package tunnelspec

// Presets expand into concrete configuration once, at creation or when the
// operator reapplies one. Updating the binary preserves existing manual tuning.
const (
	PresetBalance    = "balance"
	PresetTurbo      = "turbo"
	PresetAggressive = "aggressive"
	// Throughput sizes pools and windows for bulk traffic on a long path.
	// 500 Mbit/s at 200 ms needs 12.5 MB in flight. These windows leave
	// headroom, but neither a carrier nor a network speed is guaranteed.
	PresetThroughput = "throughput"
)

// PresetOption is shared by setup, edit and the panel.
type PresetOption struct {
	Label string `json:"label"`
	Desc  string `json:"desc"`
	Value string `json:"value"`
	// Retained for older options API clients; current profiles apply to all
	// supported reverse transports and leave this false.
	KCPOnly bool `json:"kcpOnly,omitempty"`
}

var PresetOptions = []PresetOption{
	{"Balance", "light on CPU and RAM - small or shared VPS", PresetBalance, false},
	{"Turbo", "recommended - tuned for distant links", PresetTurbo, false},
	{"Aggressive", "more gaming headroom - needs CPU and RAM", PresetAggressive, false},
	{"Throughput", "bulk traffic on fast distant links - needs RAM", PresetThroughput, false},
}

func ValidPreset(p string) bool {
	switch p {
	case PresetBalance, PresetTurbo, PresetAggressive, PresetThroughput:
		return true
	}
	return false
}

// Managed HTTPS helpers run the TCP reverse engine, so they inherit its profile.
func PresetSuitsTransport(preset, transport string) bool {
	if preset != PresetThroughput {
		return true
	}
	switch transport {
	case "tcp", "tcpmux", "stealth", "ws", "wss", "wsmux", "wssmux", "kcp", "xdi", "pck", "udp", "quic":
		return true
	}
	return false
}

func PresetOptionsFor(transport string) []PresetOption {
	out := PresetOptions[:0:0]
	for _, o := range PresetOptions {
		if PresetSuitsTransport(o.Value, transport) {
			out = append(out, o)
		}
	}
	return out
}

func presetLabel(value string) string {
	for _, o := range PresetOptions {
		if o.Value == value {
			return o.Label
		}
	}
	if value == "" {
		return "Custom"
	}
	return value
}

// ApplyPreset is the single expansion used by the terminal, panel and tests.
// MuxCon is admission capacity, not a bandwidth limit or a preallocation of
// every stream window. The receive buffer is shared by a physical session.
func ApplyPreset(s *Spec, preset string) {
	defer func() {
		short := s.ShortClient.Enabled()
		for _, p := range s.ClientPaths {
			short = short || p.Short.Enabled()
		}
		if short {
			// Short HTTPS owns a bounded relay table. A bulk profile cannot
			// exceed that protocol budget or enable aggressive growth.
			s.AggressivePool = false
			s.ConnectionPool = min(s.ConnectionPool, 32)
		}
	}()
	if !ValidPreset(preset) {
		preset = PresetTurbo
	}
	s.Preset = preset
	s.LogLevel = "info"
	s.Nodelay = true
	s.KeepAlive, s.Heartbeat = 75, 40
	s.MuxVersion = 2
	s.MuxFrameSize = 32768
	s.AggressivePool = false
	switch preset {
	case PresetBalance:
		s.ChannelSize, s.ConnectionPool, s.MuxCon = 2048, 4, 32
		s.SoRcvBuf, s.SoSndBuf = 4<<20, 4<<20
		s.MuxRecvBuffer, s.MuxStreamBuffer = 4<<20, 256<<10
	case PresetTurbo:
		// 8 MB gives one stream about 670 Mbit/s at 100 ms RTT. Warm
		// capacity and 256 slots/session absorb connection bursts while the
		// bounded adaptive pool is establishing more sessions.
		s.ChannelSize, s.ConnectionPool, s.MuxCon = 4096, 16, 256
		s.SoRcvBuf, s.SoSndBuf = 8<<20, 8<<20
		s.MuxRecvBuffer, s.MuxStreamBuffer = 16<<20, 8<<20
	case PresetAggressive:
		s.KeepAlive, s.Heartbeat = 60, 25
		s.ChannelSize, s.ConnectionPool, s.MuxCon = 8192, 32, 512
		s.AggressivePool = true
		s.SoRcvBuf, s.SoSndBuf = 32<<20, 32<<20
		s.MuxFrameSize = 65535
		s.MuxRecvBuffer, s.MuxStreamBuffer = 32<<20, 16<<20
	case PresetThroughput:
		// 64 sessions x 512 slots removes the small pool's few-thousand-
		// connection ceiling. A 32 MB stream window covers 500 Mbit/s at
		// 200 ms RTT. Windows grow on demand, but 64 busy receive windows
		// can retain 2 GB, which is why this profile is opt-in.
		s.ChannelSize, s.ConnectionPool, s.MuxCon = 8192, 64, 512
		s.SoRcvBuf, s.SoSndBuf = 32<<20, 32<<20
		s.MuxFrameSize = 65535
		s.MuxRecvBuffer, s.MuxStreamBuffer = 32<<20, 32<<20
	}
	ApplyKCPPreset(s, preset)
}

// ApplyKCPPreset fills the KCP-only knobs. They are written to the config only
// for the KCP transport, but filling them unconditionally keeps a later
// transport change (tcp -> kcp) from landing on zero values.
//
// KCP here is the "UDP + KCP + FEC" transport: a low-latency gaming tunnel, not
// a bulk mover. So every preset shares the same latency-first ARQ — NoDelay on,
// a 10 ms tick, fast-retransmit at 2 duplicate ACKs, KCP's own congestion
// window off, and ACKs sent immediately — and every preset carries FEC, because
// on a game path repairing a lost packet from parity beats waiting a whole RTT
// for a retransmit. What the preset changes is only how much headroom it buys:
// the window (which, with congestion control off, is also the bound on how much
// data can queue and inflate ping) and the parity ratio (how much loss it can
// absorb before a stall shows through).
func ApplyKCPPreset(s *Spec, preset string) {
	// MTU stays below the common 1500 path MTU with room for the KCP, FEC and
	// encryption headers, so a KCP packet never fragments in transit.
	//
	// 1250 rather than the 1350 this used to be, because every preset here
	// carries FEC and FEC is what makes a too-large MTU fatal rather than
	// merely wasteful. kcp-go pads every shard in a group out to the largest
	// packet in it, so the parity packets are always full size — where a
	// tunnel without FEC sends whatever the payload happened to be and slips
	// its small packets through a short path, one with FEC offers that path a
	// steady stream of maximum-size packets and loses all of them. The routes
	// this runs on are frequently short of 1500: a PPPoE uplink, a provider
	// that encapsulates, a tunnel somewhere upstream nobody controls.
	//
	// The 100 bytes buys nothing measurable — at these window sizes it is
	// under 8% of the per-packet payload — and it is the difference between a
	// FEC tunnel that works on an unremarkable server and one that stalls with
	// nothing in the log to say why.
	s.KCPMTU = 1250

	// Latency-first ARQ, identical on every preset. This is the "fast mode"
	// KCP was built for: NoDelay skips the delayed-ACK wait, a 10 ms tick (the
	// floor kcp-go allows) flushes retransmits promptly, Resend=2 retransmits a
	// segment after two duplicate ACKs instead of on a timer, NoCongestion
	// takes KCP's own AIMD window out of the path so one loss does not halve the
	// rate, and AckNoDelay returns each ACK at once so the sender learns of a
	// loss a round trip sooner. Together they trade bandwidth-efficiency for the
	// steady, low ping a game needs.
	s.KCPInterval = 10
	s.KCPResend = 2
	s.KCPNoDelay = 1
	s.KCPNoCongestion = 1
	s.KCPAckNoDelay = true

	switch preset {
	case PresetBalance:
		// The lightest gaming profile. With congestion control off the window
		// is the ceiling on in-flight data, so keeping it small is what keeps
		// buffering — and therefore worst-case ping — bounded on a modest link.
		// 512 × 1250 / 100 ms ≈ 51 Mbit/s, ample for game traffic plus light
		// browsing through the same tunnel.
		s.KCPSndWnd = 512
		s.KCPRcvWnd = 512
		// 10:2 repairs up to 2 lost packets in every 12 (~17% loss) for a 20%
		// parity cost — the floor that still makes "+ FEC" mean something on a
		// clean-ish route.
		s.KCPDataShards = 10
		s.KCPParityShards = 2

	case PresetTurbo:
		// The recommended default. Twice the window — ~102 Mbit/s of headroom at
		// 100 ms — for room to also pull a download through the tunnel without
		// starving the game packets, still small enough to keep queueing low.
		s.KCPSndWnd = 1024
		s.KCPRcvWnd = 1024
		// 10:3 (~33% loss tolerated, 30% overhead): the middle of the loss the
		// tuned Iran-to-abroad routes this preset targets actually see.
		s.KCPDataShards = 10
		s.KCPParityShards = 3

	case PresetAggressive:
		// The strongest profile, for a bad route on a server with headroom.
		// 2048 × 1250 / 100 ms ≈ 205 Mbit/s — larger, but deliberately far below
		// the old 8192: past the bandwidth-delay product the extra window buys a
		// gaming tunnel nothing but bufferbloat.
		s.KCPSndWnd = 2048
		s.KCPRcvWnd = 2048
		// 10:4 (~40% loss tolerated, 40% overhead): the most parity worth
		// spending before a different route is the better answer.
		s.KCPDataShards = 10
		s.KCPParityShards = 4

	case PresetThroughput:
		// Everything here undoes a latency-first default above, and each one is
		// worth a measurable amount of bandwidth.
		//
		// A 20 ms tick instead of 10 halves the number of times a second the
		// session walks its send queue. With a window this large that walk is not
		// free, and on a small VPS the tick is what puts a ceiling on packets per
		// second long before the link does.
		s.KCPInterval = 20
		// Batch the acknowledgements. AckNoDelay returns an ACK the instant a
		// packet lands, which is how a game learns of a loss a round trip sooner
		// — and on a saturated transfer it very nearly doubles the packets on the
		// wire, every one of them competing with the data for the same link.
		s.KCPAckNoDelay = false
		// Keep about 4.7 MB in flight per physical session. A 16K window
		// regressed eight-stream goodput in the capacity benchmark: ACK walks
		// and UDP bursts cost more than the extra window bought. Spread bulk
		// users across sessions instead; four such windows cover the aggregate
		// 500 Mbit/s bandwidth-delay product at 200 ms RTT. One KCP session
		// still has a lower ceiling on a long path.
		s.KCPSndWnd = 4096
		s.KCPRcvWnd = 4096
		// 10:1 — about 10% overhead instead of Aggressive's 40%. Parity is a
		// straight tax on bandwidth: every parity packet is a packet of capacity
		// not carrying data. A gaming preset pays it gladly, because repairing a
		// loss from parity beats waiting a round trip for the retransmit. A
		// transfer does not care when a byte arrives, only how many arrive a
		// second, so it keeps just enough parity to absorb ordinary jitter-loss
		// and lets ARQ handle the rest.
		s.KCPDataShards = 10
		s.KCPParityShards = 1
	}
}

// PresetLabel returns the display name of a tunnel's performance preset.
func PresetLabel(name string) string {
	s, err := Load(name)
	if err != nil {
		return ""
	}
	return presetLabel(s.Preset)
}

// PresetValueLabel maps a raw preset config value to its display name
// ("turbo" → "Turbo", "" → "Custom"), for callers that already hold the
// decoded config and should not re-read it from disk.
func PresetValueLabel(value string) string { return presetLabel(value) }
