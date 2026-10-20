package manage

import (
	"context"
	"sync"
	"time"

	"github.com/backpack/backpack/internal/tui"
)

type ctSetupMenuPath struct {
	host  string
	rows  []ConnTestResult
	view  func() ConnTestSetupView
	start func(ConnTestSetupRequest) error
}

func ctSetupPathsMenu(ctx context.Context, paths []ctSetupMenuPath) {
	if len(paths) == 0 {
		return
	}
	tui.Info("Automatic Setup: Keep Both Test Screens Open. Enter The Ports On Either Server.")
	pick := tui.ChooseOpt("After The Test", []tui.Option{
		{Title: "Set Up A Tested Tunnel", Desc: "configure both servers from here"},
		{Title: "Wait For Setup From The Other Server", Desc: "no more input needed here"},
		{Title: "Finish", Desc: "close automatic setup"},
	})
	if pick < 0 && tui.InputEnded() {
		pick = 1
	}
	view := func() ConnTestSetupView {
		waiting := ConnTestSetupView{State: "expired", Error: "the automatic setup session ended"}
		for _, p := range paths {
			v := p.view()
			if v.ID != "" {
				return v
			}
			if v.State == "starting" || v.State == "waiting" {
				waiting = v
			}
		}
		return waiting
	}
	if pick == 0 {
		at := 0
		if len(paths) > 1 {
			opts := make([]tui.Option, len(paths))
			for i, p := range paths {
				opts[i] = tui.Option{Title: p.host, Desc: ctFamily(p.host)}
			}
			at = tui.ChooseOpt("Tested IP Path", opts)
			if at < 0 {
				return
			}
		}
		p := paths[at]
		r, ok := ctAskSetup(p.rows)
		if !ok {
			return
		}
		until := time.Now().Add(30 * time.Second)
		for !p.view().Ready && time.Now().Before(until) && ctx.Err() == nil {
			ctSleep(ctx, time.Second)
		}
		if err := p.start(r); err != nil {
			tui.Error(err.Error())
			if view().ID == "" {
				return
			}
		}
		ctWaitSetup(ctx, view)
	} else if pick == 1 {
		ctWaitSetup(ctx, view)
	}
}

func ctIranSuiteSetupMenu(ctx context.Context, suite *ConnTestSuite, paths []ConnTestPath) {
	setups := suite.EnableSetup(ctx)
	var menu []ctSetupMenuPath
	for _, p := range paths {
		if s := setups[p.Host]; s != nil {
			menu = append(menu, ctSetupMenuPath{p.Host, p.Rows, s.View, s.Start})
		}
	}
	ctSetupPathsMenu(ctx, menu)
}

func ctKharejSuiteSetupMenu(parent context.Context, routes []connTestAddr, paths []ConnTestPath) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var menu []ctSetupMenuPath
	var workers sync.WaitGroup
	for i, p := range paths {
		if p.State != "done" {
			continue
		}
		a := routes[i]
		raw := (ConnTestLink{Host: a.Host, Coord: a.Coord, Tok: a.Tok}).Short()
		var mu sync.Mutex
		v := ConnTestSetupView{State: "starting"}
		read := func() ConnTestSetupView { mu.Lock(); defer mu.Unlock(); return v }
		menu = append(menu, ctSetupMenuPath{p.Host, p.Rows, read, func(r ConnTestSetupRequest) error { return StartConnTestSetupFromKharej(raw, r) }})
		workers.Add(1)
		go func() {
			defer workers.Done()
			err := RunConnTestSetupPeer(ctx, raw, func(next ConnTestSetupView) {
				mu.Lock()
				changed := v.State != next.State
				v = next
				mu.Unlock()
				if changed && (next.State == "done" || next.State == "failed") {
					ctPrintSetup(next)
				}
			})
			if err != nil {
				mu.Lock()
				v.State, v.Error = "failed", err.Error()
				mu.Unlock()
			}
		}()
	}
	ctSetupPathsMenu(ctx, menu)
	cancel()
	workers.Wait()
}
