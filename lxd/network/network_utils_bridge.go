package network

import (
	"fmt"
	"io/ioutil"
	"strings"

	"github.com/lxc/lxd/lxd/ip"
	"github.com/lxc/lxd/lxd/network/openvswitch"
	"github.com/lxc/lxd/shared"
)

// BridgeVLANFilteringStatus returns whether VLAN filtering is enabled on a bridge interface.
func BridgeVLANFilteringStatus(interfaceName string) (string, error) {
	content, err := ioutil.ReadFile(fmt.Sprintf("/sys/class/net/%s/bridge/vlan_filtering", interfaceName))
	if err != nil {
		return "", fmt.Errorf("Failed getting bridge VLAN status for %q: %w", interfaceName, err)
	}

	return strings.TrimSpace(fmt.Sprintf("%s", content)), nil
}

// BridgeVLANFilterSetStatus sets the status of VLAN filtering on a bridge interface.
func BridgeVLANFilterSetStatus(interfaceName string, status string) error {
	err := ioutil.WriteFile(fmt.Sprintf("/sys/class/net/%s/bridge/vlan_filtering", interfaceName), []byte(status), 0)
	if err != nil {
		return fmt.Errorf("Failed enabling VLAN filtering on bridge %q: %w", interfaceName, err)
	}

	return nil
}

// BridgeVLANDefaultPVID returns the VLAN default port VLAN ID (PVID).
func BridgeVLANDefaultPVID(interfaceName string) (string, error) {
	content, err := ioutil.ReadFile(fmt.Sprintf("/sys/class/net/%s/bridge/default_pvid", interfaceName))
	if err != nil {
		return "", fmt.Errorf("Failed getting bridge VLAN default PVID for %q: %w", interfaceName, err)
	}

	return strings.TrimSpace(fmt.Sprintf("%s", content)), nil
}

// BridgeVLANSetDefaultPVID sets the VLAN default port VLAN ID (PVID).
func BridgeVLANSetDefaultPVID(interfaceName string, vlanID string) error {
	err := ioutil.WriteFile(fmt.Sprintf("/sys/class/net/%s/bridge/default_pvid", interfaceName), []byte(vlanID), 0)
	if err != nil {
		return fmt.Errorf("Failed setting bridge VLAN default PVID for %q: %w", interfaceName, err)
	}

	return nil
}

// IsNativeBridge returns whether the bridge name specified is a Linux native bridge.
func IsNativeBridge(bridgeName string) bool {
	return shared.PathExists(fmt.Sprintf("/sys/class/net/%s/bridge", bridgeName))
}

// AttachInterface attaches an interface to a bridge.
func AttachInterface(bridgeName string, devName string) error {
	if IsNativeBridge(bridgeName) {
		link := &ip.Link{Name: devName}
		err := link.SetMaster(bridgeName)
		if err != nil {
			return err
		}
	} else {
		ovs := openvswitch.NewOVS()
		err := ovs.BridgePortAdd(bridgeName, devName, true)
		if err != nil {
			return err
		}
	}

	return nil
}

// DetachInterface detaches an interface from a bridge.
func DetachInterface(bridgeName string, devName string) error {
	if IsNativeBridge(bridgeName) {
		link := &ip.Link{Name: devName}
		err := link.SetNoMaster()
		if err != nil {
			return err
		}
	} else {
		ovs := openvswitch.NewOVS()
		err := ovs.BridgePortDelete(bridgeName, devName)
		if err != nil {
			return err
		}
	}

	return nil
}
