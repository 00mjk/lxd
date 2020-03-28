package device

import (
	"fmt"
	"os"
	"strings"

	deviceConfig "github.com/lxc/lxd/lxd/device/config"
	"github.com/lxc/lxd/lxd/instance"
	"github.com/lxc/lxd/lxd/instance/instancetype"
	"github.com/lxc/lxd/lxd/network"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/shared"
)

const nicRoutedIPv4GW = "169.254.0.1"
const nicRoutedIPv6GW = "fe80::1"

type nicRouted struct {
	deviceCommon
}

func (d *nicRouted) CanHotPlug() (bool, []string) {
	return false, []string{}
}

// validateConfig checks the supplied config for correctness.
func (d *nicRouted) validateConfig(instConf instance.ConfigReader) error {
	if !instanceSupported(instConf.Type(), instancetype.Container) {
		return ErrUnsupportedDevType
	}

	requiredFields := []string{}
	optionalFields := []string{
		"name",
		"parent",
		"mtu",
		"hwaddr",
		"host_name",
		"vlan",
		"ipv4.gateway",
		"ipv6.gateway",
		"ipv4.host_address",
		"ipv6.host_address",
	}

	rules := nicValidationRules(requiredFields, optionalFields)
	rules["ipv4.address"] = func(value string) error {
		if value == "" {
			return nil
		}

		return NetworkValidAddressV4List(value)
	}
	rules["ipv6.address"] = func(value string) error {
		if value == "" {
			return nil
		}

		return NetworkValidAddressV6List(value)
	}

	err := d.config.Validate(rules)
	if err != nil {
		return err
	}

	return nil
}

// validateEnvironment checks the runtime environment for correctness.
func (d *nicRouted) validateEnvironment() error {
	if d.inst.Type() == instancetype.Container && d.config["name"] == "" {
		return fmt.Errorf("Requires name property to start")
	}

	if d.config["parent"] != "" && !shared.PathExists(fmt.Sprintf("/sys/class/net/%s", d.config["parent"])) {
		return fmt.Errorf("Parent device '%s' doesn't exist", d.config["parent"])
	}

	if d.config["parent"] == "" && d.config["vlan"] != "" {
		return fmt.Errorf("The vlan setting can only be used when combined with a parent interface")
	}

	extensions := d.state.OS.LXCFeatures
	if !extensions["network_veth_router"] || !extensions["network_l2proxy"] {
		return fmt.Errorf("Requires liblxc has following API extensions: network_veth_router, network_l2proxy")
	}

	// Check necessary sysctls are configured for use with l2proxy parent for routed mode.
	if d.config["parent"] != "" && d.config["ipv4.address"] != "" {
		ipv4FwdPath := fmt.Sprintf("net/ipv4/conf/%s/forwarding", d.config["parent"])
		sysctlVal, err := util.SysctlGet(ipv4FwdPath)
		if err != nil || sysctlVal != "1\n" {
			return fmt.Errorf("Error reading net sysctl %s: %v", ipv4FwdPath, err)
		}
		if sysctlVal != "1\n" {
			return fmt.Errorf("Routed mode requires sysctl net.ipv4.conf.%s.forwarding=1", d.config["parent"])
		}
	}

	// Check necessary sysctls are configured for use with l2proxy parent for routed mode.
	if d.config["parent"] != "" && d.config["ipv6.address"] != "" {
		// net.ipv6.conf.all.forwarding=1 is required to enable general packet forwarding for IPv6.
		ipv6FwdPath := fmt.Sprintf("net/ipv6/conf/%s/forwarding", "all")
		sysctlVal, err := util.SysctlGet(ipv6FwdPath)
		if err != nil {
			return fmt.Errorf("Error reading net sysctl %s: %v", ipv6FwdPath, err)
		}
		if sysctlVal != "1\n" {
			return fmt.Errorf("Routed mode requires sysctl net.ipv6.conf.%s.forwarding=1", "all")
		}

		ipv6FwdPath = fmt.Sprintf("net/ipv6/conf/%s/forwarding", d.config["parent"])
		sysctlVal, err = util.SysctlGet(ipv6FwdPath)
		if err != nil {
			return fmt.Errorf("Error reading net sysctl %s: %v", ipv6FwdPath, err)
		}
		if sysctlVal != "1\n" {
			return fmt.Errorf("Routed mode requires sysctl net.ipv6.conf.%s.forwarding=1", d.config["parent"])
		}

		// net.ipv6.conf.all.proxy_ndp=1 is needed otherwise unicast neighbour solicitations are rejected.
		// This causes periodic latency spikes every 15-20s as the neighbour has to resort to using
		// multicast NDP resolution and expires the previous neighbour entry.
		ipv6ProxyNdpPath := fmt.Sprintf("net/ipv6/conf/%s/proxy_ndp", "all")
		sysctlVal, err = util.SysctlGet(ipv6ProxyNdpPath)
		if err != nil {
			return fmt.Errorf("Error reading net sysctl %s: %v", ipv6ProxyNdpPath, err)
		}
		if sysctlVal != "1\n" {
			return fmt.Errorf("Routed mode requires sysctl net.ipv6.conf.%s.proxy_ndp=1", "all")
		}

		ipv6ProxyNdpPath = fmt.Sprintf("net/ipv6/conf/%s/proxy_ndp", d.config["parent"])
		sysctlVal, err = util.SysctlGet(ipv6ProxyNdpPath)
		if err != nil {
			return fmt.Errorf("Error reading net sysctl %s: %v", ipv6ProxyNdpPath, err)
		}
		if sysctlVal != "1\n" {
			return fmt.Errorf("Routed mode requires sysctl net.ipv6.conf.%s.proxy_ndp=1", d.config["parent"])
		}
	}

	return nil
}

// Start is run when the instance is starting up (Routed mode doesn't support hot plugging).
func (d *nicRouted) Start() (*deviceConfig.RunConfig, error) {
	err := d.validateEnvironment()
	if err != nil {
		return nil, err
	}

	// Lock to avoid issues with containers starting in parallel.
	networkCreateSharedDeviceLock.Lock()
	defer networkCreateSharedDeviceLock.Unlock()

	saveData := make(map[string]string)

	// Decide which parent we should use based on VLAN setting.
	parentName := ""
	if d.config["parent"] != "" {
		parentName = network.GetHostDevice(d.config["parent"], d.config["vlan"])

		statusDev, err := networkCreateVlanDeviceIfNeeded(d.state, d.config["parent"], parentName, d.config["vlan"])
		if err != nil {
			return nil, err
		}

		// Record whether we created this device or not so it can be removed on stop.
		saveData["last_state.created"] = fmt.Sprintf("%t", statusDev != "existing")

		// If we created a VLAN interface, we need to setup the sysctls on that interface.
		if statusDev == "created" {
			err := d.setupParentSysctls(parentName)
			if err != nil {
				return nil, err
			}
		}
	}

	hostName := d.config["host_name"]
	if hostName == "" {
		hostName = networkRandomDevName("veth")
	}
	saveData["host_name"] = hostName

	err = d.volatileSet(saveData)
	if err != nil {
		return nil, err
	}

	runConf := deviceConfig.RunConfig{}
	nic := []deviceConfig.RunConfigItem{
		{Key: "name", Value: d.config["name"]},
		{Key: "type", Value: "veth"},
		{Key: "flags", Value: "up"},
		{Key: "veth.mode", Value: "router"},
		{Key: "veth.pair", Value: saveData["host_name"]},
	}

	// If there is a designated parent interface, activate the layer2 proxy mode to advertise
	// the instance's IPs over that interface using proxy APR/NDP.
	if parentName != "" {
		nic = append(nic,
			deviceConfig.RunConfigItem{Key: "l2proxy", Value: "1"},
			deviceConfig.RunConfigItem{Key: "link", Value: parentName},
		)
	}

	if d.config["mtu"] != "" {
		nic = append(nic, deviceConfig.RunConfigItem{Key: "mtu", Value: d.config["mtu"]})
	}

	if d.config["ipv4.address"] != "" {
		for _, addr := range strings.Split(d.config["ipv4.address"], ",") {
			addr = strings.TrimSpace(addr)
			nic = append(nic, deviceConfig.RunConfigItem{Key: "ipv4.address", Value: fmt.Sprintf("%s/32", addr)})
		}

		if nicHasAutoGateway(d.config["ipv4.gateway"]) {
			// Use a fixed link-local address as the next-hop default gateway.
			nic = append(nic, deviceConfig.RunConfigItem{Key: "ipv4.gateway", Value: d.ipv4HostAddress()})
		}
	}

	if d.config["ipv6.address"] != "" {
		for _, addr := range strings.Split(d.config["ipv6.address"], ",") {
			addr = strings.TrimSpace(addr)
			nic = append(nic, deviceConfig.RunConfigItem{Key: "ipv6.address", Value: fmt.Sprintf("%s/128", addr)})
		}

		if nicHasAutoGateway(d.config["ipv6.gateway"]) {
			// Use a fixed link-local address as the next-hop default gateway.
			nic = append(nic, deviceConfig.RunConfigItem{Key: "ipv6.gateway", Value: d.ipv6HostAddress()})
		}
	}

	runConf.NetworkInterface = nic
	runConf.PostHooks = append(runConf.PostHooks, d.postStart)
	return &runConf, nil
}

// setupParentSysctls configures the required sysctls on the parent to allow l2proxy to work.
// Because of our policy not to modify sysctls on existing interfaces, this should only be called
// if we created the parent interface.
func (d *nicRouted) setupParentSysctls(parentName string) error {
	if d.config["ipv4.address"] != "" {
		// Set necessary sysctls for use with l2proxy parent in routed mode.
		ipv4FwdPath := fmt.Sprintf("net/ipv4/conf/%s/forwarding", parentName)
		err := util.SysctlSet(ipv4FwdPath, "1")
		if err != nil {
			return fmt.Errorf("Error setting net sysctl %s: %v", ipv4FwdPath, err)
		}
	}

	if d.config["ipv6.address"] != "" {
		// Set necessary sysctls use with l2proxy parent in routed mode.
		ipv6FwdPath := fmt.Sprintf("net/ipv6/conf/%s/forwarding", parentName)
		err := util.SysctlSet(ipv6FwdPath, "1")
		if err != nil {
			return fmt.Errorf("Error reading net sysctl %s: %v", ipv6FwdPath, err)
		}

		ipv6ProxyNdpPath := fmt.Sprintf("net/ipv6/conf/%s/proxy_ndp", parentName)
		err = util.SysctlSet(ipv6ProxyNdpPath, "1")
		if err != nil {
			return fmt.Errorf("Error reading net sysctl %s: %v", ipv6ProxyNdpPath, err)
		}
	}

	return nil
}

// postStart is run after the instance is started.
func (d *nicRouted) postStart() error {
	v := d.volatileGet()

	// If host_name is defined (and it should be), then we add the dummy link-local gateway IPs
	// to the host end of the veth pair. This ensures that liveness detection of the gateways
	// inside the instance work and ensure that traffic doesn't periodically halt whilst ARP/NDP
	// is re-detected.
	if v["host_name"] != "" {
		// Attempt to disable IPv6 router advertisement acceptance.
		err := util.SysctlSet(fmt.Sprintf("net/ipv6/conf/%s/accept_ra", v["host_name"]), "0")
		if err != nil && !os.IsNotExist(err) {
			return err
		}

		if d.config["ipv4.address"] != "" {
			_, err := shared.RunCommand("ip", "-4", "addr", "add", fmt.Sprintf("%s/32", d.ipv4HostAddress()), "dev", v["host_name"])
			if err != nil {
				return err
			}
		}

		if d.config["ipv6.address"] != "" {
			_, err := shared.RunCommand("ip", "-6", "addr", "add", fmt.Sprintf("%s/128", d.ipv6HostAddress()), "dev", v["host_name"])
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// Stop is run when the device is removed from the instance.
func (d *nicRouted) Stop() (*deviceConfig.RunConfig, error) {
	runConf := deviceConfig.RunConfig{
		PostHooks: []func() error{d.postStop},
	}

	return &runConf, nil
}

// postStop is run after the device is removed from the instance.
func (d *nicRouted) postStop() error {
	defer d.volatileSet(map[string]string{
		"last_state.created": "",
		"host_name":          "",
	})

	v := d.volatileGet()

	// This will delete the parent interface if we created it for VLAN parent.
	if shared.IsTrue(v["last_state.created"]) {
		parentName := network.GetHostDevice(d.config["parent"], d.config["vlan"])
		err := networkRemoveInterfaceIfNeeded(d.state, parentName, d.inst, d.config["parent"], d.config["vlan"])
		if err != nil {
			return err
		}
	}

	return nil
}

func (d *nicRouted) ipv4HostAddress() string {
	if d.config["ipv4.host_address"] != "" {
		return d.config["ipv4.host_address"]
	}

	return nicRoutedIPv4GW
}

func (d *nicRouted) ipv6HostAddress() string {
	if d.config["ipv6.host_address"] != "" {
		return d.config["ipv6.host_address"]
	}

	return nicRoutedIPv6GW
}
