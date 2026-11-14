// Package device assembles the userspace WireGuard stack that the wg-user
// SOCKS5 routines run on: a netstack TUN, a wireguard-go device, and the
// wireproxy VirtualTun that ties them to the parsed configuration.
//
// Create is derived from wireproxy v1.1.3 (ISC licensed) wireguard.go,
// function StartWireguard. The single difference: the conn.Bind is a
// parameter instead of being hard-coded to conn.NewDefaultBind(), so the same
// stack can run over the kernel's UDP sockets (Mode = off) or over the
// wstunnel WebSocket (Mode = wstunnel).
package device

import (
	"fmt"
	"sync"

	"github.com/windtf/wireproxy"
	"golang.zx2c4.com/wireguard/conn"
	wgdevice "golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// Create builds a netstack TUN from conf and brings a wireguard-go device up
// on it, using bind for the WireGuard datagrams. logLevel is a wireguard-go
// device.LogLevel* constant; the logger writes under the "wg-user: " prefix.
func Create(conf *wireproxy.Configuration, bind conn.Bind, logLevel int) (*wireproxy.VirtualTun, error) {
	deviceConf := conf.Device

	setting, err := wireproxy.CreateIPCRequest(deviceConf)
	if err != nil {
		return nil, fmt.Errorf("device: create IPC request: %w", err)
	}

	tun, tnet, err := netstack.CreateNetTUN(setting.DeviceAddr, setting.DNS, setting.MTU)
	if err != nil {
		return nil, fmt.Errorf("device: create netstack TUN: %w", err)
	}

	// From here on the device owns the TUN, so dev.Close is the only cleanup
	// path that matters: it tears down the routines, the bind and the TUN.
	dev := wgdevice.NewDevice(tun, bind, wgdevice.NewLogger(logLevel, "wg-user: "))

	if err := dev.IpcSet(setting.IpcRequest); err != nil {
		dev.Close()
		return nil, fmt.Errorf("device: apply IPC request: %w", err)
	}

	// Up runs BindUpdate, which closes the bind before opening it. A bind that
	// survives that cycle (wgbind does) is required; nothing is skipped here.
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("device: bring device up: %w", err)
	}

	// wireproxy resolves through the TUN, and "auto" has to become a concrete
	// strategy before the routines start: a v4-only interface never has a v6
	// source address, so IPv6 would make every lookup fail.
	hasV4 := false
	hasV6 := false
	for _, addr := range setting.DeviceAddr {
		if addr.Is4() {
			hasV4 = true
		}
		if addr.Is6() {
			hasV6 = true
		}
	}

	if conf.Resolve.ResolveStrategy == "auto" {
		if hasV4 && !hasV6 {
			conf.Resolve.ResolveStrategy = "ipv4"
		} else {
			conf.Resolve.ResolveStrategy = "ipv6"
		}
	}

	return &wireproxy.VirtualTun{
		Tnet:           tnet,
		Dev:            dev,
		Conf:           deviceConf,
		ResolveConfig:  conf.Resolve,
		SystemDNS:      len(setting.DNS) == 0,
		PingRecord:     make(map[string]uint64),
		PingRecordLock: new(sync.Mutex),
	}, nil
}
