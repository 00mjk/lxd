package network

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/pkg/errors"

	lxd "github.com/lxc/lxd/client"
	"github.com/lxc/lxd/lxd/cluster"
	"github.com/lxc/lxd/lxd/cluster/request"
	"github.com/lxc/lxd/lxd/db"
	"github.com/lxc/lxd/lxd/project"
	"github.com/lxc/lxd/lxd/state"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	log "github.com/lxc/lxd/shared/log15"
	"github.com/lxc/lxd/shared/logger"
	"github.com/lxc/lxd/shared/logging"
	"github.com/lxc/lxd/shared/validate"
)

// Info represents information about a network driver.
type Info struct {
	Projects           bool // Indicates if driver can be used in network enabled projects.
	NodeSpecificConfig bool // Whether driver has cluster node specific config as a prerequisite for creation.
}

// common represents a generic LXD network.
type common struct {
	logger      logger.Logger
	state       *state.State
	id          int64
	project     string
	name        string
	description string
	config      map[string]string
	status      string
	managed     bool
	nodes       map[int64]db.NetworkNode
}

// init initialise internal variables.
func (n *common) init(state *state.State, id int64, projectName string, netInfo *api.Network, netNodes map[int64]db.NetworkNode) {
	n.logger = logging.AddContext(logger.Log, log.Ctx{"project": projectName, "driver": netInfo.Type, "network": netInfo.Name})
	n.id = id
	n.project = projectName
	n.name = netInfo.Name
	n.config = netInfo.Config
	n.state = state
	n.description = netInfo.Description
	n.status = netInfo.Status
	n.managed = netInfo.Managed
	n.nodes = netNodes
}

// FillConfig fills requested config with any default values, by default this is a no-op.
func (n *common) FillConfig(config map[string]string) error {
	return nil
}

// validationRules returns a map of config rules common to all drivers.
func (n *common) validationRules() map[string]func(string) error {
	return map[string]func(string) error{}
}

// validate a network config against common rules and optional driver specific rules.
func (n *common) validate(config map[string]string, driverRules map[string]func(value string) error) error {
	checkedFields := map[string]struct{}{}

	// Get rules common for all drivers.
	rules := n.validationRules()

	// Merge driver specific rules into common rules.
	for field, validator := range driverRules {
		rules[field] = validator
	}

	// Run the validator against each field.
	for k, validator := range rules {
		checkedFields[k] = struct{}{} //Mark field as checked.
		err := validator(config[k])
		if err != nil {
			return errors.Wrapf(err, "Invalid value for network %q option %q", n.name, k)
		}
	}

	// Look for any unchecked fields, as these are unknown fields and validation should fail.
	for k := range config {
		_, checked := checkedFields[k]
		if checked {
			continue
		}

		// User keys are not validated.
		if shared.IsUserConfig(k) {
			continue
		}

		return fmt.Errorf("Invalid option for network %q option %q", n.name, k)
	}

	return nil
}

// ValidateName validates network name.
func (n *common) ValidateName(name string) error {
	err := validate.IsURLSegmentSafe(name)
	if err != nil {
		return err
	}

	if strings.Contains(name, ":") {
		return fmt.Errorf("Cannot contain %q", ":")
	}

	return nil
}

// ID returns the network ID.
func (n *common) ID() int64 {
	return n.id
}

// Name returns the network name.
func (n *common) Name() string {
	return n.name
}

// Project returns the network project.
func (n *common) Project() string {
	return n.project
}

// Description returns the network description.
func (n *common) Description() string {
	return n.description
}

// Status returns the network status.
func (n *common) Status() string {
	return n.status
}

// LocalStatus returns network status of the local cluster member.
func (n *common) LocalStatus() string {
	node, exists := n.nodes[n.state.Cluster.GetNodeID()]
	if !exists {
		return api.NetworkStatusUnknown
	}

	return db.NetworkStateToAPIStatus(node.State)
}

// Config returns the network config.
func (n *common) Config() map[string]string {
	return n.config
}

func (n *common) IsManaged() bool {
	return n.managed
}

// Config returns the common network driver info.
func (n *common) Info() Info {
	return Info{
		Projects:           false,
		NodeSpecificConfig: true,
	}
}

// IsUsed returns whether the network is used by any instances or profiles.
func (n *common) IsUsed() (bool, error) {
	usedBy, err := UsedBy(n.state, n.project, n.name, true)
	if err != nil {
		return false, err
	}

	return len(usedBy) > 0, nil
}

// DHCPv4Subnet returns nil always.
func (n *common) DHCPv4Subnet() *net.IPNet {
	return nil
}

// DHCPv6Subnet returns nil always.
func (n *common) DHCPv6Subnet() *net.IPNet {
	return nil
}

// DHCPv4Ranges returns a parsed set of DHCPv4 ranges for this network.
func (n *common) DHCPv4Ranges() []shared.IPRange {
	dhcpRanges := make([]shared.IPRange, 0)
	if n.config["ipv4.dhcp.ranges"] != "" {
		for _, r := range strings.Split(n.config["ipv4.dhcp.ranges"], ",") {
			parts := strings.SplitN(strings.TrimSpace(r), "-", 2)
			if len(parts) == 2 {
				startIP := net.ParseIP(parts[0])
				endIP := net.ParseIP(parts[1])
				dhcpRanges = append(dhcpRanges, shared.IPRange{
					Start: startIP.To4(),
					End:   endIP.To4(),
				})
			}
		}
	}

	return dhcpRanges
}

// DHCPv6Ranges returns a parsed set of DHCPv6 ranges for this network.
func (n *common) DHCPv6Ranges() []shared.IPRange {
	dhcpRanges := make([]shared.IPRange, 0)
	if n.config["ipv6.dhcp.ranges"] != "" {
		for _, r := range strings.Split(n.config["ipv6.dhcp.ranges"], ",") {
			parts := strings.SplitN(strings.TrimSpace(r), "-", 2)
			if len(parts) == 2 {
				startIP := net.ParseIP(parts[0])
				endIP := net.ParseIP(parts[1])
				dhcpRanges = append(dhcpRanges, shared.IPRange{
					Start: startIP.To16(),
					End:   endIP.To16(),
				})
			}
		}
	}

	return dhcpRanges
}

// update the internal config variables, and if not cluster notification, notifies all nodes and updates database.
func (n *common) update(applyNetwork api.NetworkPut, targetNode string, clientType request.ClientType) error {
	// Update internal config before database has been updated (so that if update is a notification we apply
	// the config being supplied and not that in the database).
	n.description = applyNetwork.Description
	n.config = applyNetwork.Config

	// If this update isn't coming via a cluster notification itself, then notify all nodes of change and then
	// update the database.
	if clientType != request.ClientTypeNotifier {
		if targetNode == "" {
			// Notify all other nodes to update the network if no target specified.
			notifier, err := cluster.NewNotifier(n.state, n.state.Endpoints.NetworkCert(), n.state.ServerCert(), cluster.NotifyAll)
			if err != nil {
				return err
			}

			sendNetwork := applyNetwork
			sendNetwork.Config = make(map[string]string)
			for k, v := range applyNetwork.Config {
				// Don't forward node specific keys (these will be merged in on recipient node).
				if shared.StringInSlice(k, db.NodeSpecificNetworkConfig) {
					continue
				}

				sendNetwork.Config[k] = v
			}

			err = notifier(func(client lxd.InstanceServer) error {
				return client.UseProject(n.project).UpdateNetwork(n.name, sendNetwork, "")
			})
			if err != nil {
				return err
			}
		}

		// Update the database.
		err := n.state.Cluster.UpdateNetwork(n.project, n.name, applyNetwork.Description, applyNetwork.Config)
		if err != nil {
			return err
		}

	}

	return nil
}

// configChanged compares supplied new config with existing config. Returns a boolean indicating if differences in
// the config or description were found (and the database record needs updating), and a list of non-user config
// keys that have changed, and a copy of the current internal network config that can be used to revert if needed.
func (n *common) configChanged(newNetwork api.NetworkPut) (bool, []string, api.NetworkPut, error) {
	// Backup the current state.
	oldNetwork := api.NetworkPut{
		Description: n.description,
		Config:      map[string]string{},
	}

	err := shared.DeepCopy(&n.config, &oldNetwork.Config)
	if err != nil {
		return false, nil, oldNetwork, err
	}

	// Diff the configurations.
	changedKeys := []string{}
	dbUpdateNeeded := false

	if newNetwork.Description != n.description {
		dbUpdateNeeded = true
	}

	for k, v := range oldNetwork.Config {
		if v != newNetwork.Config[k] {
			dbUpdateNeeded = true

			// Add non-user changed key to list of changed keys.
			if !strings.HasPrefix(k, "user.") && !shared.StringInSlice(k, changedKeys) {
				changedKeys = append(changedKeys, k)
			}
		}
	}

	for k, v := range newNetwork.Config {
		if v != oldNetwork.Config[k] {
			dbUpdateNeeded = true

			// Add non-user changed key to list of changed keys.
			if !strings.HasPrefix(k, "user.") && !shared.StringInSlice(k, changedKeys) {
				changedKeys = append(changedKeys, k)
			}
		}
	}

	return dbUpdateNeeded, changedKeys, oldNetwork, nil
}

// rename the network directory, update database record and update internal variables.
func (n *common) rename(newName string) error {
	// Clear new directory if exists.
	if shared.PathExists(shared.VarPath("networks", newName)) {
		os.RemoveAll(shared.VarPath("networks", newName))
	}

	// Rename directory to new name.
	if shared.PathExists(shared.VarPath("networks", n.name)) {
		err := os.Rename(shared.VarPath("networks", n.name), shared.VarPath("networks", newName))
		if err != nil {
			return err
		}
	}

	// Rename the database entry.
	err := n.state.Cluster.RenameNetwork(n.project, n.name, newName)
	if err != nil {
		return err
	}

	// Reinitialise internal name variable and logger context with new name.
	n.name = newName

	return nil
}

// delete the network from the database if clusterNotification is false.
func (n *common) delete(clientType request.ClientType) error {
	// Cleanup storage.
	if shared.PathExists(shared.VarPath("networks", n.name)) {
		os.RemoveAll(shared.VarPath("networks", n.name))
	}

	return nil
}

// Create is a no-op.
func (n *common) Create(clientType request.ClientType) error {
	n.logger.Debug("Create", log.Ctx{"clientType": clientType, "config": n.config})
	return nil
}

// HandleHeartbeat is a no-op.
func (n *common) HandleHeartbeat(heartbeatData *cluster.APIHeartbeat) error {
	return nil
}

// notifyDependentNetworks allows any dependent networks to apply changes to themselves when this network changes.
func (n *common) notifyDependentNetworks(changedKeys []string) {
	if n.Project() != project.Default {
		return // Only networks in the default project can be used as dependent networks.
	}

	// Get a list of projects.
	var err error
	var projectNames []string

	err = n.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		projectNames, err = tx.GetProjectNames()
		return err
	})
	if err != nil {
		n.logger.Error("Failed to load projects", log.Ctx{"err": err})
		return
	}

	for _, projectName := range projectNames {
		// Get a list of managed networks in project.
		depNets, err := n.state.Cluster.GetCreatedNetworks(projectName)
		if err != nil {
			n.logger.Error("Failed to load networks in project", log.Ctx{"project": projectName, "err": err})
			continue // Continue to next project.
		}

		for _, depName := range depNets {
			depNet, err := LoadByName(n.state, projectName, depName)
			if err != nil {
				n.logger.Error("Failed to load dependent network", log.Ctx{"project": projectName, "dependentNetwork": depName, "err": err})
				continue // Continue to next network.
			}

			if depNet.Config()["network"] != n.Name() {
				continue // Skip network, as does not depend on our network.
			}

			err = depNet.handleDependencyChange(n.Name(), n.Config(), changedKeys)
			if err != nil {
				n.logger.Error("Failed notifying dependent network", log.Ctx{"project": projectName, "dependentNetwork": depName, "err": err})
				continue // Continue to next network.
			}
		}
	}
}

// handleDependencyChange is a placeholder for networks that don't need to handle changes from dependent networks.
func (n *common) handleDependencyChange(netName string, netConfig map[string]string, changedKeys []string) error {
	return nil
}

// bgpValidate
func (n *common) bgpValidationRules(config map[string]string) (map[string]func(value string) error, error) {
	rules := map[string]func(value string) error{}
	for k := range config {
		// BGP keys have the peer name in their name, extract the suffix.
		if !strings.HasPrefix(k, "bgp.") {
			continue
		}

		// Validate remote name in key.
		fields := strings.Split(k, ".")
		if len(fields) != 4 {
			return nil, fmt.Errorf("Invalid network configuration key: %s", k)
		}

		bgpKey := fields[3]

		// Add the correct validation rule for the dynamic field based on last part of key.
		switch bgpKey {
		case "address":
			rules[k] = validate.Optional(validate.IsNetworkAddress)
		case "asn":
			rules[k] = validate.Optional(validate.IsInRange(1, 4294967294))
		case "password":
			rules[k] = validate.Optional(validate.IsAny)
		}
	}

	return rules, nil
}

// bgpSetup initializes BGP peers and prefixes.
func (n *common) bgpSetup(oldConfig map[string]string) error {
	err := n.bgpSetupPeers(oldConfig)
	if err != nil {
		return err
	}

	err = n.bgpSetupPrefixes(oldConfig)
	if err != nil {
		return err
	}

	return nil
}

// bgpClear initializes BGP peers and prefixes.
func (n *common) bgpClear(config map[string]string) error {
	// Clear all peers.
	err := n.bgpClearPeers(config)
	if err != nil {
		return err
	}

	// Clear all prefixes.
	err = n.bgpClearPrefixes()
	if err != nil {
		return err
	}

	return nil
}

// bgpClearPeers removes all BGP peers on the network.
func (n *common) bgpClearPeers(config map[string]string) error {
	peers := n.bgpGetPeers(config)
	for _, peer := range peers {
		// Remove the peer.
		fields := strings.Split(peer, ",")
		err := n.state.BGP.RemovePeer(net.ParseIP(fields[0]))
		if err != nil {
			return err
		}
	}

	return nil
}

// bgpClearPrefixes removes all BGP prefixes for the network.
func (n *common) bgpClearPrefixes() error {
	err := n.state.BGP.RemovePrefixByOwner(fmt.Sprintf("network_%d", n.id))
	if err != nil {
		return err
	}

	return nil
}

// bgpSetupPeers updates the list of BGP peers.
func (n *common) bgpSetupPeers(oldConfig map[string]string) error {
	// Setup BGP (and handled config changes).
	newPeers := n.bgpGetPeers(n.config)
	oldPeers := n.bgpGetPeers(oldConfig)

	// Remove old peers.
	for _, peer := range oldPeers {
		if shared.StringInSlice(peer, newPeers) {
			continue
		}

		// Remove old peer.
		fields := strings.Split(peer, ",")
		err := n.state.BGP.RemovePeer(net.ParseIP(fields[0]))
		if err != nil {
			return err
		}
	}

	// Add new peers.
	for _, peer := range newPeers {
		if shared.StringInSlice(peer, oldPeers) {
			continue
		}

		// Add new peer.
		fields := strings.Split(peer, ",")
		asn, err := strconv.ParseUint(fields[1], 10, 32)
		if err != nil {
			return err
		}

		err = n.state.BGP.AddPeer(net.ParseIP(fields[0]), uint32(asn), fields[2])
		if err != nil {
			return err
		}
	}

	return nil
}

// bgpSetupPrefixes refreshes the prefix list for the network.
func (n *common) bgpSetupPrefixes(oldConfig map[string]string) error {
	// Parse nexthop configuration.
	nexthopV4 := net.ParseIP(n.config["bgp.ipv4.nexthop"])
	if nexthopV4 == nil {
		nexthopV4 = net.ParseIP(n.config["volatile.network.ipv4.address"])
		if nexthopV4 == nil {
			nexthopV4 = net.ParseIP("0.0.0.0")
		}
	}

	nexthopV6 := net.ParseIP(n.config["bgp.ipv6.nexthop"])
	if nexthopV6 == nil {
		nexthopV6 = net.ParseIP(n.config["volatile.network.ipv6.address"])
		if nexthopV6 == nil {
			nexthopV6 = net.ParseIP("::")
		}
	}

	// Clear existing prefixes.
	bgpOwner := fmt.Sprintf("network_%d", n.id)
	if oldConfig != nil {
		err := n.state.BGP.RemovePrefixByOwner(bgpOwner)
		if err != nil {
			return err
		}
	}

	// Add the new prefixes.
	if shared.IsTrue(n.config["ipv4.nat"]) {
		if n.config["ipv4.nat.address"] != "" {
			_, subnet, err := net.ParseCIDR(fmt.Sprintf("%s/32", n.config["ipv4.nat.address"]))
			if err != nil {
				return err
			}

			err = n.state.BGP.AddPrefix(*subnet, nexthopV4, bgpOwner)
			if err != nil {
				return err
			}
		}
	} else if !shared.StringInSlice(n.config["ipv4.address"], []string{"", "none"}) {
		_, subnet, err := net.ParseCIDR(n.config["ipv4.address"])
		if err != nil {
			return err
		}

		err = n.state.BGP.AddPrefix(*subnet, nexthopV4, bgpOwner)
		if err != nil {
			return err
		}
	}

	if shared.IsTrue(n.config["ipv6.nat"]) {
		if n.config["ipv6.nat.address"] != "" {
			_, subnet, err := net.ParseCIDR(fmt.Sprintf("%s/128", n.config["ipv6.nat.address"]))
			if err != nil {
				return err
			}

			err = n.state.BGP.AddPrefix(*subnet, nexthopV6, bgpOwner)
			if err != nil {
				return err
			}
		}
	} else if !shared.StringInSlice(n.config["ipv6.address"], []string{"", "none"}) {
		_, subnet, err := net.ParseCIDR(n.config["ipv6.address"])
		if err != nil {
			return err
		}

		err = n.state.BGP.AddPrefix(*subnet, nexthopV6, bgpOwner)
		if err != nil {
			return err
		}
	}

	return nil
}

// bgpGetPeers returns a list of strings representing the BGP peers.
func (n *common) bgpGetPeers(config map[string]string) []string {
	// Get a list of peer names.
	peerNames := []string{}
	for k := range config {
		if !strings.HasPrefix(k, "bgp.peers.") {
			continue
		}

		fields := strings.Split(k, ".")
		if !shared.StringInSlice(fields[2], peerNames) {
			peerNames = append(peerNames, fields[2])
		}
	}

	// Build up a list of peer strings.
	peers := []string{}
	for _, peerName := range peerNames {
		peerAddress := config[fmt.Sprintf("bgp.peers.%s.address", peerName)]
		peerASN := config[fmt.Sprintf("bgp.peers.%s.asn", peerName)]
		peerPassword := config[fmt.Sprintf("bgp.peers.%s.password", peerName)]

		if peerAddress != "" && peerASN != "" {
			peers = append(peers, fmt.Sprintf("%s,%s,%s", peerAddress, peerASN, peerPassword))
		}
	}

	return peers
}
