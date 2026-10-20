package optimize

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// HighConcurrencyRange leaves the low service-port band alone while nearly
// doubling the number of connections one source IP can make to one backend.
const HighConcurrencyRange = "10240 65535"

var (
	capacityFile   = "/etc/sysctl.d/zzzz-backpack-capacity.conf"
	capacityBackup = "/etc/backpack/high-concurrency-backup.json"
	procNetDir     = "/proc/net"
	procIPv4Dir    = "/proc/sys/net/ipv4"
	capacitySysctl = func(key, value string) error {
		out, err := exec.Command("sysctl", "-w", key+"="+value).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %w: %s", key, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
)

const capacityHeader = "# Managed by backpack high-concurrency; restore with backpack tune restore\n"

type capacityState struct {
	Range    string `json:"range"`
	Reserved string `json:"reserved"`
}

// ParseReservedPorts validates Linux's comma-separated reservation syntax.
// Reject the whole value on errors: silently dropping one reservation could
// give a panel's service port to an outbound connection.
func ParseReservedPorts(value string) ([]int, error) {
	var ports []int
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	for _, part := range strings.Split(value, ",") {
		ends := strings.Split(strings.TrimSpace(part), "-")
		if len(ends) > 2 {
			return nil, fmt.Errorf("invalid reserved port %q", part)
		}
		lo, err := strconv.Atoi(ends[0])
		if err != nil || lo < 1 || lo > 65535 {
			return nil, fmt.Errorf("invalid reserved port %q", part)
		}
		hi := lo
		if len(ends) == 2 {
			hi, err = strconv.Atoi(ends[1])
			if err != nil || hi < lo || hi > 65535 {
				return nil, fmt.Errorf("invalid reserved port range %q", part)
			}
		}
		for p := lo; p <= hi; p++ {
			ports = append(ports, p)
		}
	}
	return ports, nil
}

// TCP LISTEN sockets include local backends and unrelated panels. Connected
// sockets must not become permanent reservations. UDP's UNCONN state also
// includes transient proxy sockets, so fixed UDP ports must be supplied by the
// configured tunnel list or --reserve-ports.
func tcpListenPorts(data string) ([]int, error) {
	var ports []int
	for _, line := range strings.Split(data, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] == "sl" {
			continue
		}
		if len(f) < 4 {
			return nil, fmt.Errorf("malformed TCP socket table")
		}
		if f[3] != "0A" {
			continue
		}
		_, hexPort, ok := strings.Cut(f[1], ":")
		p, err := strconv.ParseUint(hexPort, 16, 16)
		if !ok || err != nil || p == 0 {
			return nil, fmt.Errorf("invalid TCP listener %q", f[1])
		}
		ports = append(ports, int(p))
	}
	return ports, nil
}

func currentCapacity() (capacityState, error) {
	var state capacityState
	for key, target := range map[string]*string{"ip_local_port_range": &state.Range, "ip_local_reserved_ports": &state.Reserved} {
		b, err := os.ReadFile(filepath.Join(procIPv4Dir, key))
		if err != nil {
			return state, err
		}
		*target = strings.TrimSpace(string(b))
	}
	return state, nil
}

func capacityReservations(extra []int) (string, error) {
	s, err := currentCapacity()
	if err != nil {
		return "", err
	}
	ports, err := ParseReservedPorts(s.Reserved)
	if err != nil {
		return "", err
	}
	ports = append(ports, extra...)
	for _, name := range []string{"tcp", "tcp6"} {
		b, err := os.ReadFile(filepath.Join(procNetDir, name))
		if name == "tcp6" && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("read listeners: %w", err)
		}
		listeners, err := tcpListenPorts(string(b))
		if err != nil {
			return "", err
		}
		ports = append(ports, listeners...)
	}
	return reservedList(ports), nil
}

func capacityConfig(reserved string) []byte {
	// Reservations must precede the wider range both at boot and live.
	return []byte(capacityHeader + "net.ipv4.ip_local_reserved_ports = " + reserved + "\nnet.ipv4.ip_local_port_range = " + HighConcurrencyRange + "\n")
}

// Keep the ordinary optimizer's boot file at the pre-profile port policy.
// Otherwise running Optimize once would copy the wider range into a second
// file, and tune restore would appear to work until the next reboot.
func capacityTuningRows(reserve []int) (live, persisted [][2]string, err error) {
	rows := append([][2]string(nil), sysctls...)
	if !HighConcurrencyEnabled() {
		if list := reservedList(reserve); list != "" {
			rows = append([][2]string{{"net.ipv4.ip_local_reserved_ports", list}}, rows...)
		}
		return rows, rows, nil
	}
	baseline, err := readCapacityBackup()
	var merged string
	if err == nil {
		merged, err = capacityReservations(reserve)
	}
	if err == nil {
		err = atomicCapacityWrite(capacityFile, capacityConfig(merged), 0644)
	}
	if err != nil {
		// Leave both port sysctls alone if discovery or persistence fails.
		var safe [][2]string
		for _, row := range rows {
			if row[0] != "net.ipv4.ip_local_port_range" {
				safe = append(safe, row)
			}
		}
		return safe, safe, err
	}
	live = append([][2]string{{"net.ipv4.ip_local_reserved_ports", merged}}, rows...)
	persisted = append([][2]string{{"net.ipv4.ip_local_reserved_ports", baseline.Reserved}}, rows...)
	for i := range live {
		if live[i][0] == "net.ipv4.ip_local_port_range" {
			live[i][1] = HighConcurrencyRange
			persisted[i][1] = baseline.Range
		}
	}
	return live, persisted, nil
}

func atomicCapacityWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".backpack-capacity-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// HighConcurrencyEnabled is opt-in state, separate from the normal optimizer.
func HighConcurrencyEnabled() bool {
	b, err := os.ReadFile(capacityFile)
	return err == nil && strings.HasPrefix(string(b), capacityHeader)
}

// EnableHighConcurrency changes no tunnel, route or restart schedule. It takes
// effect for new connections without restarting the engine. Repeated calls add
// reservations and retain the original rollback snapshot.
func EnableHighConcurrency(extra []int) (string, error) {
	if runtime.GOOS != "linux" {
		return "", fmt.Errorf("high-concurrency tuning requires Linux")
	}
	return enableHighConcurrency(extra)
}

func enableHighConcurrency(extra []int) (string, error) {
	before, err := currentCapacity()
	if err != nil {
		return "", err
	}
	reserved, err := capacityReservations(extra)
	if err != nil {
		return "", err
	}
	previous, readErr := os.ReadFile(capacityFile)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return "", readErr
	}
	if readErr == nil && !strings.HasPrefix(string(previous), capacityHeader) {
		return "", fmt.Errorf("refusing to overwrite unmanaged %s", capacityFile)
	}
	_, backupErr := os.Stat(capacityBackup)
	first := errors.Is(backupErr, os.ErrNotExist)
	if backupErr != nil && !first {
		return "", backupErr
	}
	if first {
		if readErr == nil {
			return "", fmt.Errorf("missing rollback snapshot %s", capacityBackup)
		}
		b, _ := json.Marshal(before)
		if err := atomicCapacityWrite(capacityBackup, b, 0600); err != nil {
			return "", err
		}
	} else {
		if _, err := readCapacityBackup(); err != nil {
			return "", err
		}
	}
	rollback := func(cause error) (string, error) {
		// Shrink first, then release reservations, to protect service ports.
		errRange := capacitySysctl("net.ipv4.ip_local_port_range", before.Range)
		var errReserved error
		if errRange == nil {
			errReserved = capacitySysctl("net.ipv4.ip_local_reserved_ports", before.Reserved)
		}
		var errFile error
		if readErr == nil {
			errFile = atomicCapacityWrite(capacityFile, previous, 0644)
		} else {
			errFile = os.Remove(capacityFile)
			if errors.Is(errFile, os.ErrNotExist) {
				errFile = nil
			}
		}
		if first && errRange == nil && errReserved == nil && errFile == nil {
			_ = os.Remove(capacityBackup)
		}
		return "", errors.Join(cause, errRange, errReserved, errFile)
	}
	if err := capacitySysctl("net.ipv4.ip_local_reserved_ports", reserved); err != nil {
		return rollback(err)
	}
	if err := capacitySysctl("net.ipv4.ip_local_port_range", HighConcurrencyRange); err != nil {
		return rollback(err)
	}
	if err := atomicCapacityWrite(capacityFile, capacityConfig(reserved), 0644); err != nil {
		return rollback(err)
	}
	return reserved, nil
}

func readCapacityBackup() (capacityState, error) {
	var state capacityState
	b, err := os.ReadFile(capacityBackup)
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(b, &state); err != nil {
		return state, err
	}
	f := strings.Fields(state.Range)
	if len(f) != 2 {
		return state, fmt.Errorf("invalid saved port range")
	}
	lo, e1 := strconv.Atoi(f[0])
	hi, e2 := strconv.Atoi(f[1])
	if e1 != nil || e2 != nil || lo < 1 || hi > 65535 || lo > hi {
		return state, fmt.Errorf("invalid saved port range")
	}
	_, err = ParseReservedPorts(state.Reserved)
	return state, err
}

// RestoreHighConcurrency restores the original two sysctls. The backup remains
// until all steps succeed, so a failed restore can be retried.
func RestoreHighConcurrency() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("high-concurrency tuning requires Linux")
	}
	return restoreHighConcurrency()
}

func restoreHighConcurrency() error {
	state, err := readCapacityBackup()
	if err != nil {
		return err
	}
	b, readErr := os.ReadFile(capacityFile)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if readErr == nil && !strings.HasPrefix(string(b), capacityHeader) {
		return fmt.Errorf("refusing to remove unmanaged %s", capacityFile)
	}
	if err := capacitySysctl("net.ipv4.ip_local_port_range", state.Range); err != nil {
		return err
	}
	if err := capacitySysctl("net.ipv4.ip_local_reserved_ports", state.Reserved); err != nil {
		return err
	}
	if err := os.Remove(capacityFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Remove(capacityBackup)
}
