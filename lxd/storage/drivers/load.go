package drivers

import (
	"github.com/lxc/lxd/lxd/state"
	"github.com/lxc/lxd/shared/logger"
)

var drivers = map[string]func() driver{
	"btrfs":  func() driver { return &btrfs{} },
	"cephfs": func() driver { return &cephfs{} },
	"dir":    func() driver { return &dir{} },
	"lvm":    func() driver { return &lvm{} },
	"zfs":    func() driver { return &zfs{} },
	"ceph":   func() driver { return &ceph{} },
}

// Validators contains functions used for validating a drivers's config.
type Validators struct {
	PoolRules   func() map[string]func(string) error
	VolumeRules func(vol Volume) map[string]func(string) error
}

// Load returns a Driver for an existing low-level storage pool.
func Load(state *state.State, driverName string, name string, config map[string]string, logger logger.Logger, volIDFunc func(volType VolumeType, volName string) (int64, error), commonRules *Validators) (Driver, error) {
	var driverFunc func() driver

	// Locate the driver loader.
	if state.OS.MockMode {
		driverFunc = func() driver { return &mock{} }
	} else {
		df, ok := drivers[driverName]
		if !ok {
			return nil, ErrUnknownDriver
		}
		driverFunc = df
	}

	d := driverFunc()
	d.init(state, name, config, logger, volIDFunc, commonRules)

	err := d.load()
	if err != nil {
		return nil, err
	}

	return d, nil
}

// SupportedDrivers returns a list of supported storage drivers.
func SupportedDrivers(s *state.State) []Info {
	supportedDrivers := []Info{}

	for driverName := range drivers {
		driver, err := Load(s, driverName, "", nil, nil, nil, nil)
		if err != nil {
			continue
		}

		supportedDrivers = append(supportedDrivers, driver.Info())
	}

	return supportedDrivers
}
