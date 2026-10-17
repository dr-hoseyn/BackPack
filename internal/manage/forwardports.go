package manage

import (
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"

	"github.com/backpack/backpack/internal/manage/spec"
	"github.com/backpack/backpack/internal/tui"
	"github.com/backpack/backpack/internal/tunnel/portmap"
)

// forwardPortEditor edits a private draft. Returning without Save never writes
// a config or restarts a service, including when stdin closes mid-question.
type forwardPortEditor struct {
	defaultHost string
	allowEmpty  bool
	reverse     bool
	creating    bool
	reviewNext  bool     // the shared settings review provides the final confirmation
	reserved    []string // control listener and hidden relay, never editable here
	checkEntry  func(string) error
}

func (e forwardPortEditor) setupHelp() {
	e.help()
	tui.Warn("Type A For Guided Setup With Separate Source And Target Fields.")
	tui.Warn("Or Enter A List: 443=8443,8080=127.0.0.1:80 (443 Alone Means The Same Port).")
}

func (e forwardPortEditor) setupPorts(raw string) ([]string, error) {
	if strings.EqualFold(strings.TrimSpace(raw), "a") {
		e.creating = true
		ports, saved := e.edit(nil)
		if !saved {
			return nil, fmt.Errorf("forwarded port setup cancelled")
		}
		return ports, nil
	}
	ports := spec.ParsePorts(raw)
	return ports, e.validate(ports)
}

func (e forwardPortEditor) edit(current []string) ([]string, bool) {
	draft := slices.Clone(current)
	for {
		tui.Clear()
		tui.Title("Forwarded Ports")
		e.help()
		e.show(draft)
		if !slices.Equal(current, draft) {
			tui.Warn("Unsaved Changes — Choose Save To Continue.")
		}
		fmt.Println()
		saveLabel := "Save And Apply"
		if e.reviewNext {
			saveLabel = "Review And Apply"
		}
		if e.creating {
			saveLabel = "Save Mappings For Setup"
		}
		for i, label := range []string{"Add Mapping", "Edit Mapping", "Delete Mapping", "Delete All Mappings", saveLabel} {
			fmt.Printf("  %s %s\n", tui.Color(tui.Red, fmt.Sprintf("%d)", i+1)), tui.Color(tui.Bold+tui.White, label))
		}
		tui.Warn("  0) Back Without Saving (Enter Also Goes Back)")
		choice, ok := tui.PromptOrEnd("Choice: ")
		if !ok || choice == "" || choice == "0" || strings.EqualFold(choice, "q") {
			return nil, false
		}
		switch choice {
		case "1", "2":
			index := -1
			if choice == "2" {
				index = selectForwardPort(draft)
				if index < 0 {
					continue
				}
			}
			old := ""
			if index >= 0 {
				old = draft[index]
			}
			entry, ok := e.ask(old)
			if !ok {
				continue
			}
			next := slices.Clone(draft)
			if index < 0 {
				next = append(next, entry)
			} else {
				next[index] = entry
			}
			if err := e.validate(next); err != nil {
				tui.Error(err.Error())
				tui.PressEnter()
				continue
			}
			draft = next
		case "3":
			index := selectForwardPort(draft)
			if index >= 0 && tui.Confirm("Delete Mapping "+draft[index], false) {
				draft = slices.Delete(draft, index, index+1)
			}
		case "4":
			if len(draft) > 0 && tui.Confirm("Delete ALL Forwarded Port Mappings", false) {
				draft = []string{}
			}
		case "5":
			if slices.Equal(current, draft) {
				return nil, false
			}
			if err := e.validate(draft); err != nil {
				tui.Error(err.Error())
				tui.PressEnter()
				continue
			}
			if e.reviewNext {
				return append([]string{}, draft...), true
			}
			confirmation := "Apply These Mappings And Restart The Tunnel"
			if e.creating {
				confirmation = "Use These Mappings For The New Tunnel"
			}
			if tui.Confirm(confirmation, false) {
				// A non-nil empty slice is an explicit clear, distinct from cancel.
				return append([]string{}, draft...), true
			}
		default:
			tui.Error("Choose 1-5, Or 0 To Go Back.")
			tui.PressEnter()
		}
	}
}

func (e forwardPortEditor) help() {
	fmt.Println()
	// These labels name the two machines, not the tunnel's control port.
	tui.Info("SOURCE: This Server (Iran), Where Users Connect")
	tui.Info("TARGET: Service Reached From The Kharej Server")
	tui.Warn("Source And Target Ports Can Differ: 443 -> 8443.")
	tui.Warn("A Target Port Alone Uses " + e.defaultHost + " On The Kharej Side.")
	tui.Warn("The Tunnel/Carrier Port Is Separate From These Forwarded Ports.")
}

func (e forwardPortEditor) show(ports []string) {
	fmt.Println()
	tui.Rule()
	fmt.Printf("      %s\n", tui.Color(tui.Gray, "SOURCE (THIS SERVER)  ->  TARGET (KHAREJ)"))
	if len(ports) == 0 {
		tui.Warn("  No Forwarded Ports.")
	}
	for i, entry := range ports {
		source, target := e.endpoints(entry)
		fmt.Printf("  %s  %s %s %s\n", tui.Color(tui.Red, fmt.Sprintf("%d)", i+1)),
			tui.Color(tui.Bold+tui.White, source), tui.Color(tui.Gray, "->"), tui.Color(tui.White, target))
	}
	tui.Rule()
}

func (e forwardPortEditor) endpoints(entry string) (source, target string) {
	source, target, mapped := strings.Cut(entry, "=")
	source, target = strings.TrimSpace(source), strings.TrimSpace(target)
	if !mapped {
		target = source
		if _, ports, err := net.SplitHostPort(source); err == nil {
			target = ports
		}
	}
	parts := strings.Split(target, "|")
	for i, part := range parts {
		part = strings.TrimSpace(part)
		if !strings.Contains(part, ":") {
			part = net.JoinHostPort(e.defaultHost, part)
		} else if h, p, err := net.SplitHostPort(part); err == nil && h == "" {
			part = net.JoinHostPort(e.defaultHost, p)
		}
		parts[i] = part
	}
	if !strings.Contains(source, ":") {
		source = "*:" + source
	}
	return source, strings.Join(parts, " | ")
}

func selectForwardPort(ports []string) int {
	if len(ports) == 0 {
		tui.Warn("No Mappings To Select.")
		tui.PressEnter()
		return -1
	}
	raw, ok := tui.PromptOrEnd("Mapping Number (Enter/0 = Cancel): ")
	if !ok || raw == "" || raw == "0" {
		return -1
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > len(ports) {
		tui.Error("Choose A Mapping Number From The List.")
		tui.PressEnter()
		return -1
	}
	return n - 1
}

func (e forwardPortEditor) ask(old string) (string, bool) {
	tui.Info("Source: A Port, Range, Or Local IP:Port (e.g. 443 Or 10000-10009).")
	tui.Info("Target: A Port Or Host:Port (e.g. 8443 Or 127.0.0.1:8443).")
	left, right, mapped := strings.Cut(old, "=")
	if !mapped && old != "" {
		right = left
		if _, p, err := net.SplitHostPort(left); err == nil {
			right = p
		}
	}
	read := func(label, previous string) (string, bool) {
		if old == "" {
			label += " (Enter/0 = Cancel)"
		} else {
			label += " [" + previous + "] (Enter = Keep, 0 = Cancel)"
		}
		raw, ok := tui.PromptOrEnd(label + ": ")
		if !ok || raw == "0" || (old == "" && raw == "") {
			return "", false
		}
		if raw == "" {
			raw = previous
		}
		return raw, true
	}
	source, ok := read("Source Port On This Server", strings.TrimSpace(left))
	if !ok {
		return "", false
	}
	target, ok := read("Target Port Or Address On Kharej", strings.TrimSpace(right))
	if !ok {
		return "", false
	}
	if old != "" && source == strings.TrimSpace(left) && target == strings.TrimSpace(right) {
		return old, true
	}
	return source + "=" + target, true
}

func (e forwardPortEditor) validate(ports []string) error {
	if len(ports) == 0 && !e.allowEmpty {
		return fmt.Errorf("this tunnel needs at least one forwarded mapping; add a replacement before saving")
	}
	if e.checkEntry != nil {
		for _, entry := range ports {
			if err := e.checkEntry(entry); err != nil {
				return err
			}
		}
	}
	all := append(slices.Clone(ports), e.reserved...)
	// Reverse transports forward a range to one fixed target or the same ports;
	// only the direct engines support offset destination ranges.
	if e.reverse {
		for _, entry := range ports {
			_, target, mapped := strings.Cut(entry, "=")
			if !mapped {
				continue
			}
			for _, endpoint := range strings.Split(target, "|") {
				p := strings.TrimSpace(endpoint)
				if _, port, err := net.SplitHostPort(p); err == nil {
					p = port
				}
				if !spec.ValidPort(p) {
					return fmt.Errorf("reverse targets need a single port or host:port, not %q", endpoint)
				}
			}
		}
	}
	// Validate each entry separately: reverse ranges may have several backends,
	// whereas the direct engines deliberately refuse that combination.
	var listeners []string
	for _, entry := range all {
		specs := []string{entry}
		if e.reverse {
			left, target, mapped := strings.Cut(entry, "=")
			if mapped && strings.Contains(target, "|") {
				// Validate all backends, then expand the listeners only once.
				for _, backend := range strings.Split(target, "|") {
					if _, err := portmap.Expand([]string{"1=" + backend}, e.defaultHost); err != nil {
						return err
					}
				}
				specs = []string{left}
			}
		}
		mappings, err := portmap.Expand(specs, e.defaultHost)
		if err != nil {
			return err
		}
		for _, m := range mappings {
			for _, address := range append([]string{m.Listen}, m.Targets...) {
				host, _, _ := net.SplitHostPort(address)
				if strings.ContainsAny(host, " \t\r\n,=|/[]") {
					return fmt.Errorf("invalid host in mapping %q", entry)
				}
			}
			for _, other := range listeners {
				if forwardListenersOverlap(m.Listen, other) {
					return fmt.Errorf("source %s overlaps another mapping or the tunnel's reserved listener %s", m.Listen, other)
				}
			}
			listeners = append(listeners, m.Listen)
			if len(listeners) > portmap.MaxExpandedPorts {
				return fmt.Errorf("too many forwarded ports (maximum %d)", portmap.MaxExpandedPorts)
			}
		}
	}
	return nil
}

func forwardListenersOverlap(a, b string) bool {
	ah, ap, _ := net.SplitHostPort(a)
	bh, bp, _ := net.SplitHostPort(b)
	if ap != bp {
		return false
	}
	wildcard := func(host string) bool { return host == "" || host == "0.0.0.0" || host == "::" }
	if wildcard(ah) || wildcard(bh) || strings.EqualFold(ah, bh) {
		return true
	}
	aip, bip := net.ParseIP(ah), net.ParseIP(bh)
	return aip != nil && bip != nil && aip.Equal(bip)
}
