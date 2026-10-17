package manage

import (
	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/tui"
)

func configureXHTTPOptions(x *config.XrayClientConfig) {
	if !tui.Confirm("Tune XHTTP Connections Or Use A Separate Download Endpoint", false) {
		return
	}
	x.MaxConcurrency = tui.PromptInt("Maximum Concurrent Requests (0 = Default; Excludes Maximum Connections)", x.MaxConcurrency)
	x.MaxConnections = tui.PromptInt("Maximum Connections (0 = Default; Excludes Maximum Concurrent Requests)", x.MaxConnections)
	x.MaxRequests = tui.PromptInt("Requests Before Connection Renewal (0 = Default)", x.MaxRequests)
	x.MaxReuseSeconds = tui.PromptInt("Connection Reuse Lifetime In Seconds (0 = Default)", x.MaxReuseSeconds)
	if !tui.Confirm("Use A Separate Verified HTTPS Download Endpoint", x.DownloadServer != "") {
		x.DownloadServer, x.DownloadServerName, x.DownloadHost, x.DownloadPath, x.DownloadCAFile = "", "", "", "", ""
		return
	}
	tui.Info("Both Endpoints Must Reach The Same XHTTP Server. Each Endpoint Verifies Its Own TLS Certificate.")
	x.DownloadServer = tui.PromptDefault("Download Server (Host:Port)", x.DownloadServer)
	x.DownloadServerName = tui.PromptDefault("Download TLS Server Name", defaultString(x.DownloadServerName, x.ServerName))
	x.DownloadHost = tui.PromptDefault("Download HTTP Host (Empty = Download TLS Name)", x.DownloadHost)
	x.DownloadPath = tui.PromptDefault("Download Path (Empty = Upload Path)", x.DownloadPath)
	x.DownloadCAFile = tui.PromptDefault("Download CA File (Empty = System Trust)", x.DownloadCAFile)
}
