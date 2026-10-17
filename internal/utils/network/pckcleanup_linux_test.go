package network

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// Run only in the CI network namespace: this exercises real firewall rules,
// including cleanup after switching away from PCK without reopening a carrier.
func TestIPv6PckFirewallCleanupPreservesLiveOwners(t *testing.T) {
	if os.Getenv("BP_L3_LIVE") != "1" || os.Geteuid() != 0 {
		t.Skip("requires the isolated root network namespace")
	}
	token := fmt.Sprintf("cleanup-%d", os.Getpid())
	for _, v6 := range []bool{false, true} {
		tool, familyToken := "iptables", token
		if v6 {
			tool, familyToken = "ip6tables", "v6-"+token
		}
		id := pckTunnelID(familyToken)
		rules := pckRules(id, 39761, 39761)
		g, err := installPckGuard(id, nil, rules, v6)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(g.remove)
		if !g.Installed() {
			t.Fatalf("%s guard not installed", tool)
		}
		check := func(want bool) {
			t.Helper()
			for _, table := range []string{"filter", "raw"} {
				out, err := pckFirewall(tool, "-t", table, "-S")
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(out), pckRulePrefix(id)) != want {
					t.Fatalf("%s %s: expected presence=%v, got %s", tool, table, want, out)
				}
			}
		}
		CleanupPckGuards(token)
		check(true)
		g.remove()
		check(false)
		// Recreate rules without an owner, as left by a killed process.
		for _, r := range rules {
			if _, err := pckFirewall(tool, append([]string{"-t", r[0], "-I"}, r[1:]...)...); err != nil {
				t.Fatal(err)
			}
		}
		CleanupPckGuards(token)
		check(false)
		for _, r := range rules {
			if _, err := pckFirewall(tool, append([]string{"-t", r[0], "-I"}, r[1:]...)...); err != nil {
				t.Fatal(err)
			}
		}
		CleanupPckGuards()
		check(false)
	}
}
