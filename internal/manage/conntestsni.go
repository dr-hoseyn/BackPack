package manage

import (
	"fmt"
	"io"
	"strings"
)

// These are ClientHello names, not HTTPS reachability checks. Each candidate
// must carry the same real tunnel traffic as every other Connection Test row.
var connTestSNIDomains = []string{"www.speedtest.net", "www.microsoft.com", "www.apple.com"}

type ctDirectCasePlan struct {
	carrier, sni, key string
}

func ctDirectPlans(domain string) []ctDirectCasePlan {
	domain = strings.TrimSpace(domain)
	var plans []ctDirectCasePlan
	for _, carrier := range connTestDirect {
		if carrier != "sni" || domain != "" {
			sni := ""
			if carrier == "sni" {
				sni = domain
			}
			plans = append(plans, ctDirectCasePlan{carrier, sni, carrier})
			continue
		}
		for _, candidate := range connTestSNIDomains {
			plans = append(plans, ctDirectCasePlan{carrier, candidate, "sni@" + candidate})
		}
	}
	return plans
}

func ctSNIVariantKey(key string) bool {
	for _, domain := range connTestSNIDomains {
		if key == "sni@"+domain {
			return true
		}
	}
	return false
}

func ctLinkCaseKey(link ConnTestLink, c ShareLink) string {
	if link.SNIVariants && c.Kind == "direct" && c.Tr == "sni" {
		return "sni@" + c.SNI
	}
	return c.Tr
}

func (c *connTestCase) caseKey() string {
	if c.key != "" {
		return c.key
	}
	return c.tr
}

func (c *connTestCase) initialResult() ConnTestResult {
	r := ConnTestResult{Kind: c.kind, Transport: c.tr, Status: ctTesting, Total: connTestSoak}
	if c.kind == "direct" && c.tr == "sni" {
		r.SNIDomain = c.l3.SNIDomain
		if c.caseKey() != c.tr {
			r.CaseID = c.caseKey()
		}
	}
	return r
}

func ctResultCaseKey(r ConnTestResult) string {
	if r.CaseID != "" {
		return r.CaseID
	}
	return r.Transport
}

// Short numbered labels preserve the table's column widths. The full domains
// are listed below the table and shown directly on the web panel's rows.
func ctResultName(r ConnTestResult) string {
	if ctSNIVariantKey(r.CaseID) {
		for i, domain := range connTestSNIDomains {
			if r.CaseID == "sni@"+domain {
				return fmt.Sprintf("SNI #%d", i+1)
			}
		}
	}
	return ctName(r.Transport)
}

func ctSNINotes(out io.Writer, rows []ConnTestResult) {
	wrote := false
	for _, r := range rows {
		if r.Kind != "direct" || r.Transport != "sni" || r.SNIDomain == "" {
			continue
		}
		if !wrote {
			fmt.Fprintln(out, "\nDirect SNI domains:")
			wrote = true
		}
		fmt.Fprintf(out, "  %-*s %s\n", ctNameWidth, ctResultName(r), r.SNIDomain)
	}
}
