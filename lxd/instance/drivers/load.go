package drivers

import (
	"fmt"
	"sync"

	"github.com/lxc/lxd/lxd/db"
	"github.com/lxc/lxd/lxd/device"
	deviceConfig "github.com/lxc/lxd/lxd/device/config"
	"github.com/lxc/lxd/lxd/instance"
	"github.com/lxc/lxd/lxd/instance/instancetype"
	"github.com/lxc/lxd/lxd/revert"
	"github.com/lxc/lxd/lxd/state"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/logger"
)

// Instance driver definitions.
var instanceDrivers = map[string]func() instance.Instance{
	"lxc":  func() instance.Instance { return &lxc{} },
	"qemu": func() instance.Instance { return &qemu{} },
}

// DriverStatus definition.
type DriverStatus struct {
	Info      instance.Info
	Warning   *db.Warning
	Supported bool
}

// Supported instance drivers cache variables.
var driverStatusesMu sync.Mutex
var driverStatuses map[instancetype.Type]*DriverStatus

func init() {
	// Expose load to the instance package, to avoid circular imports.
	instance.Load = load

	// Expose validDevices to the instance package, to avoid circular imports.
	instance.ValidDevices = validDevices

	// Expose create to the instance package, to avoid circular imports.
	instance.Create = create
}

// load creates the underlying instance type struct and returns it as an Instance.
func load(s *state.State, args db.InstanceArgs, profiles []api.Profile) (instance.Instance, error) {
	var inst instance.Instance
	var err error

	if args.Type == instancetype.Container {
		inst, err = lxcLoad(s, args, profiles)
	} else if args.Type == instancetype.VM {
		inst, err = qemuLoad(s, args, profiles)
	} else {
		return nil, fmt.Errorf("Invalid instance type for instance %s", args.Name)
	}

	if err != nil {
		return nil, err
	}

	return inst, nil
}

// validDevices validate instance device configs.
func validDevices(state *state.State, projectName string, instanceType instancetype.Type, devices deviceConfig.Devices, expanded bool) error {
	// Empty device list
	if devices == nil {
		return nil
	}

	instConf := &common{
		dbType:       instanceType,
		localDevices: devices.Clone(),
		project:      projectName,
	}

	// In non-expanded validation expensive checks should be avoided.
	if expanded {
		// The devices being validated are already expanded, so just use the same
		// devices clone as we used for the main devices config.
		instConf.expandedDevices = instConf.localDevices
	}

	// Check each device individually using the device package.
	// Use instConf.localDevices so that the cloned config is passed into the driver, so it cannot modify it.
	for name, config := range instConf.localDevices {
		// Enforce a maximum name length of 64 characters (safe maximum allowing use for sockets and other filesystem use).
		if len(name) > 64 {
			return fmt.Errorf("The maximum device name length is 64 characters")
		}

		err := device.Validate(instConf, state, name, config)
		if err != nil {
			return fmt.Errorf("Device validation failed for %q: %w", name, err)
		}
	}

	// Check we have a root disk if in expanded validation mode.
	if expanded {
		_, _, err := shared.GetRootDiskDevice(devices.CloneNative())
		if err != nil {
			return fmt.Errorf("Failed detecting root disk device: %w", err)
		}
	}

	return nil
}

func create(s *state.State, args db.InstanceArgs, revert *revert.Reverter) (instance.Instance, error) {
	if args.Type == instancetype.Container {
		return lxcCreate(s, args, revert)
	} else if args.Type == instancetype.VM {
		return qemuCreate(s, args, revert)
	}

	return nil, fmt.Errorf("Instance type invalid")
}

// DriverStatuses returns a map of DriverStatus structs for all instance type drivers.
// The first time this function is called each of the instance drivers will be probed for support and the result
// will be cached internally to make subsequent calls faster.
func DriverStatuses() map[instancetype.Type]*DriverStatus {
	driverStatusesMu.Lock()
	defer driverStatusesMu.Unlock()

	if driverStatuses != nil {
		return driverStatuses
	}

	driverStatuses = make(map[instancetype.Type]*DriverStatus, len(instanceDrivers))

	for _, instanceDriver := range instanceDrivers {
		driverStatus := &DriverStatus{}

		driverInfo := instanceDriver().Info()
		driverStatus.Info = driverInfo
		driverStatus.Supported = true

		if driverInfo.Error != nil || driverInfo.Version == "" {
			logger.Warn("Instance type not operational", logger.Ctx{"type": driverInfo.Type, "driver": driverInfo.Name, "err": driverInfo.Error})

			driverStatus.Supported = false
			driverStatus.Warning = &db.Warning{
				TypeCode:    db.WarningInstanceTypeNotOperational,
				LastMessage: fmt.Sprintf("%v", driverInfo.Error),
			}
		}

		driverStatuses[driverInfo.Type] = driverStatus
	}

	return driverStatuses
}
