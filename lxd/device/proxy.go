package device

import (
	"bufio"
	"bytes"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
	liblxc "gopkg.in/lxc/go-lxc.v2"

	deviceConfig "github.com/lxc/lxd/lxd/device/config"
	"github.com/lxc/lxd/lxd/instance"
	"github.com/lxc/lxd/lxd/instance/instancetype"
	"github.com/lxc/lxd/lxd/project"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/logger"
)

type proxy struct {
	deviceCommon
}

type proxyProcInfo struct {
	listenPid      string
	connectPid     string
	connectAddr    string
	listenAddr     string
	listenAddrGID  string
	listenAddrUID  string
	listenAddrMode string
	securityUID    string
	securityGID    string
	proxyProtocol  string
}

// validateConfig checks the supplied config for correctness.
func (d *proxy) validateConfig(instConf instance.ConfigReader) error {
	if !instanceSupported(instConf.Type(), instancetype.Container) {
		return ErrUnsupportedDevType
	}

	validateAddr := func(input string) error {
		_, err := ProxyParseAddr(input)
		return err
	}

	// Supported bind types are: "host" or "guest" (and "container", a legacy option equivalent to "guest").
	// If an empty value is supplied the default behavior is to assume "host" bind mode.
	validateBind := func(input string) error {
		if !shared.StringInSlice(d.config["bind"], []string{"", "host", "guest", "container"}) {
			return fmt.Errorf("Invalid binding side given. Must be \"host\" or \"guest\"")
		}

		return nil
	}

	rules := map[string]func(string) error{
		"listen":         validateAddr,
		"connect":        validateAddr,
		"bind":           validateBind,
		"mode":           unixValidOctalFileMode,
		"nat":            shared.IsBool,
		"gid":            unixValidUserID,
		"uid":            unixValidUserID,
		"security.uid":   unixValidUserID,
		"security.gid":   unixValidUserID,
		"proxy_protocol": shared.IsBool,
	}

	err := d.config.Validate(rules)
	if err != nil {
		return err
	}

	listenAddr, err := ProxyParseAddr(d.config["listen"])
	if err != nil {
		return err
	}

	connectAddr, err := ProxyParseAddr(d.config["connect"])
	if err != nil {
		return err
	}

	if len(connectAddr.Addr) > len(listenAddr.Addr) {
		// Cannot support single port -> multiple port
		return fmt.Errorf("Cannot map a single port to multiple ports")
	}

	if shared.IsTrue(d.config["proxy_protocol"]) && (!strings.HasPrefix(d.config["connect"], "tcp") || shared.IsTrue(d.config["nat"])) {
		return fmt.Errorf("The PROXY header can only be sent to tcp servers in non-nat mode")
	}

	if (!strings.HasPrefix(d.config["listen"], "unix:") || strings.HasPrefix(d.config["listen"], "unix:@")) &&
		(d.config["uid"] != "" || d.config["gid"] != "" || d.config["mode"] != "") {
		return fmt.Errorf("Only proxy devices for non-abstract unix sockets can carry uid, gid, or mode properties")
	}

	if shared.IsTrue(d.config["nat"]) {
		if d.config["bind"] != "" && d.config["bind"] != "host" {
			return fmt.Errorf("Only host-bound proxies can use NAT")
		}

		// Support TCP <-> TCP and UDP <-> UDP
		if listenAddr.ConnType == "unix" || connectAddr.ConnType == "unix" ||
			listenAddr.ConnType != connectAddr.ConnType {
			return fmt.Errorf("Proxying %s <-> %s is not supported when using NAT",
				listenAddr.ConnType, connectAddr.ConnType)
		}
	}

	return nil
}

// validateEnvironment checks the runtime environment for correctness.
func (d *proxy) validateEnvironment() error {
	if d.name == "" {
		return fmt.Errorf("Device name cannot be empty")
	}

	return nil
}

// Start is run when the device is added to the container.
func (d *proxy) Start() (*deviceConfig.RunConfig, error) {
	err := d.validateEnvironment()
	if err != nil {
		return nil, err
	}

	// Proxy devices have to be setup once the container is running.
	runConf := deviceConfig.RunConfig{}
	runConf.PostHooks = []func() error{
		func() error {
			if shared.IsTrue(d.config["nat"]) {
				return d.setupNAT()
			}

			proxyValues, err := d.setupProxyProcInfo()
			if err != nil {
				return err
			}

			devFileName := fmt.Sprintf("proxy.%s", d.name)
			pidPath := filepath.Join(d.inst.DevicesPath(), devFileName)
			logFileName := fmt.Sprintf("proxy.%s.log", d.name)
			logPath := filepath.Join(d.inst.LogPath(), logFileName)

			_, err = shared.RunCommand(
				d.state.OS.ExecPath,
				"forkproxy",
				proxyValues.listenPid,
				proxyValues.listenAddr,
				proxyValues.connectPid,
				proxyValues.connectAddr,
				logPath,
				pidPath,
				proxyValues.listenAddrGID,
				proxyValues.listenAddrUID,
				proxyValues.listenAddrMode,
				proxyValues.securityGID,
				proxyValues.securityUID,
				proxyValues.proxyProtocol,
			)
			if err != nil {
				return fmt.Errorf("Error occurred when starting proxy device: %s", err)
			}

			// Poll log file a few times until we see "Started" to indicate successful start.
			for i := 0; i < 10; i++ {
				started, err := d.checkProcStarted(logPath)

				if err != nil {
					return fmt.Errorf("Error occurred when starting proxy device: %s", err)
				}

				if started {
					return nil
				}

				time.Sleep(time.Second)
			}

			return fmt.Errorf("Error occurred when starting proxy device, please look in %s", logPath)
		},
	}

	return &runConf, nil
}

// checkProcStarted checks for the "Started" line in the log file. Returns true if found, false
// if not, and error if any other error occurs.
func (d *proxy) checkProcStarted(logPath string) (bool, error) {
	file, err := os.Open(logPath)
	if err != nil {
		return false, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if line == "Status: Started" {
			return true, nil
		}

		if strings.HasPrefix(line, "Error:") {
			return false, fmt.Errorf("%s", line)
		}
	}

	err = scanner.Err()
	if err != nil {
		return false, err
	}

	return false, nil
}

// Stop is run when the device is removed from the instance.
func (d *proxy) Stop() (*deviceConfig.RunConfig, error) {
	// Remove possible iptables entries
	err := d.state.Firewall.InstanceClearProxyNAT(d.inst.Project(), d.inst.Name(), d.name)
	if err != nil {
		logger.Errorf("Failed to remove proxy NAT filters: %v", err)
	}

	devFileName := fmt.Sprintf("proxy.%s", d.name)
	devPath := filepath.Join(d.inst.DevicesPath(), devFileName)

	if !shared.PathExists(devPath) {
		// There's no proxy process if NAT is enabled
		return nil, nil
	}

	err = d.killProxyProc(devPath)
	if err != nil {
		return nil, err
	}

	return nil, nil
}

func (d *proxy) setupNAT() error {
	listenAddr, err := ProxyParseAddr(d.config["listen"])
	if err != nil {
		return err
	}

	connectAddr, err := ProxyParseAddr(d.config["connect"])
	if err != nil {
		return err
	}

	connectHost, _, err := net.SplitHostPort(connectAddr.Addr[0])
	if err != nil {
		return err
	}

	ipFamily := "ipv4"
	if strings.Contains(connectHost, ":") {
		ipFamily = "ipv6"
	}

	var connectIP net.IP
	var hostName string

	for devName, devConfig := range d.inst.ExpandedDevices() {
		if devConfig["type"] != "nic" || (devConfig["type"] == "nic" && devConfig.NICType() != "bridged") {
			continue
		}

		// Ensure the connect IP matches one of the NIC's static IPs otherwise we could mess with other
		// instance's network traffic. If the wildcard address is supplied as the connect host then the
		// first bridged NIC which has a static IP address defined is selected as the connect host IP.
		if ipFamily == "ipv4" && devConfig["ipv4.address"] != "" {
			if connectHost == devConfig["ipv4.address"] || connectHost == "0.0.0.0" {
				connectIP = net.ParseIP(devConfig["ipv4.address"])
			}
		} else if ipFamily == "ipv6" && devConfig["ipv6.address"] != "" {
			if connectHost == devConfig["ipv6.address"] || connectHost == "::" {
				connectIP = net.ParseIP(devConfig["ipv6.address"])
			}
		}

		if connectIP != nil {
			// Get host_name of device so we can enable hairpin mode on bridge port.
			hostName = d.inst.ExpandedConfig()[fmt.Sprintf("volatile.%s.host_name", devName)]
			break // Found a match, stop searching.
		}
	}

	if connectIP == nil {
		return fmt.Errorf("Proxy connect IP cannot be used with any of the instance NICs static IPs")
	}

	// Override the host part of the connectAddr.Addr to the chosen connect IP.
	for i, addr := range connectAddr.Addr {
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			return err
		}

		if ipFamily == "ipv4" {
			connectAddr.Addr[i] = fmt.Sprintf("%s:%s", connectIP.String(), port)
		} else if ipFamily == "ipv6" {
			// IPv6 addresses need to be enclosed in square brackets.
			connectAddr.Addr[i] = fmt.Sprintf("[%s]:%s", connectIP.String(), port)
		}
	}

	err = d.checkBridgeNetfilterEnabled(ipFamily)
	if err != nil {
		logger.Warnf("Proxy bridge netfilter not enabled: %v. Instances using the bridge will not be able to connect to the proxy's listen IP", err)
	} else {
		if hostName == "" {
			return fmt.Errorf("Proxy cannot find bridge port host_name to enable hairpin mode")
		}

		// br_netfilter is enabled, so we need to enable hairpin mode on instance's bridge port otherwise
		// the instances on the bridge will not be able to connect to the proxy device's listn IP and the
		// NAT rule added by the firewall below to allow instance <-> instance traffic will also not work.
		_, err = shared.RunCommand("bridge", "link", "set", "dev", hostName, "hairpin", "on")
		if err != nil {
			return errors.Wrapf(err, "Error enabling hairpin mode on bridge port %q", hostName)
		}
	}

	err = d.state.Firewall.InstanceSetupProxyNAT(d.inst.Project(), d.inst.Name(), d.name, listenAddr, connectAddr)
	if err != nil {
		return err
	}

	return nil
}

// checkBridgeNetfilterEnabled checks whether the bridge netfilter feature is loaded and enabled.
// If it is not an error is returned. This is needed in order for instances connected to a bridge to access the
// proxy's listen IP on the LXD host, as otherwise the packets from the bridge do not go through the netfilter
// NAT SNAT/MASQUERADE rules.
func (d *proxy) checkBridgeNetfilterEnabled(ipFamily string) error {
	sysctlName := "iptables"
	if ipFamily == "ipv6" {
		sysctlName = "ip6tables"
	}

	sysctlPath := fmt.Sprintf("net/bridge/bridge-nf-call-%s", sysctlName)
	sysctlVal, err := util.SysctlGet(sysctlPath)
	if err != nil {
		return errors.Wrap(err, "br_netfilter not loaded")
	}

	sysctlVal = strings.TrimSpace(sysctlVal)
	if sysctlVal != "1" {
		return fmt.Errorf("br_netfilter sysctl net.bridge.bridge-nf-call-%s=%s", sysctlName, sysctlVal)
	}

	return nil
}

func (d *proxy) rewriteHostAddr(addr string) string {
	fields := strings.SplitN(addr, ":", 2)
	proto := fields[0]
	addr = fields[1]
	if proto == "unix" && !strings.HasPrefix(addr, "@") {
		// Unix non-abstract sockets need to be addressed to the host
		// filesystem, not be scoped inside the LXD snap.
		addr = shared.HostPath(addr)
	}
	return fmt.Sprintf("%s:%s", proto, addr)
}

func (d *proxy) setupProxyProcInfo() (*proxyProcInfo, error) {
	cname := project.Instance(d.inst.Project(), d.inst.Name())
	cc, err := liblxc.NewContainer(cname, d.state.OS.LxcPath)
	if err != nil {
		return nil, err
	}
	defer cc.Release()

	containerPid := strconv.Itoa(cc.InitPid())
	lxdPid := strconv.Itoa(os.Getpid())

	var listenPid, connectPid string

	connectAddr := d.config["connect"]
	listenAddr := d.config["listen"]

	switch d.config["bind"] {
	case "host", "":
		listenPid = lxdPid
		connectPid = containerPid
		listenAddr = d.rewriteHostAddr(listenAddr)
	case "guest", "container":
		listenPid = containerPid
		connectPid = lxdPid
		connectAddr = d.rewriteHostAddr(connectAddr)
	default:
		return nil, fmt.Errorf("Invalid binding side given. Must be \"host\" or \"guest\"")
	}

	listenAddrMode := "0644"
	if d.config["mode"] != "" {
		listenAddrMode = d.config["mode"]
	}

	p := &proxyProcInfo{
		listenPid:      listenPid,
		connectPid:     connectPid,
		connectAddr:    connectAddr,
		listenAddr:     listenAddr,
		listenAddrGID:  d.config["gid"],
		listenAddrUID:  d.config["uid"],
		listenAddrMode: listenAddrMode,
		securityGID:    d.config["security.gid"],
		securityUID:    d.config["security.uid"],
		proxyProtocol:  d.config["proxy_protocol"],
	}

	return p, nil
}

func (d *proxy) killProxyProc(pidPath string) error {
	// Get the contents of the pid file
	contents, err := ioutil.ReadFile(pidPath)
	if err != nil {
		return err
	}
	pidString := strings.TrimSpace(string(contents))

	// Check if the process still exists
	if !shared.PathExists(fmt.Sprintf("/proc/%s", pidString)) {
		os.Remove(pidPath)
		return nil
	}

	// Check if it's forkdns
	cmdArgs, err := ioutil.ReadFile(fmt.Sprintf("/proc/%s/cmdline", pidString))
	if err != nil {
		os.Remove(pidPath)
		return nil
	}

	cmdFields := strings.Split(string(bytes.TrimRight(cmdArgs, string("\x00"))), string(byte(0)))
	if len(cmdFields) < 5 || cmdFields[1] != "forkproxy" {
		os.Remove(pidPath)
		return nil
	}

	// Parse the pid
	pidInt, err := strconv.Atoi(pidString)
	if err != nil {
		return err
	}

	// Actually kill the process
	err = unix.Kill(pidInt, unix.SIGKILL)
	if err != nil {
		return err
	}

	// Cleanup
	os.Remove(pidPath)
	return nil
}
