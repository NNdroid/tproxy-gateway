//go:build linux
// +build linux

package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

func setupAutoRouteNetlink() error {
	if !cfg.Routing.AutoRoute {
		return nil
	}

	parsed, err := cfg.FakeIP.ParseCIDRs()
	if err != nil {
		return fmt.Errorf("failed to parse FakeIP CIDRs for AutoRoute: %v", err)
	}

	zap.S().Infof("[AutoRoute] Initializing adaptive Go Netlink sockets (v4 CIDRs: %d, v6 CIDRs: %d) -> Table: %d | Fwmark: %d | NftTable: %s",
		len(parsed.V4Prefixes), len(parsed.V6Prefixes), cfg.Routing.Table, cfg.Routing.Fwmark, cfg.Routing.NftTable)

	// Enable kernel IP forwarding for LAN devices
	if len(parsed.V4Prefixes) > 0 {
		_ = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644)
	}
	if len(parsed.V6Prefixes) > 0 {
		_ = os.WriteFile("/proc/sys/net/ipv6/conf/all/forwarding", []byte("1"), 0644)
	}

	loLink, err := netlink.LinkByName("lo")
	if err != nil {
		return fmt.Errorf("failed to get loopback interface: %v", err)
	}

	// 1. IPv4 Policy Rule & Local Route (Only if IPv4 CIDRs configured)
	if len(parsed.V4Prefixes) > 0 {
		rule4 := netlink.NewRule()
		rule4.Mark = uint32(cfg.Routing.Fwmark)
		rule4.Mask = uint32Ptr(uint32(cfg.Routing.Fwmark))
		rule4.Table = cfg.Routing.Table
		rule4.Family = netlink.FAMILY_V4
		if err := netlink.RuleAdd(rule4); err != nil && !isExistErr(err) {
			zap.S().Warnf("[AutoRoute] IPv4 RuleAdd netlink warning: %v", err)
		}

		route4 := &netlink.Route{
			LinkIndex: loLink.Attrs().Index,
			Table:     cfg.Routing.Table,
			Type:      unix.RTN_LOCAL,
			Scope:     netlink.SCOPE_HOST,
			Family:    netlink.FAMILY_V4,
			Dst:       &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
		}
		if err := netlink.RouteAdd(route4); err != nil && !isExistErr(err) {
			zap.S().Warnf("[AutoRoute] IPv4 RouteAdd netlink warning: %v", err)
		}
	}

	// 2. IPv6 Policy Rule & Local Route (Only if IPv6 CIDRs configured)
	if len(parsed.V6Prefixes) > 0 {
		rule6 := netlink.NewRule()
		rule6.Mark = uint32(cfg.Routing.Fwmark)
		rule6.Mask = uint32Ptr(uint32(cfg.Routing.Fwmark))
		rule6.Table = cfg.Routing.Table
		rule6.Family = netlink.FAMILY_V6
		if err := netlink.RuleAdd(rule6); err != nil && !isExistErr(err) {
			zap.S().Warnf("[AutoRoute] IPv6 RuleAdd netlink warning: %v", err)
		}

		route6 := &netlink.Route{
			LinkIndex: loLink.Attrs().Index,
			Table:     cfg.Routing.Table,
			Type:      unix.RTN_LOCAL,
			Scope:     netlink.SCOPE_HOST,
			Family:    netlink.FAMILY_V6,
			Dst:       &net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)},
		}
		if err := netlink.RouteAdd(route6); err != nil && !isExistErr(err) {
			zap.S().Warnf("[AutoRoute] IPv6 RouteAdd netlink warning: %v", err)
		}
	}

	zap.S().Infof("[AutoRoute] Native Go Netlink policy rules & routes injected successfully.")

	// 3. Native Netlink Nftables Setup (PREROUTING for LAN + OUTPUT for Gateway Host)
	if err := setupNftablesNetlink(); err != nil {
		zap.S().Errorf("[AutoRoute] Native Netlink Nftables setup error: %v", err)
		return err
	}

	return nil
}

func cleanupAutoRouteNetlink() {
	if !cfg.Routing.AutoRoute {
		return
	}
	zap.S().Infof("[AutoRoute] Cleaning up policy routes and nftables via Netlink sockets...")

	loLink, err := netlink.LinkByName("lo")
	if err == nil {
		rule4 := netlink.NewRule()
		rule4.Mark = uint32(cfg.Routing.Fwmark)
		rule4.Mask = uint32Ptr(uint32(cfg.Routing.Fwmark))
		rule4.Table = cfg.Routing.Table
		rule4.Family = netlink.FAMILY_V4
		_ = netlink.RuleDel(rule4)

		route4 := &netlink.Route{
			LinkIndex: loLink.Attrs().Index,
			Table:     cfg.Routing.Table,
			Type:      unix.RTN_LOCAL,
			Scope:     netlink.SCOPE_HOST,
			Family:    netlink.FAMILY_V4,
			Dst:       &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
		}
		_ = netlink.RouteDel(route4)

		rule6 := netlink.NewRule()
		rule6.Mark = uint32(cfg.Routing.Fwmark)
		rule6.Mask = uint32Ptr(uint32(cfg.Routing.Fwmark))
		rule6.Table = cfg.Routing.Table
		rule6.Family = netlink.FAMILY_V6
		_ = netlink.RuleDel(rule6)

		route6 := &netlink.Route{
			LinkIndex: loLink.Attrs().Index,
			Table:     cfg.Routing.Table,
			Type:      unix.RTN_LOCAL,
			Scope:     netlink.SCOPE_HOST,
			Family:    netlink.FAMILY_V6,
			Dst:       &net.IPNet{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)},
		}
		_ = netlink.RouteDel(route6)
	}

	// Netlink delete nftables table
	c, err := nftables.New()
	if err == nil {
		table := &nftables.Table{
			Name:   cfg.Routing.NftTable,
			Family: nftables.TableFamilyINet,
		}
		c.DelTable(table)
		_ = c.Flush()
	}

	zap.S().Infof("[AutoRoute] Netlink sockets cleaned up all firewall tables and routes successfully.")
}

func setupNftablesNetlink() error {
	c, err := nftables.New()
	if err != nil {
		return fmt.Errorf("failed to open Netlink Nftables socket: %v", err)
	}

	table := c.AddTable(&nftables.Table{
		Name:   cfg.Routing.NftTable,
		Family: nftables.TableFamilyINet,
	})

	// 1. Prerouting Chain for External LAN Devices
	preroutingChain := c.AddChain(&nftables.Chain{
		Name:     "prerouting",
		Table:    table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookPrerouting,
		Priority: nftables.ChainPriorityMangle,
	})

	// 2. Output Chain for Local Gateway Host Processes
	outputChain := c.AddChain(&nftables.Chain{
		Name:     "output",
		Table:    table,
		Type:     nftables.ChainTypeRoute,
		Hooknum:  nftables.ChainHookOutput,
		Priority: nftables.ChainPriorityMangle,
	})

	_, tproxyPortStr, err := net.SplitHostPort(cfg.Server.TProxyAddr)
	if err != nil {
		idx := strings.LastIndex(cfg.Server.TProxyAddr, ":")
		if idx != -1 {
			tproxyPortStr = cfg.Server.TProxyAddr[idx+1:]
		} else {
			tproxyPortStr = "10800"
		}
	}
	tproxyPort, _ := strconv.Atoi(tproxyPortStr)

	for _, cidr := range cfg.FakeIP.CIDRs {
		prefix, parseErr := netip.ParsePrefix(cidr)
		if parseErr != nil {
			continue
		}

		addr := prefix.Addr()
		if addr.Is4() {
			ipNet := net.IPNet{IP: addr.AsSlice(), Mask: net.CIDRMask(prefix.Bits(), 32)}

			// Prerouting TProxy Rule (External LAN Devices -> TProxy)
			c.AddRule(&nftables.Rule{
				Table: table,
				Chain: preroutingChain,
				Exprs: []expr.Any{
					&expr.Payload{
						DestRegister: 1,
						Base:         expr.PayloadBaseNetworkHeader,
						Offset:       16, // IPv4 Daddr offset
						Len:          4,
					},
					&expr.Bitwise{
						SourceRegister: 1,
						DestRegister:   1,
						Len:            4,
						Mask:           ipNet.Mask,
						Xor:            make([]byte, 4),
					},
					&expr.Cmp{
						Op:       expr.CmpOpEq,
						Register: 1,
						Data:     ipNet.IP.To4(),
					},
					&expr.Immediate{
						Register: 1,
						Data:     encodeUint32(uint32(cfg.Routing.Fwmark)),
					},
					&expr.Meta{
						Key:            expr.MetaKeyMARK,
						Register:       1,
						SourceRegister: true,
					},
					&expr.Immediate{
						Register: 1,
						Data:     net.ParseIP("127.0.0.1").To4(),
					},
					&expr.Immediate{
						Register: 2,
						Data:     encodeUint16(uint16(tproxyPort)),
					},
					&expr.TProxy{
						Family:      unix.NFPROTO_IPV4,
						TableFamily: unix.NFPROTO_INET,
						RegAddr:     1,
						RegPort:     2,
					},
					&expr.Verdict{
						Kind: expr.VerdictAccept,
					},
				},
			})

			// Output Mark Rule (Local Machine Processes -> Mark fwmark -> Route to lo -> Hit Prerouting)
			c.AddRule(&nftables.Rule{
				Table: table,
				Chain: outputChain,
				Exprs: []expr.Any{
					&expr.Payload{
						DestRegister: 1,
						Base:         expr.PayloadBaseNetworkHeader,
						Offset:       16, // IPv4 Daddr offset
						Len:          4,
					},
					&expr.Bitwise{
						SourceRegister: 1,
						DestRegister:   1,
						Len:            4,
						Mask:           ipNet.Mask,
						Xor:            make([]byte, 4),
					},
					&expr.Cmp{
						Op:       expr.CmpOpEq,
						Register: 1,
						Data:     ipNet.IP.To4(),
					},
					&expr.Immediate{
						Register: 1,
						Data:     encodeUint32(uint32(cfg.Routing.Fwmark)),
					},
					&expr.Meta{
						Key:            expr.MetaKeyMARK,
						Register:       1,
						SourceRegister: true,
					},
					&expr.Verdict{
						Kind: expr.VerdictAccept,
					},
				},
			})
		} else if addr.Is6() {
			ipNet := net.IPNet{IP: addr.AsSlice(), Mask: net.CIDRMask(prefix.Bits(), 128)}

			// Prerouting TProxy Rule (IPv6 LAN Devices -> TProxy)
			c.AddRule(&nftables.Rule{
				Table: table,
				Chain: preroutingChain,
				Exprs: []expr.Any{
					&expr.Payload{
						DestRegister: 1,
						Base:         expr.PayloadBaseNetworkHeader,
						Offset:       24, // IPv6 Daddr offset
						Len:          16,
					},
					&expr.Bitwise{
						SourceRegister: 1,
						DestRegister:   1,
						Len:            16,
						Mask:           ipNet.Mask,
						Xor:            make([]byte, 16),
					},
					&expr.Cmp{
						Op:       expr.CmpOpEq,
						Register: 1,
						Data:     ipNet.IP.To16(),
					},
					&expr.Immediate{
						Register: 1,
						Data:     encodeUint32(uint32(cfg.Routing.Fwmark)),
					},
					&expr.Meta{
						Key:            expr.MetaKeyMARK,
						Register:       1,
						SourceRegister: true,
					},
					&expr.Immediate{
						Register: 1,
						Data:     net.ParseIP("::1").To16(),
					},
					&expr.Immediate{
						Register: 2,
						Data:     encodeUint16(uint16(tproxyPort)),
					},
					&expr.TProxy{
						Family:      unix.NFPROTO_IPV6,
						TableFamily: unix.NFPROTO_INET,
						RegAddr:     1,
						RegPort:     2,
					},
					&expr.Verdict{
						Kind: expr.VerdictAccept,
					},
				},
			})

			// Output Mark Rule (IPv6 Local Gateway Host Processes -> Mark fwmark -> Route to lo -> Hit Prerouting)
			c.AddRule(&nftables.Rule{
				Table: table,
				Chain: outputChain,
				Exprs: []expr.Any{
					&expr.Payload{
						DestRegister: 1,
						Base:         expr.PayloadBaseNetworkHeader,
						Offset:       24, // IPv6 Daddr offset
						Len:          16,
					},
					&expr.Bitwise{
						SourceRegister: 1,
						DestRegister:   1,
						Len:            16,
						Mask:           ipNet.Mask,
						Xor:            make([]byte, 16),
					},
					&expr.Cmp{
						Op:       expr.CmpOpEq,
						Register: 1,
						Data:     ipNet.IP.To16(),
					},
					&expr.Immediate{
						Register: 1,
						Data:     encodeUint32(uint32(cfg.Routing.Fwmark)),
					},
					&expr.Meta{
						Key:            expr.MetaKeyMARK,
						Register:       1,
						SourceRegister: true,
					},
					&expr.Verdict{
						Kind: expr.VerdictAccept,
					},
				},
			})
		}
	}

	if err := c.Flush(); err != nil {
		return fmt.Errorf("failed to flush Netlink nftables rules: %v", err)
	}

	zap.S().Infof("[AutoRoute] Netlink Nftables PREROUTING and OUTPUT chains constructed via AF_NETLINK socket successfully.")
	return nil
}

func uint32Ptr(v uint32) *uint32 {
	return &v
}

func encodeUint32(v uint32) []byte {
	b := make([]byte, 4)
	binary.NativeEndian.PutUint32(b, v)
	return b
}

func encodeUint16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

func isExistErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "file exists") || strings.Contains(err.Error(), "exist")
}
