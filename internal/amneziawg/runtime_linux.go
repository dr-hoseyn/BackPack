//go:build linux

package amneziawg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/device"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
	"golang.org/x/sys/unix"
)

func runDevice(ctx context.Context, c Config, out io.Writer) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ip, err := exec.LookPath("ip")
	if err != nil {
		return errors.New("AmneziaWG requires iproute2")
	}
	// IFF_TUN_EXCL refuses an existing interface atomically, including another
	// process racing our startup. Never attach to or configure an existing TUN.
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("AmneziaWG requires Linux TUN and CAP_NET_ADMIN: %w", err)
	}
	file := os.NewFile(uintptr(fd), "/dev/net/tun")
	ifr, err := unix.NewIfreq(c.Interface)
	if err == nil {
		ifr.SetUint16(unix.IFF_TUN | unix.IFF_NO_PI | unix.IFF_VNET_HDR | unix.IFF_TUN_EXCL)
		err = unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr)
	}
	if err != nil {
		file.Close()
		return fmt.Errorf("create exclusive AmneziaWG interface: %w", err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		file.Close()
		return err
	}
	t, err := tun.CreateTUNFromFile(file, c.MTU)
	if err != nil {
		file.Close()
		return fmt.Errorf("initialize AmneziaWG TUN: %w", err)
	}
	// Suppress low-level configuration logs, which are not a health verdict.
	ipc, err := c.ipcConfig()
	if err != nil {
		t.Close()
		return err
	}
	d, err := newConfiguredDevice(t, ipc, &device.Logger{Verbosef: func(string, ...any) {}, Errorf: func(string, ...any) {}})
	if err != nil {
		return err
	}
	defer d.Close()

	for _, args := range [][]string{
		{"address", "add", c.Address, "dev", c.Interface},
		{"link", "set", "dev", c.Interface, "mtu", strconv.Itoa(c.MTU), "up"},
		{"route", "add", c.PeerAddress, "dev", c.Interface},
	} {
		cmdctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := exec.CommandContext(cmdctx, ip, args...).Run()
		cancel()
		if err != nil {
			return fmt.Errorf("configure AmneziaWG interface (existing routes are never replaced): %w", err)
		}
	}
	if err := d.Up(); err != nil {
		return fmt.Errorf("start AmneziaWG UDP endpoint: %w", err)
	}
	fmt.Fprintf(out, "AmneziaWG %s · experimental · interface %s ready\nLocal %s ↔ peer %s · UDP required · handshake not yet verified\n", Version, c.Interface, c.Address, c.PeerAddress)
	select {
	case <-ctx.Done():
		return nil
	case <-d.Wait():
		return errors.New("AmneziaWG device stopped unexpectedly")
	}
}
