package manage

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/backpack/backpack/internal/tui"
)

func ctAskSetup(results []ConnTestResult) (ConnTestSetupRequest, bool) {
	var choices []tui.Option
	var usable []ConnTestResult
	for _, i := range ctByPreference(results) {
		r := results[i]
		if r.Status != ctOK || !ctSetupSupported(r.Kind, r.Transport) {
			continue
		}
		usable = append(usable, r)
		choices = append(choices, tui.Option{Title: ctKind(r.Kind) + " " + ctName(r.Transport) + " " + r.SNIDomain,
			Desc: fmt.Sprintf("%.1f Mbps · %d ms", r.Mbps, r.RTTms)})
	}
	if len(choices) == 0 {
		tui.Warn("No Tunnel Passed — Run Another Test Before Automatic Setup.")
		return ConnTestSetupRequest{}, false
	}
	pick := tui.ChooseOpt("Set Up A Tested Tunnel On Both Servers", choices)
	if pick < 0 {
		return ConnTestSetupRequest{}, false
	}
	r := usable[pick]
	ports := tui.Prompt("Forwarded Ports (443, or 443=127.0.0.1:2096): ")
	req := ConnTestSetupRequest{ID: randomToken(16), Kind: r.Kind, Transport: r.Transport, SNIDomain: r.SNIDomain, Ports: ports}
	if r.Transport != "udp" || r.Kind != "reverse" {
		req.AcceptUDP = tui.Confirm("Carry UDP As Well As TCP", false)
	}
	return req, true
}

func ctPrintSetup(v ConnTestSetupView) {
	switch v.State {
	case "done":
		tui.Success("Tunnel Connected — Both Servers Configured Automatically.")
		tui.Info("Iran: " + v.Iran + " · Kharej: " + v.Kharej)
	case "failed", "expired":
		tui.Error(v.Error)
	default:
		tui.Info("Automatic Setup: " + v.State)
	}
}

func ctWaitSetup(ctx context.Context, view func() ConnTestSetupView) {
	last := ""
	for ctx.Err() == nil {
		v := view()
		if v.State != last {
			ctPrintSetup(v)
			last = v.State
		}
		if v.State == "done" || v.State == "failed" || v.State == "expired" {
			return
		}
		ctSleep(ctx, time.Second)
	}
}

func ctIranSetupMenu(ctx context.Context, s *ConnTestSetup, results []ConnTestResult) {
	tui.Info("Automatic Setup Is Available For 15 Minutes. Keep Both Test Screens Open.")
	pick := tui.ChooseOpt("After The Test", []tui.Option{
		{Title: "Set Up A Tested Tunnel", Desc: "enter ports here; both servers are configured automatically"},
		{Title: "Wait For Setup From Kharej", Desc: "the other server chooses the tunnel and ports"},
		{Title: "Finish", Desc: "close automatic setup"},
	})
	if pick < 0 && tui.InputEnded() {
		pick = 1 // a headless test remains available for the other operator
	}
	switch pick {
	case 0:
		if r, ok := ctAskSetup(results); ok {
			// The verdict can arrive just before the kharej's engines finish
			// stopping. Give its setup poll a chance to advertise readiness.
			until := time.Now().Add(30 * time.Second)
			for !s.View().Ready && time.Now().Before(until) && ctx.Err() == nil {
				ctSleep(ctx, time.Second)
			}
			if err := s.Start(r); err != nil {
				tui.Error(err.Error())
				return
			}
			ctWaitSetup(ctx, s.View)
		}
	case 1:
		ctWaitSetup(ctx, s.View)
	}
}

// The poller runs while the kharej operator reads or fills the menu. Iran can
// therefore initiate setup without another click on this server.
func ctKharejSetupMenu(ctx context.Context, raw string, results []ConnTestResult) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	v := ConnTestSetupView{State: "starting"}
	done := make(chan error, 1)
	go func() {
		err := RunConnTestSetupPeer(ctx, raw, func(next ConnTestSetupView) {
			mu.Lock()
			changed := v.State != next.State
			v = next
			mu.Unlock()
			if changed && (next.State == "done" || next.State == "failed") {
				fmt.Println()
				ctPrintSetup(next)
			}
		})
		if err != nil {
			mu.Lock()
			v.State, v.Error = "failed", err.Error()
			mu.Unlock()
		}
		done <- err
	}()
	view := func() ConnTestSetupView { mu.Lock(); defer mu.Unlock(); return v }
	tui.Info("Keep This Screen Open: Iran Can Now Set Up Both Ends Automatically.")
	pick := tui.ChooseOpt("After The Test", []tui.Option{
		{Title: "Set Up A Tested Tunnel", Desc: "enter ports here; Iran receives all settings automatically"},
		{Title: "Wait For Setup From Iran", Desc: "no further input is needed on this server"},
		{Title: "Finish", Desc: "close automatic setup"},
	})
	if pick < 0 && tui.InputEnded() {
		pick = 1
	}
	if pick == 0 {
		if r, ok := ctAskSetup(results); ok {
			until := time.Now().Add(30 * time.Second)
			for !view().Ready && time.Now().Before(until) && ctx.Err() == nil {
				ctSleep(ctx, time.Second)
			}
			if err := StartConnTestSetupFromKharej(raw, r); err != nil {
				tui.Error(err.Error())
				cancel()
			} else {
				ctWaitSetup(ctx, view)
			}
		}
	} else if pick == 1 {
		ctWaitSetup(ctx, view)
	}
	if view().State != "done" {
		cancel()
	}
	if err := <-done; err != nil && view().State != "done" {
		tui.Warn(err.Error())
	}
}
