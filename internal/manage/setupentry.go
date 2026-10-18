package manage

import (
	"fmt"

	"github.com/backpack/backpack/internal/tui"
)

// Setup starts with geography, which has the same meaning for every engine.
// Iran publishes forwarded ports; Kharej holds their backend services. A setup
// link carries its own engine and carrier, so Kharej can apply it directly.

// SetupIran is the entry point for the Iran server: the machine users connect
// to, which exposes the forwarded ports in both directions.
func SetupIran() {
	switch askDirection("Iran") {
	case directionReverse:
		// The Iran side of a reverse tunnel is, in the config, the server.
		SetupServer()
	case directionDirect:
		setupDirectFor(sideIran)
	case directionAWG:
		setupAWG(sideIran)
	case directionAutomatic:
		setupAutomaticIran()
	}
}

// SetupKharej is the entry point for the server abroad: the machine that holds
// the real service.
func SetupKharej() {
	// A link already names the engine, carrier and every paired setting.
	// Ask for it before asking questions it can answer.
	switch tui.ChooseOpt("Setup Kharej", []tui.Option{
		{Title: "Setup Link", Desc: "recommended — detect the tunnel and protocols from the Iran link"},
		{Title: "Manual Setup", Desc: "choose the tunnel type and enter settings"},
	}) {
	case 0:
		setupFromLink()
		return
	case 1:
	default:
		return
	}
	switch askDirection("Kharej") {
	case directionReverse:
		// The kharej side of a reverse tunnel is, in the config, the client.
		setupClientManual()
	case directionDirect:
		setupDirectFor(sideKharej)
	case directionAWG:
		setupAWG(sideKharej)
	case directionAutomatic:
		setupFromLink()
	}
}

type tunnelDirection int

const (
	directionCancelled tunnelDirection = iota
	directionReverse
	directionDirect
	directionAWG
	directionAutomatic
)

// askDirection is the one question that decides which engine builds the
// tunnel. Options are described by the situation that calls for them,
// because "reverse" and "direct" mean nothing to somebody setting up their
// first tunnel.
func askDirection(machine string) tunnelDirection {
	tui.Clear()
	tui.Title("Setup " + machine)
	fmt.Println()

	switch tui.ChooseOpt("Direction", []tui.Option{
		{
			Title: "Reverse",
			Desc:  "kharej dials Iran — try this first",
		},
		{
			Title: "Direct",
			Desc:  "Iran dials kharej — when inbound to Iran is blocked",
		},
		{Title: "Automatic", Desc: "enter forwarded ports — verified paths, pairing and failover are automatic"},
		{Title: "AmneziaWG", Desc: "independent native IP tunnel — UDP, Linux and TUN required"},
	}) {
	case 0:
		return directionReverse
	case 1:
		return directionDirect
	case 2:
		return directionAutomatic
	case 3:
		return directionAWG
	}
	return directionCancelled
}

// setupDirectFor runs the direct wizard for a machine already chosen, so the
// question the menu just answered is not asked a second time.
func setupDirectFor(side directSide) {
	tui.Clear()
	tui.Title("Direct Tunnel — " + sideName(side))
	fmt.Println()

	// Straight to how it travels. There is no kind to choose any more: a direct
	// tunnel is a full IP tunnel wrapped in Backpack's own GRE, and the only
	// open question is which carrier gets it across.
	setupL3(side)
}

func sideName(s directSide) string {
	if s == sideIran {
		return "Iran"
	}
	return "Kharej"
}
