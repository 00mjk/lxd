package network

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"

	"github.com/pkg/errors"

	lxd "github.com/lxc/lxd/client"
	"github.com/lxc/lxd/lxd/cluster"
	"github.com/lxc/lxd/lxd/daemon"
	"github.com/lxc/lxd/lxd/dnsmasq"
	firewallConsts "github.com/lxc/lxd/lxd/firewall/consts"
	"github.com/lxc/lxd/lxd/instance"
	"github.com/lxc/lxd/lxd/node"
	"github.com/lxc/lxd/lxd/state"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/logger"
	"github.com/lxc/lxd/shared/subprocess"
	"github.com/lxc/lxd/shared/version"
)

// ForkdnsServersListPath defines the path that contains the forkdns server candidate file.
const ForkdnsServersListPath = "forkdns.servers"

// ForkdnsServersListFile file that contains the server candidates list.
const ForkdnsServersListFile = "servers.conf"

var forkdnsServersLock sync.Mutex

// DHCPRange represents a range of IPs from start to end.
type DHCPRange struct {
	Start net.IP
	End   net.IP
}

// Network represents a LXD network.
type Network struct {
	// Properties
	state       *state.State
	id          int64
	name        string
	description string

	// config
	config map[string]string
}

// Name returns the network name.
func (n *Network) Name() string {
	return n.name
}

// Config returns the network config.
func (n *Network) Config() map[string]string {
	return n.config
}

// IsRunning returns whether the network is up.
func (n *Network) IsRunning() bool {
	return shared.PathExists(fmt.Sprintf("/sys/class/net/%s", n.name))
}

// IsUsed returns whether the network is used by any instances.
func (n *Network) IsUsed() bool {
	// Look for instances using the interface
	insts, err := instance.LoadFromAllProjects(n.state)
	if err != nil {
		return true
	}

	for _, inst := range insts {
		if IsInUse(inst, n.name) {
			return true
		}
	}

	return false
}

// Delete deletes a network.
func (n *Network) Delete(withDatabase bool) error {
	// Bring the network down
	if n.IsRunning() {
		err := n.Stop()
		if err != nil {
			return err
		}
	}

	// If withDatabase is false, this is a cluster notification, and we
	// don't want to perform any database work.
	if !withDatabase {
		return nil
	}

	// Remove the network from the database
	err := n.state.Cluster.NetworkDelete(n.name)
	if err != nil {
		return err
	}

	return nil
}

// Rename renames a network.
func (n *Network) Rename(name string) error {
	// Sanity checks
	if n.IsUsed() {
		return fmt.Errorf("The network is currently in use")
	}

	// Bring the network down
	if n.IsRunning() {
		err := n.Stop()
		if err != nil {
			return err
		}
	}

	// Rename directory
	if shared.PathExists(shared.VarPath("networks", name)) {
		os.RemoveAll(shared.VarPath("networks", name))
	}

	if shared.PathExists(shared.VarPath("networks", n.name)) {
		err := os.Rename(shared.VarPath("networks", n.name), shared.VarPath("networks", name))
		if err != nil {
			return err
		}
	}

	forkDNSLogPath := fmt.Sprintf("forkdns.%s.log", n.name)
	if shared.PathExists(shared.LogPath(forkDNSLogPath)) {
		err := os.Rename(forkDNSLogPath, shared.LogPath(fmt.Sprintf("forkdns.%s.log", name)))
		if err != nil {
			return err
		}
	}

	// Rename the database entry
	err := n.state.Cluster.NetworkRename(n.name, name)
	if err != nil {
		return err
	}
	n.name = name

	// Bring the network up
	err = n.Start()
	if err != nil {
		return err
	}

	return nil
}

// Start starts the network.
func (n *Network) Start() error {
	return n.setup(nil)
}

// setup restarts the network.
func (n *Network) setup(oldConfig map[string]string) error {
	// If we are in mock mode, just no-op.
	if n.state.OS.MockMode {
		return nil
	}

	// Create directory
	if !shared.PathExists(shared.VarPath("networks", n.name)) {
		err := os.MkdirAll(shared.VarPath("networks", n.name), 0711)
		if err != nil {
			return err
		}
	}

	// Create the bridge interface
	if !n.IsRunning() {
		if n.config["bridge.driver"] == "openvswitch" {
			_, err := exec.LookPath("ovs-vsctl")
			if err != nil {
				return fmt.Errorf("Open vSwitch isn't installed on this system")
			}

			_, err = shared.RunCommand("ovs-vsctl", "add-br", n.name)
			if err != nil {
				return err
			}
		} else {
			_, err := shared.RunCommand("ip", "link", "add", "dev", n.name, "type", "bridge")
			if err != nil {
				return err
			}
		}
	}

	// Get a list of tunnels
	tunnels := n.getTunnels()

	// IPv6 bridge configuration
	if !shared.StringInSlice(n.config["ipv6.address"], []string{"", "none"}) {
		if !shared.PathExists("/proc/sys/net/ipv6") {
			return fmt.Errorf("Network has ipv6.address but kernel IPv6 support is missing")
		}

		err := util.SysctlSet(fmt.Sprintf("net/ipv6/conf/%s/autoconf", n.name), "0")
		if err != nil {
			return err
		}

		err = util.SysctlSet(fmt.Sprintf("net/ipv6/conf/%s/accept_dad", n.name), "0")
		if err != nil {
			return err
		}
	}

	// Get a list of interfaces
	ifaces, err := net.Interfaces()
	if err != nil {
		return err
	}

	// Cleanup any existing tunnel device
	for _, iface := range ifaces {
		if strings.HasPrefix(iface.Name, fmt.Sprintf("%s-", n.name)) {
			_, err = shared.RunCommand("ip", "link", "del", "dev", iface.Name)
			if err != nil {
				return err
			}
		}
	}

	// Set the MTU
	mtu := ""
	if n.config["bridge.mtu"] != "" {
		mtu = n.config["bridge.mtu"]
	} else if len(tunnels) > 0 {
		mtu = "1400"
	} else if n.config["bridge.mode"] == "fan" {
		if n.config["fan.type"] == "ipip" {
			mtu = "1480"
		} else {
			mtu = "1450"
		}
	}

	// Attempt to add a dummy device to the bridge to force the MTU
	if mtu != "" && n.config["bridge.driver"] != "openvswitch" {
		_, err = shared.RunCommand("ip", "link", "add", "dev", fmt.Sprintf("%s-mtu", n.name), "mtu", mtu, "type", "dummy")
		if err == nil {
			_, err = shared.RunCommand("ip", "link", "set", "dev", fmt.Sprintf("%s-mtu", n.name), "up")
			if err == nil {
				AttachInterface(n.name, fmt.Sprintf("%s-mtu", n.name))
			}
		}
	}

	// Now, set a default MTU
	if mtu == "" {
		mtu = "1500"
	}

	_, err = shared.RunCommand("ip", "link", "set", "dev", n.name, "mtu", mtu)
	if err != nil {
		return err
	}

	// Set the MAC address
	if n.config["bridge.hwaddr"] != "" {
		_, err = shared.RunCommand("ip", "link", "set", "dev", n.name, "address", n.config["bridge.hwaddr"])
		if err != nil {
			return err
		}
	}

	// Bring it up
	_, err = shared.RunCommand("ip", "link", "set", "dev", n.name, "up")
	if err != nil {
		return err
	}

	// Add any listed existing external interface
	if n.config["bridge.external_interfaces"] != "" {
		for _, entry := range strings.Split(n.config["bridge.external_interfaces"], ",") {
			entry = strings.TrimSpace(entry)
			iface, err := net.InterfaceByName(entry)
			if err != nil {
				continue
			}

			unused := true
			addrs, err := iface.Addrs()
			if err == nil {
				for _, addr := range addrs {
					ip, _, err := net.ParseCIDR(addr.String())
					if ip != nil && err == nil && ip.IsGlobalUnicast() {
						unused = false
						break
					}
				}
			}

			if !unused {
				return fmt.Errorf("Only unconfigured network interfaces can be bridged")
			}

			err = AttachInterface(n.name, entry)
			if err != nil {
				return err
			}
		}
	}

	// Remove any existing IPv4 iptables rules
	if n.config["ipv4.firewall"] == "" || shared.IsTrue(n.config["ipv4.firewall"]) || (oldConfig != nil && (oldConfig["ipv4.firewall"] == "" || shared.IsTrue(oldConfig["ipv4.firewall"]))) {
		err = n.state.Firewall.NetworkClear(firewallConsts.FamilyIPv4, firewallConsts.TableAll, n.name)
		if err != nil {
			return err
		}

		err = n.state.Firewall.NetworkClear(firewallConsts.FamilyIPv4, firewallConsts.TableMangle, n.name)
		if err != nil {
			return err
		}
	}

	if shared.IsTrue(n.config["ipv4.nat"]) || (oldConfig != nil && shared.IsTrue(oldConfig["ipv4.nat"])) {
		err = n.state.Firewall.NetworkClear(firewallConsts.FamilyIPv4, firewallConsts.TableNat, n.name)
		if err != nil {
			return err
		}
	}

	// Snapshot container specific IPv4 routes (added with boot proto) before removing IPv4 addresses.
	// This is because the kernel removes any static routes on an interface when all addresses removed.
	ctRoutes, err := n.bootRoutesV4()
	if err != nil {
		return err
	}

	// Flush all IPv4 addresses and routes
	_, err = shared.RunCommand("ip", "-4", "addr", "flush", "dev", n.name, "scope", "global")
	if err != nil {
		return err
	}

	_, err = shared.RunCommand("ip", "-4", "route", "flush", "dev", n.name, "proto", "static")
	if err != nil {
		return err
	}

	// Configure IPv4 firewall (includes fan)
	if n.config["bridge.mode"] == "fan" || !shared.StringInSlice(n.config["ipv4.address"], []string{"", "none"}) {
		if n.HasDHCPv4() && (n.config["ipv4.firewall"] == "" || shared.IsTrue(n.config["ipv4.firewall"])) {
			// Setup basic iptables overrides for DHCP/DNS
			n.state.Firewall.NetworkSetupIPv4DNSOverrides(n.name)
		}

		// Attempt a workaround for broken DHCP clients
		if n.config["ipv4.firewall"] == "" || shared.IsTrue(n.config["ipv4.firewall"]) {
			n.state.Firewall.NetworkSetupIPv4DHCPWorkaround(n.name)
		}

		// Allow forwarding
		if n.config["bridge.mode"] == "fan" || n.config["ipv4.routing"] == "" || shared.IsTrue(n.config["ipv4.routing"]) {
			err = util.SysctlSet("net/ipv4/ip_forward", "1")
			if err != nil {
				return err
			}

			if n.config["ipv4.firewall"] == "" || shared.IsTrue(n.config["ipv4.firewall"]) {
				err = n.state.Firewall.NetworkSetupAllowForwarding(firewallConsts.FamilyIPv4, n.name, firewallConsts.ActionAccept)
				if err != nil {
					return err
				}
			}
		} else {
			if n.config["ipv4.firewall"] == "" || shared.IsTrue(n.config["ipv4.firewall"]) {
				err = n.state.Firewall.NetworkSetupAllowForwarding(firewallConsts.FamilyIPv4, n.name, firewallConsts.ActionReject)
				if err != nil {
					return err
				}
			}
		}
	}

	// Start building process using subprocess package
	command := "dnsmasq"
	dnsmasqCmd := []string{"--keep-in-foreground", "--strict-order", "--bind-interfaces",
		"--except-interface=lo",
		"--no-ping", // --no-ping is very important to prevent delays to lease file updates.
		fmt.Sprintf("--interface=%s", n.name)}

	dnsmasqVersion, err := dnsmasq.GetVersion()
	if err != nil {
		return err
	}

	// --dhcp-rapid-commit option is only supported on >2.79
	minVer, _ := version.NewDottedVersion("2.79")
	if dnsmasqVersion.Compare(minVer) > 0 {
		dnsmasqCmd = append(dnsmasqCmd, "--dhcp-rapid-commit")
	}

	if !daemon.Debug {
		// --quiet options are only supported on >2.67
		minVer, _ := version.NewDottedVersion("2.67")

		if err == nil && dnsmasqVersion.Compare(minVer) > 0 {
			dnsmasqCmd = append(dnsmasqCmd, []string{"--quiet-dhcp", "--quiet-dhcp6", "--quiet-ra"}...)
		}
	}

	// Configure IPv4
	if !shared.StringInSlice(n.config["ipv4.address"], []string{"", "none"}) {
		// Parse the subnet
		ip, subnet, err := net.ParseCIDR(n.config["ipv4.address"])
		if err != nil {
			return err
		}

		// Update the dnsmasq config
		dnsmasqCmd = append(dnsmasqCmd, fmt.Sprintf("--listen-address=%s", ip.String()))
		if n.HasDHCPv4() {
			if !shared.StringInSlice("--dhcp-no-override", dnsmasqCmd) {
				dnsmasqCmd = append(dnsmasqCmd, []string{"--dhcp-no-override", "--dhcp-authoritative", fmt.Sprintf("--dhcp-leasefile=%s", shared.VarPath("networks", n.name, "dnsmasq.leases")), fmt.Sprintf("--dhcp-hostsfile=%s", shared.VarPath("networks", n.name, "dnsmasq.hosts"))}...)
			}

			if n.config["ipv4.dhcp.gateway"] != "" {
				dnsmasqCmd = append(dnsmasqCmd, fmt.Sprintf("--dhcp-option=3,%s", n.config["ipv4.dhcp.gateway"]))
			}

			expiry := "1h"
			if n.config["ipv4.dhcp.expiry"] != "" {
				expiry = n.config["ipv4.dhcp.expiry"]
			}

			if n.config["ipv4.dhcp.ranges"] != "" {
				for _, dhcpRange := range strings.Split(n.config["ipv4.dhcp.ranges"], ",") {
					dhcpRange = strings.TrimSpace(dhcpRange)
					dnsmasqCmd = append(dnsmasqCmd, []string{"--dhcp-range", fmt.Sprintf("%s,%s", strings.Replace(dhcpRange, "-", ",", -1), expiry)}...)
				}
			} else {
				dnsmasqCmd = append(dnsmasqCmd, []string{"--dhcp-range", fmt.Sprintf("%s,%s,%s", GetIP(subnet, 2).String(), GetIP(subnet, -2).String(), expiry)}...)
			}
		}

		// Add the address
		_, err = shared.RunCommand("ip", "-4", "addr", "add", "dev", n.name, n.config["ipv4.address"])
		if err != nil {
			return err
		}

		// Configure NAT
		if shared.IsTrue(n.config["ipv4.nat"]) {
			//If a SNAT source address is specified, use that, otherwise default to using MASQUERADE mode.
			args := []string{"-s", subnet.String(), "!", "-d", subnet.String(), "-j", "MASQUERADE"}
			if n.config["ipv4.nat.address"] != "" {
				args = []string{"-s", subnet.String(), "!", "-d", subnet.String(), "-j", "SNAT", "--to", n.config["ipv4.nat.address"]}
			}

			if n.config["ipv4.nat.order"] == "after" {
				err = n.state.Firewall.NetworkSetupNAT(firewallConsts.FamilyIPv4, n.name, firewallConsts.LocationAppend, args...)
				if err != nil {
					return err
				}
			} else {
				err = n.state.Firewall.NetworkSetupNAT(firewallConsts.FamilyIPv4, n.name, firewallConsts.LocationPrepend, args...)
				if err != nil {
					return err
				}
			}
		}

		// Add additional routes
		if n.config["ipv4.routes"] != "" {
			for _, route := range strings.Split(n.config["ipv4.routes"], ",") {
				route = strings.TrimSpace(route)
				_, err = shared.RunCommand("ip", "-4", "route", "add", "dev", n.name, route, "proto", "static")
				if err != nil {
					return err
				}
			}
		}

		// Restore container specific IPv4 routes to interface.
		err = n.applyBootRoutesV4(ctRoutes)
		if err != nil {
			return err
		}
	}

	// Remove any existing IPv6 iptables rules
	if n.config["ipv6.firewall"] == "" || shared.IsTrue(n.config["ipv6.firewall"]) || (oldConfig != nil && (oldConfig["ipv6.firewall"] == "" || shared.IsTrue(oldConfig["ipv6.firewall"]))) {
		err = n.state.Firewall.NetworkClear(firewallConsts.FamilyIPv6, firewallConsts.TableAll, n.name)
		if err != nil {
			return err
		}
	}

	if shared.IsTrue(n.config["ipv6.nat"]) || (oldConfig != nil && shared.IsTrue(oldConfig["ipv6.nat"])) {
		err = n.state.Firewall.NetworkClear(firewallConsts.FamilyIPv6, firewallConsts.TableNat, n.name)
		if err != nil {
			return err
		}
	}

	// Snapshot container specific IPv6 routes (added with boot proto) before removing IPv6 addresses.
	// This is because the kernel removes any static routes on an interface when all addresses removed.
	ctRoutes, err = n.bootRoutesV6()
	if err != nil {
		return err
	}

	// Flush all IPv6 addresses and routes
	_, err = shared.RunCommand("ip", "-6", "addr", "flush", "dev", n.name, "scope", "global")
	if err != nil {
		return err
	}

	_, err = shared.RunCommand("ip", "-6", "route", "flush", "dev", n.name, "proto", "static")
	if err != nil {
		return err
	}

	// Configure IPv6
	if !shared.StringInSlice(n.config["ipv6.address"], []string{"", "none"}) {
		// Enable IPv6 for the subnet
		err := util.SysctlSet(fmt.Sprintf("net/ipv6/conf/%s/disable_ipv6", n.name), "0")
		if err != nil {
			return err
		}

		// Parse the subnet
		ip, subnet, err := net.ParseCIDR(n.config["ipv6.address"])
		if err != nil {
			return err
		}

		// Update the dnsmasq config
		dnsmasqCmd = append(dnsmasqCmd, []string{fmt.Sprintf("--listen-address=%s", ip.String()), "--enable-ra"}...)
		if n.HasDHCPv6() {
			if n.config["ipv6.firewall"] == "" || shared.IsTrue(n.config["ipv6.firewall"]) {
				// Setup basic iptables overrides for DHCP/DNS
				n.state.Firewall.NetworkSetupIPv6DNSOverrides(n.name)
			}

			// Build DHCP configuration
			if !shared.StringInSlice("--dhcp-no-override", dnsmasqCmd) {
				dnsmasqCmd = append(dnsmasqCmd, []string{"--dhcp-no-override", "--dhcp-authoritative", fmt.Sprintf("--dhcp-leasefile=%s", shared.VarPath("networks", n.name, "dnsmasq.leases")), fmt.Sprintf("--dhcp-hostsfile=%s", shared.VarPath("networks", n.name, "dnsmasq.hosts"))}...)
			}

			expiry := "1h"
			if n.config["ipv6.dhcp.expiry"] != "" {
				expiry = n.config["ipv6.dhcp.expiry"]
			}

			if shared.IsTrue(n.config["ipv6.dhcp.stateful"]) {
				subnetSize, _ := subnet.Mask.Size()
				if n.config["ipv6.dhcp.ranges"] != "" {
					for _, dhcpRange := range strings.Split(n.config["ipv6.dhcp.ranges"], ",") {
						dhcpRange = strings.TrimSpace(dhcpRange)
						dnsmasqCmd = append(dnsmasqCmd, []string{"--dhcp-range", fmt.Sprintf("%s,%d,%s", strings.Replace(dhcpRange, "-", ",", -1), subnetSize, expiry)}...)
					}
				} else {
					dnsmasqCmd = append(dnsmasqCmd, []string{"--dhcp-range", fmt.Sprintf("%s,%s,%d,%s", GetIP(subnet, 2), GetIP(subnet, -1), subnetSize, expiry)}...)
				}
			} else {
				dnsmasqCmd = append(dnsmasqCmd, []string{"--dhcp-range", fmt.Sprintf("::,constructor:%s,ra-stateless,ra-names", n.name)}...)
			}
		} else {
			dnsmasqCmd = append(dnsmasqCmd, []string{"--dhcp-range", fmt.Sprintf("::,constructor:%s,ra-only", n.name)}...)
		}

		// Allow forwarding
		if n.config["ipv6.routing"] == "" || shared.IsTrue(n.config["ipv6.routing"]) {
			// Get a list of proc entries
			entries, err := ioutil.ReadDir("/proc/sys/net/ipv6/conf/")
			if err != nil {
				return err
			}

			// First set accept_ra to 2 for everything
			for _, entry := range entries {
				content, err := ioutil.ReadFile(fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/accept_ra", entry.Name()))
				if err == nil && string(content) != "1\n" {
					continue
				}

				err = util.SysctlSet(fmt.Sprintf("net/ipv6/conf/%s/accept_ra", entry.Name()), "2")
				if err != nil && !os.IsNotExist(err) {
					return err
				}
			}

			// Then set forwarding for all of them
			for _, entry := range entries {
				err = util.SysctlSet(fmt.Sprintf("net/ipv6/conf/%s/forwarding", entry.Name()), "1")
				if err != nil && !os.IsNotExist(err) {
					return err
				}
			}

			if n.config["ipv6.firewall"] == "" || shared.IsTrue(n.config["ipv6.firewall"]) {
				err = n.state.Firewall.NetworkSetupAllowForwarding(firewallConsts.FamilyIPv6, n.name, firewallConsts.ActionAccept)
				if err != nil {
					return err
				}
			}
		} else {
			if n.config["ipv6.firewall"] == "" || shared.IsTrue(n.config["ipv6.firewall"]) {
				err = n.state.Firewall.NetworkSetupAllowForwarding(firewallConsts.FamilyIPv6, n.name, firewallConsts.ActionReject)
				if err != nil {
					return err
				}
			}
		}

		// Add the address
		_, err = shared.RunCommand("ip", "-6", "addr", "add", "dev", n.name, n.config["ipv6.address"])
		if err != nil {
			return err
		}

		// Configure NAT
		if shared.IsTrue(n.config["ipv6.nat"]) {
			args := []string{"-s", subnet.String(), "!", "-d", subnet.String(), "-j", "MASQUERADE"}
			if n.config["ipv6.nat.address"] != "" {
				args = []string{"-s", subnet.String(), "!", "-d", subnet.String(), "-j", "SNAT", "--to", n.config["ipv6.nat.address"]}
			}

			if n.config["ipv6.nat.order"] == "after" {
				err = n.state.Firewall.NetworkSetupNAT(firewallConsts.FamilyIPv6, n.name, firewallConsts.LocationAppend, args...)
				if err != nil {
					return err
				}
			} else {
				err = n.state.Firewall.NetworkSetupNAT(firewallConsts.FamilyIPv6, n.name, firewallConsts.LocationPrepend, args...)
				if err != nil {
					return err
				}
			}
		}

		// Add additional routes
		if n.config["ipv6.routes"] != "" {
			for _, route := range strings.Split(n.config["ipv6.routes"], ",") {
				route = strings.TrimSpace(route)
				_, err = shared.RunCommand("ip", "-6", "route", "add", "dev", n.name, route, "proto", "static")
				if err != nil {
					return err
				}
			}
		}

		// Restore container specific IPv6 routes to interface.
		err = n.applyBootRoutesV6(ctRoutes)
		if err != nil {
			return err
		}
	}

	// Configure the fan
	dnsClustered := false
	dnsClusteredAddress := ""
	var overlaySubnet *net.IPNet
	if n.config["bridge.mode"] == "fan" {
		tunName := fmt.Sprintf("%s-fan", n.name)

		// Parse the underlay
		underlay := n.config["fan.underlay_subnet"]
		_, underlaySubnet, err := net.ParseCIDR(underlay)
		if err != nil {
			return nil
		}

		// Parse the overlay
		overlay := n.config["fan.overlay_subnet"]
		if overlay == "" {
			overlay = "240.0.0.0/8"
		}

		_, overlaySubnet, err = net.ParseCIDR(overlay)
		if err != nil {
			return err
		}

		// Get the address
		fanAddress, devName, devAddr, err := n.fanAddress(underlaySubnet, overlaySubnet)
		if err != nil {
			return err
		}

		addr := strings.Split(fanAddress, "/")
		if n.config["fan.type"] == "ipip" {
			fanAddress = fmt.Sprintf("%s/24", addr[0])
		}

		// Update the MTU based on overlay device (if available)
		fanMtuInt, err := GetDevMTU(devName)
		if err == nil {
			// Apply overhead
			if n.config["fan.type"] == "ipip" {
				fanMtuInt = fanMtuInt - 20
			} else {
				fanMtuInt = fanMtuInt - 50
			}

			// Apply changes
			fanMtu := fmt.Sprintf("%d", fanMtuInt)
			if fanMtu != mtu {
				mtu = fanMtu
				if n.config["bridge.driver"] != "openvswitch" {
					_, err = shared.RunCommand("ip", "link", "set", "dev", fmt.Sprintf("%s-mtu", n.name), "mtu", mtu)
					if err != nil {
						return err
					}
				}

				_, err = shared.RunCommand("ip", "link", "set", "dev", n.name, "mtu", mtu)
				if err != nil {
					return err
				}
			}
		}

		// Parse the host subnet
		_, hostSubnet, err := net.ParseCIDR(fmt.Sprintf("%s/24", addr[0]))
		if err != nil {
			return err
		}

		// Add the address
		_, err = shared.RunCommand("ip", "-4", "addr", "add", "dev", n.name, fanAddress)
		if err != nil {
			return err
		}

		// Update the dnsmasq config
		expiry := "1h"
		if n.config["ipv4.dhcp.expiry"] != "" {
			expiry = n.config["ipv4.dhcp.expiry"]
		}

		dnsmasqCmd = append(dnsmasqCmd, []string{
			fmt.Sprintf("--listen-address=%s", addr[0]),
			"--dhcp-no-override", "--dhcp-authoritative",
			fmt.Sprintf("--dhcp-leasefile=%s", shared.VarPath("networks", n.name, "dnsmasq.leases")),
			fmt.Sprintf("--dhcp-hostsfile=%s", shared.VarPath("networks", n.name, "dnsmasq.hosts")),
			"--dhcp-range", fmt.Sprintf("%s,%s,%s", GetIP(hostSubnet, 2).String(), GetIP(hostSubnet, -2).String(), expiry)}...)

		// Setup the tunnel
		if n.config["fan.type"] == "ipip" {
			_, err = shared.RunCommand("ip", "-4", "route", "flush", "dev", "tunl0")
			if err != nil {
				return err
			}

			_, err = shared.RunCommand("ip", "link", "set", "dev", "tunl0", "up")
			if err != nil {
				return err
			}

			// Fails if the map is already set
			shared.RunCommand("ip", "link", "change", "dev", "tunl0", "type", "ipip", "fan-map", fmt.Sprintf("%s:%s", overlay, underlay))

			_, err = shared.RunCommand("ip", "route", "add", overlay, "dev", "tunl0", "src", addr[0])
			if err != nil {
				return err
			}
		} else {
			vxlanID := fmt.Sprintf("%d", binary.BigEndian.Uint32(overlaySubnet.IP.To4())>>8)

			_, err = shared.RunCommand("ip", "link", "add", tunName, "type", "vxlan", "id", vxlanID, "dev", devName, "dstport", "0", "local", devAddr, "fan-map", fmt.Sprintf("%s:%s", overlay, underlay))
			if err != nil {
				return err
			}

			err = AttachInterface(n.name, tunName)
			if err != nil {
				return err
			}

			_, err = shared.RunCommand("ip", "link", "set", "dev", tunName, "mtu", mtu, "up")
			if err != nil {
				return err
			}

			_, err = shared.RunCommand("ip", "link", "set", "dev", n.name, "up")
			if err != nil {
				return err
			}
		}

		// Configure NAT
		if n.config["ipv4.nat"] == "" || shared.IsTrue(n.config["ipv4.nat"]) {
			if n.config["ipv4.nat.order"] == "after" {
				err = n.state.Firewall.NetworkSetupTunnelNAT(n.name, firewallConsts.LocationAppend, *overlaySubnet)
				if err != nil {
					return err
				}
			} else {
				err = n.state.Firewall.NetworkSetupTunnelNAT(n.name, firewallConsts.LocationPrepend, *overlaySubnet)
				if err != nil {
					return err
				}
			}
		}

		// Setup clustered DNS
		clusterAddress, err := node.ClusterAddress(n.state.Node)
		if err != nil {
			return err
		}

		// If clusterAddress is non-empty, this indicates the intention for this node to be
		// part of a cluster and so we should ensure that dnsmasq and forkdns are started
		// in cluster mode. Note: During LXD initialisation the cluster may not actually be
		// setup yet, but we want the DNS processes to be ready for when it is.
		if clusterAddress != "" {
			dnsClustered = true
		}

		dnsClusteredAddress = strings.Split(fanAddress, "/")[0]
	}

	// Configure tunnels
	for _, tunnel := range tunnels {
		getConfig := func(key string) string {
			return n.config[fmt.Sprintf("tunnel.%s.%s", tunnel, key)]
		}

		tunProtocol := getConfig("protocol")
		tunLocal := getConfig("local")
		tunRemote := getConfig("remote")
		tunName := fmt.Sprintf("%s-%s", n.name, tunnel)

		// Configure the tunnel
		cmd := []string{"ip", "link", "add", "dev", tunName}
		if tunProtocol == "gre" {
			// Skip partial configs
			if tunProtocol == "" || tunLocal == "" || tunRemote == "" {
				continue
			}

			cmd = append(cmd, []string{"type", "gretap", "local", tunLocal, "remote", tunRemote}...)
		} else if tunProtocol == "vxlan" {
			tunGroup := getConfig("group")
			tunInterface := getConfig("interface")

			// Skip partial configs
			if tunProtocol == "" {
				continue
			}

			cmd = append(cmd, []string{"type", "vxlan"}...)

			if tunLocal != "" && tunRemote != "" {
				cmd = append(cmd, []string{"local", tunLocal, "remote", tunRemote}...)
			} else {
				if tunGroup == "" {
					tunGroup = "239.0.0.1"
				}

				devName := tunInterface
				if devName == "" {
					_, devName, err = DefaultGatewaySubnetV4()
					if err != nil {
						return err
					}
				}

				cmd = append(cmd, []string{"group", tunGroup, "dev", devName}...)
			}

			tunPort := getConfig("port")
			if tunPort == "" {
				tunPort = "0"
			}
			cmd = append(cmd, []string{"dstport", tunPort}...)

			tunID := getConfig("id")
			if tunID == "" {
				tunID = "1"
			}
			cmd = append(cmd, []string{"id", tunID}...)

			tunTTL := getConfig("ttl")
			if tunTTL == "" {
				tunTTL = "1"
			}
			cmd = append(cmd, []string{"ttl", tunTTL}...)
		}

		// Create the interface
		_, err = shared.RunCommand(cmd[0], cmd[1:]...)
		if err != nil {
			return err
		}

		// Bridge it and bring up
		err = AttachInterface(n.name, tunName)
		if err != nil {
			return err
		}

		_, err = shared.RunCommand("ip", "link", "set", "dev", tunName, "mtu", mtu, "up")
		if err != nil {
			return err
		}

		_, err = shared.RunCommand("ip", "link", "set", "dev", n.name, "up")
		if err != nil {
			return err
		}
	}

	// Kill any existing dnsmasq and forkdns daemon for this network
	err = dnsmasq.Kill(n.name, false)
	if err != nil {
		return err
	}

	err = n.killForkDNS()
	if err != nil {
		return err
	}

	// Configure dnsmasq
	if n.config["bridge.mode"] == "fan" || !shared.StringInSlice(n.config["ipv4.address"], []string{"", "none"}) || !shared.StringInSlice(n.config["ipv6.address"], []string{"", "none"}) {
		// Setup the dnsmasq domain
		dnsDomain := n.config["dns.domain"]
		if dnsDomain == "" {
			dnsDomain = "lxd"
		}

		if n.config["dns.mode"] != "none" {
			if dnsClustered {
				dnsmasqCmd = append(dnsmasqCmd, "-s", dnsDomain)
				dnsmasqCmd = append(dnsmasqCmd, "-S", fmt.Sprintf("/%s/%s#1053", dnsDomain, dnsClusteredAddress))
				dnsmasqCmd = append(dnsmasqCmd, fmt.Sprintf("--rev-server=%s,%s#1053", overlaySubnet, dnsClusteredAddress))
			} else {
				dnsmasqCmd = append(dnsmasqCmd, []string{"-s", dnsDomain, "-S", fmt.Sprintf("/%s/", dnsDomain)}...)
			}
		}

		// Create a config file to contain additional config (and to prevent dnsmasq from reading /etc/dnsmasq.conf)
		err = ioutil.WriteFile(shared.VarPath("networks", n.name, "dnsmasq.raw"), []byte(fmt.Sprintf("%s\n", n.config["raw.dnsmasq"])), 0644)
		if err != nil {
			return err
		}
		dnsmasqCmd = append(dnsmasqCmd, fmt.Sprintf("--conf-file=%s", shared.VarPath("networks", n.name, "dnsmasq.raw")))

		// Attempt to drop privileges
		if n.state.OS.UnprivUser != "" {
			dnsmasqCmd = append(dnsmasqCmd, []string{"-u", n.state.OS.UnprivUser}...)
		}

		// Create DHCP hosts directory
		if !shared.PathExists(shared.VarPath("networks", n.name, "dnsmasq.hosts")) {
			err = os.MkdirAll(shared.VarPath("networks", n.name, "dnsmasq.hosts"), 0755)
			if err != nil {
				return err
			}
		}

		// Check for dnsmasq
		_, err := exec.LookPath("dnsmasq")
		if err != nil {
			return fmt.Errorf("dnsmasq is required for LXD managed bridges")
		}

		// Update the static leases
		err = UpdateDNSMasqStatic(n.state, n.name)
		if err != nil {
			return err
		}

		// Create subprocess object dnsmasq (occasionally races, try a few times)
		p, err := subprocess.NewProcess(command, dnsmasqCmd, "", "")
		if err != nil {
			return fmt.Errorf("Failed to create subprocess: %s", err)
		}

		err = p.Start()
		if err != nil {
			return fmt.Errorf("Failed to run: %s %s: %v", command, strings.Join(dnsmasqCmd, " "), err)
		}

		err = p.Save(shared.VarPath("networks", n.name, "dnsmasq.pid"))
		if err != nil {
			// Kill Process if started, but could not save the file
			err2 := p.Stop()
			if err != nil {
				return fmt.Errorf("Could not kill subprocess while handling saving error: %s: %s", err, err2)
			}

			return fmt.Errorf("Failed to save subprocess details: %s", err)
		}

		// Spawn DNS forwarder if needed (backgrounded to avoid deadlocks during cluster boot)
		if dnsClustered {
			// Create forkdns servers directory
			if !shared.PathExists(shared.VarPath("networks", n.name, ForkdnsServersListPath)) {
				err = os.MkdirAll(shared.VarPath("networks", n.name, ForkdnsServersListPath), 0755)
				if err != nil {
					return err
				}
			}

			// Create forkdns servers.conf file if doesn't exist
			f, err := os.OpenFile(shared.VarPath("networks", n.name, ForkdnsServersListPath+"/"+ForkdnsServersListFile), os.O_RDONLY|os.O_CREATE, 0666)
			if err != nil {
				return err
			}
			f.Close()

			err = n.spawnForkDNS(dnsClusteredAddress)
			if err != nil {
				return err
			}
		}
	} else {
		// Clean up old dnsmasq config if exists and we are not starting dnsmasq.
		leasesPath := shared.VarPath("networks", n.name, "dnsmasq.leases")
		if shared.PathExists(leasesPath) {
			err := os.Remove(leasesPath)
			if err != nil {
				return errors.Wrapf(err, "Failed to remove old dnsmasq leases file '%s'", leasesPath)
			}
		}

		// And same for our PID file.
		pidPath := shared.VarPath("networks", n.name, "dnsmasq.pid")
		if shared.PathExists(pidPath) {
			err := os.Remove(pidPath)
			if err != nil {
				return errors.Wrapf(err, "Failed to remove old dnsmasq pid file '%s'", pidPath)
			}
		}
	}

	return nil
}

// Stop stops the network.
func (n *Network) Stop() error {
	if !n.IsRunning() {
		return fmt.Errorf("The network is already stopped")
	}

	// Destroy the bridge interface
	if n.config["bridge.driver"] == "openvswitch" {
		_, err := shared.RunCommand("ovs-vsctl", "del-br", n.name)
		if err != nil {
			return err
		}
	} else {
		_, err := shared.RunCommand("ip", "link", "del", "dev", n.name)
		if err != nil {
			return err
		}
	}

	// Cleanup iptables
	if n.config["ipv4.firewall"] == "" || shared.IsTrue(n.config["ipv4.firewall"]) {
		err := n.state.Firewall.NetworkClear(firewallConsts.FamilyIPv4, firewallConsts.TableAll, n.name)
		if err != nil {
			return err
		}

		err = n.state.Firewall.NetworkClear(firewallConsts.FamilyIPv4, firewallConsts.TableMangle, n.name)
		if err != nil {
			return err
		}
	}

	if shared.IsTrue(n.config["ipv4.nat"]) {
		err := n.state.Firewall.NetworkClear(firewallConsts.FamilyIPv4, firewallConsts.TableNat, n.name)
		if err != nil {
			return err
		}
	}

	if n.config["ipv6.firewall"] == "" || shared.IsTrue(n.config["ipv6.firewall"]) {
		err := n.state.Firewall.NetworkClear(firewallConsts.FamilyIPv6, firewallConsts.TableAll, n.name)
		if err != nil {
			return err
		}
	}

	if shared.IsTrue(n.config["ipv6.nat"]) {
		err := n.state.Firewall.NetworkClear(firewallConsts.FamilyIPv6, firewallConsts.TableNat, n.name)
		if err != nil {
			return err
		}
	}

	// Kill any existing dnsmasq and forkdns daemon for this network
	err := dnsmasq.Kill(n.name, false)
	if err != nil {
		return err
	}

	err = n.killForkDNS()
	if err != nil {
		return err
	}

	// Get a list of interfaces
	ifaces, err := net.Interfaces()
	if err != nil {
		return err
	}

	// Cleanup any existing tunnel device
	for _, iface := range ifaces {
		if strings.HasPrefix(iface.Name, fmt.Sprintf("%s-", n.name)) {
			_, err = shared.RunCommand("ip", "link", "del", "dev", iface.Name)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// Update updates the network.
func (n *Network) Update(newNetwork api.NetworkPut, notify bool) error {
	err := fillAuto(newNetwork.Config)
	if err != nil {
		return err
	}
	newConfig := newNetwork.Config

	// Backup the current state
	oldConfig := map[string]string{}
	oldDescription := n.description
	err = shared.DeepCopy(&n.config, &oldConfig)
	if err != nil {
		return err
	}

	// Define a function which reverts everything.  Defer this function
	// so that it doesn't need to be explicitly called in every failing
	// return path.  Track whether or not we want to undo the changes
	// using a closure.
	undoChanges := true
	defer func() {
		if undoChanges {
			// Revert changes to the struct
			n.config = oldConfig
			n.description = oldDescription

			// Update the database
			n.state.Cluster.NetworkUpdate(n.name, n.description, n.config)

			// Reset any change that was made to the bridge
			n.setup(newConfig)
		}
	}()

	// Diff the configurations
	changedConfig := []string{}
	userOnly := true
	for key := range oldConfig {
		if oldConfig[key] != newConfig[key] {
			if !strings.HasPrefix(key, "user.") {
				userOnly = false
			}

			if !shared.StringInSlice(key, changedConfig) {
				changedConfig = append(changedConfig, key)
			}
		}
	}

	for key := range newConfig {
		if oldConfig[key] != newConfig[key] {
			if !strings.HasPrefix(key, "user.") {
				userOnly = false
			}

			if !shared.StringInSlice(key, changedConfig) {
				changedConfig = append(changedConfig, key)
			}
		}
	}

	// Skip on no change
	if len(changedConfig) == 0 && newNetwork.Description == n.description {
		return nil
	}

	// Update the network
	if !userOnly {
		if shared.StringInSlice("bridge.driver", changedConfig) && n.IsRunning() {
			err = n.Stop()
			if err != nil {
				return err
			}
		}

		if shared.StringInSlice("bridge.external_interfaces", changedConfig) && n.IsRunning() {
			devices := []string{}
			for _, dev := range strings.Split(newConfig["bridge.external_interfaces"], ",") {
				dev = strings.TrimSpace(dev)
				devices = append(devices, dev)
			}

			for _, dev := range strings.Split(oldConfig["bridge.external_interfaces"], ",") {
				dev = strings.TrimSpace(dev)
				if dev == "" {
					continue
				}

				if !shared.StringInSlice(dev, devices) && shared.PathExists(fmt.Sprintf("/sys/class/net/%s", dev)) {
					err = DetachInterface(n.name, dev)
					if err != nil {
						return err
					}
				}
			}
		}
	}

	// Apply changes
	n.config = newConfig
	n.description = newNetwork.Description

	// Update the database
	if !notify {
		// Notify all other nodes to update the network.
		notifier, err := cluster.NewNotifier(n.state, n.state.Endpoints.NetworkCert(), cluster.NotifyAll)
		if err != nil {
			return err
		}

		err = notifier(func(client lxd.InstanceServer) error {
			return client.UpdateNetwork(n.name, newNetwork, "")
		})
		if err != nil {
			return err
		}

		// Update the database.
		err = n.state.Cluster.NetworkUpdate(n.name, n.description, n.config)
		if err != nil {
			return err
		}
	}

	// Restart the network
	if !userOnly {
		err = n.setup(oldConfig)
		if err != nil {
			return err
		}
	}

	// Success, update the closure to mark that the changes should be kept.
	undoChanges = false

	return nil
}

func (n *Network) spawnForkDNS(listenAddress string) error {
	// Setup the dnsmasq domain
	dnsDomain := n.config["dns.domain"]
	if dnsDomain == "" {
		dnsDomain = "lxd"
	}

	// Spawn the daemon using subprocess
	command := n.state.OS.ExecPath
	forkdnsargs := []string{"forkdns",
		fmt.Sprintf("%s:1053", listenAddress),
		dnsDomain,
		n.name}

	logPath := shared.LogPath(fmt.Sprintf("forkdns.%s.log", n.name))

	p, err := subprocess.NewProcess(command, forkdnsargs, logPath, logPath)
	if err != nil {
		return fmt.Errorf("Failed to create subprocess: %s", err)
	}

	err = p.Start()
	if err != nil {
		return fmt.Errorf("Failed to run: %s %s: %v", command, strings.Join(forkdnsargs, " "), err)
	}

	err = p.Save(shared.VarPath("networks", n.name, "forkdns.pid"))
	if err != nil {
		// Kill Process if started, but could not save the file
		err2 := p.Stop()
		if err != nil {
			return fmt.Errorf("Could not kill subprocess while handling saving error: %s: %s", err, err2)
		}

		return fmt.Errorf("Failed to save subprocess details: %s", err)
	}

	return nil
}

// RefreshForkdnsServerAddresses retrieves the IPv4 address of each cluster node (excluding ourselves)
// for this network. It then updates the forkdns server list file if there are changes.
func (n *Network) RefreshForkdnsServerAddresses(heartbeatData *cluster.APIHeartbeat) error {
	addresses := []string{}
	localAddress, err := node.HTTPSAddress(n.state.Node)
	if err != nil {
		return err
	}

	logger.Infof("Refreshing forkdns peers for %v", n.name)

	cert := n.state.Endpoints.NetworkCert()
	for _, node := range heartbeatData.Members {
		if node.Address == localAddress {
			// No need to query ourselves.
			continue
		}

		client, err := cluster.Connect(node.Address, cert, true)
		if err != nil {
			return err
		}

		state, err := client.GetNetworkState(n.name)
		if err != nil {
			return err
		}

		for _, addr := range state.Addresses {
			// Only get IPv4 addresses of nodes on network.
			if addr.Family != "inet" || addr.Scope != "global" {
				continue
			}

			addresses = append(addresses, addr.Address)
			break
		}
	}

	// Compare current stored list to retrieved list and see if we need to update.
	curList, err := ForkdnsServersList(n.name)
	if err != nil {
		// Only warn here, but continue on to regenerate the servers list from cluster info.
		logger.Warnf("Failed to load existing forkdns server list: %v", err)
	}

	// If current list is same as cluster list, nothing to do.
	if err == nil && reflect.DeepEqual(curList, addresses) {
		return nil
	}

	err = n.updateForkdnsServersFile(addresses)
	if err != nil {
		return err
	}

	logger.Infof("Updated forkdns server list for '%s': %v", n.name, addresses)
	return nil
}

func (n *Network) getTunnels() []string {
	tunnels := []string{}

	for k := range n.config {
		if !strings.HasPrefix(k, "tunnel.") {
			continue
		}

		fields := strings.Split(k, ".")
		if !shared.StringInSlice(fields[1], tunnels) {
			tunnels = append(tunnels, fields[1])
		}
	}

	return tunnels
}

// bootRoutesV4 returns a list of IPv4 boot routes on the network's device.
func (n *Network) bootRoutesV4() ([]string, error) {
	routes := []string{}
	cmd := exec.Command("ip", "-4", "route", "show", "dev", n.name, "proto", "boot")
	ipOut, err := cmd.StdoutPipe()
	if err != nil {
		return routes, err
	}
	cmd.Start()
	scanner := bufio.NewScanner(ipOut)
	for scanner.Scan() {
		route := strings.Replace(scanner.Text(), "linkdown", "", -1)
		routes = append(routes, route)
	}
	cmd.Wait()
	return routes, nil
}

// bootRoutesV6 returns a list of IPv6 boot routes on the network's device.
func (n *Network) bootRoutesV6() ([]string, error) {
	routes := []string{}
	cmd := exec.Command("ip", "-6", "route", "show", "dev", n.name, "proto", "boot")
	ipOut, err := cmd.StdoutPipe()
	if err != nil {
		return routes, err
	}
	cmd.Start()
	scanner := bufio.NewScanner(ipOut)
	for scanner.Scan() {
		route := strings.Replace(scanner.Text(), "linkdown", "", -1)
		routes = append(routes, route)
	}
	cmd.Wait()
	return routes, nil
}

// applyBootRoutesV4 applies a list of IPv4 boot routes to the network's device.
func (n *Network) applyBootRoutesV4(routes []string) error {
	for _, route := range routes {
		cmd := []string{"-4", "route", "replace", "dev", n.name, "proto", "boot"}
		cmd = append(cmd, strings.Fields(route)...)
		_, err := shared.RunCommand("ip", cmd...)
		if err != nil {
			return err
		}
	}

	return nil
}

// applyBootRoutesV6 applies a list of IPv6 boot routes to the network's device.
func (n *Network) applyBootRoutesV6(routes []string) error {
	for _, route := range routes {
		cmd := []string{"-6", "route", "replace", "dev", n.name, "proto", "boot"}
		cmd = append(cmd, strings.Fields(route)...)
		_, err := shared.RunCommand("ip", cmd...)
		if err != nil {
			return err
		}
	}

	return nil
}

func (n *Network) fanAddress(underlay *net.IPNet, overlay *net.IPNet) (string, string, string, error) {
	// Sanity checks
	underlaySize, _ := underlay.Mask.Size()
	if underlaySize != 16 && underlaySize != 24 {
		return "", "", "", fmt.Errorf("Only /16 or /24 underlays are supported at this time")
	}

	overlaySize, _ := overlay.Mask.Size()
	if overlaySize != 8 && overlaySize != 16 {
		return "", "", "", fmt.Errorf("Only /8 or /16 overlays are supported at this time")
	}

	if overlaySize+(32-underlaySize)+8 > 32 {
		return "", "", "", fmt.Errorf("Underlay or overlay networks too large to accommodate the FAN")
	}

	// Get the IP
	ip, dev, err := n.addressForSubnet(underlay)
	if err != nil {
		return "", "", "", err
	}
	ipStr := ip.String()

	// Force into IPv4 format
	ipBytes := ip.To4()
	if ipBytes == nil {
		return "", "", "", fmt.Errorf("Invalid IPv4: %s", ip)
	}

	// Compute the IP
	ipBytes[0] = overlay.IP[0]
	if overlaySize == 16 {
		ipBytes[1] = overlay.IP[1]
		ipBytes[2] = ipBytes[3]
	} else if underlaySize == 24 {
		ipBytes[1] = ipBytes[3]
		ipBytes[2] = 0
	} else if underlaySize == 16 {
		ipBytes[1] = ipBytes[2]
		ipBytes[2] = ipBytes[3]
	}

	ipBytes[3] = 1

	return fmt.Sprintf("%s/%d", ipBytes.String(), overlaySize), dev, ipStr, err
}

func (n *Network) addressForSubnet(subnet *net.IPNet) (net.IP, string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return net.IP{}, "", err
	}

	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err != nil {
				continue
			}

			if subnet.Contains(ip) {
				return ip, iface.Name, nil
			}
		}
	}

	return net.IP{}, "", fmt.Errorf("No address found in subnet")
}

func (n *Network) killForkDNS() error {
	// Check if we have a running forkdns at all
	pidPath := shared.VarPath("networks", n.name, "forkdns.pid")

	// If the pid file doesn't exist, there is no process to kill.
	if !shared.PathExists(pidPath) {
		return nil
	}

	p, err := subprocess.ImportProcess(pidPath)
	if err != nil {
		return fmt.Errorf("Could not read pid file: %s", err)
	}

	err = p.Stop()
	if err != nil && err != subprocess.ErrNotRunning {
		return fmt.Errorf("Unable to kill dnsmasq: %s", err)
	}

	return nil
}

// updateForkdnsServersFile takes a list of node addresses and writes them atomically to
// the forkdns.servers file ready for forkdns to notice and re-apply its config.
func (n *Network) updateForkdnsServersFile(addresses []string) error {
	// We don't want to race with ourselves here
	forkdnsServersLock.Lock()
	defer forkdnsServersLock.Unlock()

	permName := shared.VarPath("networks", n.name, ForkdnsServersListPath+"/"+ForkdnsServersListFile)
	tmpName := permName + ".tmp"

	// Open tmp file and truncate
	tmpFile, err := os.Create(tmpName)
	if err != nil {
		return err
	}
	defer tmpFile.Close()

	for _, address := range addresses {
		_, err := tmpFile.WriteString(address + "\n")
		if err != nil {
			return err
		}
	}

	tmpFile.Close()

	// Atomically rename finished file into permanent location so forkdns can pick it up.
	err = os.Rename(tmpName, permName)
	if err != nil {
		return err
	}

	return nil
}

// HasDHCPv4 indicates whether the network has DHCPv4 enabled.
func (n *Network) HasDHCPv4() bool {
	if n.config["ipv4.dhcp"] == "" || shared.IsTrue(n.config["ipv4.dhcp"]) {
		return true
	}

	return false
}

// HasDHCPv6 indicates whether the network has DHCPv6 enabled (includes stateless SLAAC router advertisement mode).
// Technically speaking stateless SLAAC RA mode isn't DHCPv6, but for consistency with LXD's config paradigm, DHCP
// here means "an ability to automatically allocate IPs and routes", rather than stateful DHCP with leases.
// To check if true stateful DHCPv6 is enabled check the "ipv6.dhcp.stateful" config key.
func (n *Network) HasDHCPv6() bool {
	if n.config["ipv6.dhcp"] == "" || shared.IsTrue(n.config["ipv6.dhcp"]) {
		return true
	}

	return false
}

// DHCPv4Ranges returns a parsed set of DHCPv4 ranges for this network.
func (n *Network) DHCPv4Ranges() []DHCPRange {
	dhcpRanges := make([]DHCPRange, 0)
	if n.config["ipv4.dhcp.ranges"] != "" {
		for _, r := range strings.Split(n.config["ipv4.dhcp.ranges"], ",") {
			parts := strings.SplitN(strings.TrimSpace(r), "-", 2)
			if len(parts) == 2 {
				startIP := net.ParseIP(parts[0])
				endIP := net.ParseIP(parts[1])
				dhcpRanges = append(dhcpRanges, DHCPRange{
					Start: startIP.To4(),
					End:   endIP.To4(),
				})
			}
		}
	}

	return dhcpRanges
}

// DHCPv6Ranges returns a parsed set of DHCPv6 ranges for this network.
func (n *Network) DHCPv6Ranges() []DHCPRange {
	dhcpRanges := make([]DHCPRange, 0)
	if n.config["ipv6.dhcp.ranges"] != "" {
		for _, r := range strings.Split(n.config["ipv6.dhcp.ranges"], ",") {
			parts := strings.SplitN(strings.TrimSpace(r), "-", 2)
			if len(parts) == 2 {
				startIP := net.ParseIP(parts[0])
				endIP := net.ParseIP(parts[1])
				dhcpRanges = append(dhcpRanges, DHCPRange{
					Start: startIP.To16(),
					End:   endIP.To16(),
				})
			}
		}
	}

	return dhcpRanges
}
