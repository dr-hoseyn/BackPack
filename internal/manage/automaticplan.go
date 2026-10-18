package manage

import (
	"context"
	"errors"
	"fmt"
	"github.com/backpack/backpack/internal/app"
	"net"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

type AutomaticRequest struct {
	Carriers   string `json:"carriers"`
	DERPMapURL string `json:"derpMapURL"`
	Name       string `json:"name"`
	Host       string `json:"host"`
	Ports      string `json:"ports"`
	Profile    string `json:"profile"`
	Cover      string `json:"cover"`
}

var automaticDNSName = regexp.MustCompile(`(?i)^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*\.?$`)

type AutomaticListener struct {
	Protocol string `json:"protocol"`
	Network  string `json:"network"`
	Port     string `json:"port"`
}
type AutomaticPreview struct {
	Name      string              `json:"name"`
	Host      string              `json:"host"`
	Profile   string              `json:"profile"`
	Ports     []string            `json:"ports"`
	Listeners []AutomaticListener `json:"listeners"`
	Warnings  []string            `json:"warnings"`
	Expires   time.Time           `json:"expires"`
}
type AutomaticPlan struct {
	Preview                    AutomaticPreview
	private, cover, derpMapURL string
	carriers, endpoints        []string
	mu                         sync.Mutex
	closed                     bool
	reservations               []func()
	timer                      *time.Timer
}

func (p *AutomaticPlan) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if p.timer != nil {
		p.timer.Stop()
	}
	for _, close := range p.reservations {
		close()
	}
	p.reservations = nil
}

// PrepareAutomatic reserves listeners but creates no files or services. It
// returns only public intent; credentials and certificates are made on commit.
func PrepareAutomatic(ctx context.Context, r AutomaticRequest) (*AutomaticPlan, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("Automatic requires Linux")
	}
	if r.Profile == "" {
		r.Profile = "economy"
	}
	if r.Profile != "economy" && r.Profile != "full" {
		return nil, errors.New("profile must be economy or full")
	}
	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" {
		r.Name = freeName("automatic")
	}
	if !validName(r.Name) || fileExists(app.ConfigPath(r.Name)) {
		return nil, errors.New("choose an unused valid tunnel name")
	}
	ports := parsePorts(r.Ports)
	if len(ports) == 0 {
		return nil, errors.New("at least one forwarded port is required")
	}
	if err := validatePortSpecs(ports); err != nil {
		return nil, err
	}
	if busy := reverseBusyPorts(ports, "", "tcp"); len(busy) > 0 {
		return nil, fmt.Errorf("forwarded TCP ports already in use: %s", strings.Join(busy, ", "))
	}
	r.Host = strings.TrimSpace(r.Host)
	if r.Host == "" {
		r.Host = PublicIPv4()
	}
	if r.Host == "" || r.Host == "-" || strings.ContainsAny(r.Host, "/ \t\r\n") {
		return nil, errors.New("a public IPv4 or DNS hostname is required")
	}
	if net.ParseIP(r.Host) == nil && (len(r.Host) > 253 || !automaticDNSName.MatchString(r.Host)) {
		return nil, errors.New("host must be a valid public IPv4 or DNS hostname")
	}
	if ip := net.ParseIP(r.Host); ip != nil && ip.To4() == nil {
		return nil, errors.New("Automatic public listeners currently require IPv4")
	}
	sing, xray := automaticExecutable(managedHelperPath("sing-box")), automaticExecutable(managedHelperPath("xray"))
	cover := ""
	if r.Cover != "" {
		if !xray {
			return nil, errors.New("cover verification requires the pinned Xray helper")
		}
		results := scanRealityCovers(ctx, []string{r.Cover}, managedHelperPath("xray"), probeRealitySetup)
		for _, v := range results {
			if v.err == nil {
				cover = v.target
			}
		}
		if cover == "" {
			return nil, errors.New("the requested cover did not pass authenticated verification")
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	carriers := automaticCarriers(sing, xray, automaticExecutable(managedHelperPath("naive")), cover)
	if r.Carriers != "" {
		var err error
		carriers, err = customAutomaticCarriers(r.Carriers, r.DERPMapURL, cover, sing, xray, automaticExecutable(managedHelperPath("naive")))
		if err != nil {
			return nil, err
		}
	} else {
		carriers = modernAutomaticCarriers(carriers)
	}
	if r.Profile == "economy" && r.Carriers == "" {
		carriers = economicalCarriers(carriers)
	}
	if r.Profile == "economy" && len(carriers) > 3 {
		return nil, errors.New("Economy accepts at most three explicit carriers; select Full to combine more")
	}
	if len(carriers) < 2 {
		return nil, errors.New("install pinned sing-box or Xray; Automatic needs at least two compatible carriers")
	}
	nativeUDP := false
	for _, carrier := range carriers {
		nativeUDP = nativeUDP || carrier == "hysteria2" || carrier == "tuic"
	}
	if nativeUDP {
		if busy := automaticUDPConflict(ports); busy != "" {
			return nil, fmt.Errorf("forwarded UDP port already in use: %s", busy)
		}
	}
	p := &AutomaticPlan{Preview: AutomaticPreview{Name: r.Name, Host: r.Host, Ports: ports, Profile: r.Profile, Expires: time.Now().Add(2 * time.Minute)}, cover: cover, carriers: carriers, derpMapURL: r.DERPMapURL}
	used := map[int]bool{}
	for _, port := range ReservedPorts() {
		used[port] = true
	}
	reserve := func(bind string) (string, error) {
		addr, close, err := reserveAutomaticPort(bind, ports, used)
		if err == nil {
			p.reservations = append(p.reservations, close)
		}
		return addr, err
	}
	var err error
	p.private, err = reserve("127.0.0.1")
	if err != nil {
		p.Close()
		return nil, err
	}
	for _, carrier := range carriers {
		addr, err := reserve("0.0.0.0")
		if err != nil {
			p.Close()
			return nil, err
		}
		p.endpoints = append(p.endpoints, addr)
		network := "TCP"
		if carrier == "trusttunnel" {
			network = "TCP+UDP"
		}
		if carrier == "webtransport" {
			network = "UDP"
		}
		if carrier == "tailcat" {
			network = "DERP (no public listener)"
			p.Preview.Warnings = append(p.Preview.Warnings, "Tailcat is experimental and uses the explicitly selected DERP map; install its separate helper on both peers.")
		}
		if carrier == "hysteria2" || carrier == "tuic" {
			network = "UDP"
		}
		p.Preview.Listeners = append(p.Preview.Listeners, AutomaticListener{carrier, network, addrPort(addr)})
	}
	if cover == "" {
		p.Preview.Warnings = append(p.Preview.Warnings, "REALITY and ShadowTLS omitted: supply a cover endpoint to verify them.")
	}
	p.Preview.Warnings = append(p.Preview.Warnings, "Allow the listed inbound ports before pairing. Firewall rules are not changed.")
	p.timer = time.AfterFunc(time.Until(p.Preview.Expires), p.Close)
	return p, nil
}

func (p *AutomaticPlan) Commit() (name, service, link string, active bool, err error) {
	p.mu.Lock()
	if p.closed || time.Now().After(p.Preview.Expires) {
		p.mu.Unlock()
		return "", "", "", false, errors.New("preview expired; prepare it again")
	}
	p.closed = true
	p.timer.Stop()
	p.mu.Unlock()
	defer p.Close()
	name = p.Preview.Name
	if _, e := os.Stat(app.ConfigPath(name)); !os.IsNotExist(e) {
		return name, "", "", false, errors.New("tunnel name is no longer available")
	}
	xray := false
	for _, c := range p.carriers {
		xray = xray || c == "xhttp"
	}
	material, e := automaticCertificates(name, p.Preview.Host, xray)
	if e != nil {
		err = e
		return
	}
	s, e := buildAutomaticSpec(name, p.Preview.Host, p.private, material, p.cover, p.carriers, p.endpoints, p.Preview.Ports)
	if e != nil {
		err = e
		return
	}
	for i := range s.ServerPaths {
		if s.ServerPaths[i].Sing.Protocol == "tailcat" {
			s.ServerPaths[i].Sing.Tailcat.DERPMapURL = p.derpMapURL
		}
	}
	s.PathProfile = p.Preview.Profile
	if err = validateManagedSpec(s); err != nil {
		return
	}
	link = pendingReverseLink(s, p.Preview.Host, linkExtras{})
	if link == "" {
		err = errors.New("could not generate the paired setup link")
		return
	}
	if err = refuseBusyForwards(s); err != nil {
		return
	}
	// Helpers cannot inherit these sockets; release immediately before Save.
	p.Close()
	service, err = s.Save()
	if service != "" {
		active = IsActive(service)
	}
	return
}
