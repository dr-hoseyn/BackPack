package manage

import (
	"github.com/backpack/backpack/internal/tui"
)

func configureShort(n *TunnelSpec, old TunnelSpec, public, host string) bool {
	n.MPTCP = false
	if n.Role == "client" {
		x := old.ShortClient
		x.Server = public
		x.CAFile = tui.PromptDefault("Private CA File (Blank = System Trust)", x.CAFile)
		n.ShortClient = x
		n.ConnectionPool = 8
		return true
	}
	x := old.ShortServer
	x.Listen = public
	x.ServerName = tui.PromptDefault("Verified Certificate Hostname Or IP", defaultString(x.ServerName, host))
	if tui.Confirm("Automatically Obtain And Renew A Public Certificate With ACME (DNS Name, Public TCP 443 Required)", x.ACMEDomain != "") {
		x.ACMEDomain = x.ServerName
		x.ACMEEmail = tui.PromptDefault("ACME Contact Email (Optional)", x.ACMEEmail)
		x.Certificate, x.Key = "", ""
	} else {
		x.ACMEDomain, x.ACMEEmail = "", ""
		var ok bool
		x.Certificate, x.Key, ok = managedCertificate(*n, x.ServerName, x.Certificate, x.Key)
		if !ok {
			return false
		}
	}
	n.ShortServer = x
	return true
}
