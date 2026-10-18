package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/backpack/backpack/internal/manage"
)

func runDiagnose(args []string) Result {
	asJSON, args := takeJSONFlag(args)
	path, selected := "", ""
	for len(args) > 0 {
		if len(args) < 2 {
			return fail(CodeUsage, "diagnose requires -c FILE [--path NAME] [--json]\n")
		}
		switch args[0] {
		case "-c":
			if path != "" {
				return fail(CodeUsage, "duplicate -c\n")
			}
			path = args[1]
		case "--path":
			if selected != "" {
				return fail(CodeUsage, "duplicate --path\n")
			}
			selected = args[1]
		default:
			return fail(CodeUsage, "unknown diagnose option\n")
		}
		args = args[2:]
	}
	if path == "" {
		return fail(CodeUsage, "diagnose requires -c FILE [--path NAME] [--json]\n")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	reports, err := manage.DiagnoseDatagramConfig(ctx, path, selected)
	if err != nil {
		return fail(CodeFailed, "%v\n", err)
	}
	var r Result
	if asJSON {
		r = jsonResult(reports)
	} else {
		var b strings.Builder
		for _, report := range reports {
			fmt.Fprintf(&b, "%s (%s), verified=%t\n", report.Path, report.Protocol, report.Verified)
			for _, s := range report.Stages {
				fmt.Fprintf(&b, "  %s %s [%s]: %s", s.Status, s.Name, s.Code, s.Detail)
				if s.Endpoint != "" {
					fmt.Fprintf(&b, " (%s; sent=%d received=%d)", s.Endpoint, s.SentPackets, s.ReceivedPackets)
				}
				b.WriteByte('\n')
			}
		}
		r = ok(b.String())
	}
	for _, report := range reports {
		if !report.Verified {
			r.Code = CodeUnhealthy
		}
	}
	return r
}
