package device

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/lxc/incus/v7/internal/linux"
	"github.com/lxc/incus/v7/internal/server/cgroup"
	deviceConfig "github.com/lxc/incus/v7/internal/server/device/config"
	"github.com/lxc/incus/v7/internal/server/fsmonitor/drivers"
	"github.com/lxc/incus/v7/internal/server/instance"
	"github.com/lxc/incus/v7/internal/server/instance/instancetype"
	"github.com/lxc/incus/v7/shared/util"
	"github.com/lxc/incus/v7/shared/validate"
)

// unixIsOurDeviceType checks that device file type matches what we are expecting in the config.
func unixIsOurDeviceType(config deviceConfig.Device, dType string) bool {
	if config["type"] == "unix-char" && dType == "c" {
		return true
	}

	if config["type"] == "unix-block" && dType == "b" {
		return true
	}

	return false
}

type unixCommon struct {
	deviceCommon
}

// isRequired indicates whether the device config requires this device to start OK.
func (d *unixCommon) isRequired() bool {
	// Defaults to required.
	return util.IsTrueOrEmpty(d.config["required"])
}

// validateConfig checks the supplied config for correctness.
func (d *unixCommon) validateConfig(instConf instance.ConfigReader, partialValidation bool) error {
	if !instanceSupported(instConf.Type(), instancetype.Container) {
		return ErrUnsupportedDevType
	}

	rules := map[string]func(string) error{
		// gendoc:generate(entity=devices, group=unix-char-block, key=source)
		//
		// ---
		//  type: string
		//  shortdesc: Path on the host (one of `source` and `path` must be set)
		"source": func(value string) error {
			if value == "" {
				return nil
			}

			if strings.HasPrefix(value, d.state.DevMonitor.PrefixPath()) {
				return nil
			}

			return &drivers.ErrInvalidPath{PrefixPath: d.state.DevMonitor.PrefixPath()}
		},

		// gendoc:generate(entity=devices, group=unix-char-block, key=gid)
		//
		// ---
		//  type: int
		//  default: 0
		//  shortdesc: GID of the device owner in the instance
		"gid": unixValidUserID,

		// gendoc:generate(entity=devices, group=unix-char-block, key=major)
		//
		// ---
		//  type: int
		//  default: device on host
		//  shortdesc: Device major number
		"major": unixValidDeviceNum,

		// gendoc:generate(entity=devices, group=unix-char-block, key=minor)
		//
		// ---
		//  type: int
		//  default: device on host
		//  shortdesc: Device minor number
		"minor": unixValidDeviceNum,

		// gendoc:generate(entity=devices, group=unix-char-block, key=mode)
		//
		// ---
		//  type: int
		//  default: 0660
		//  shortdesc: Mode of the device in the instance
		"mode": unixValidOctalFileMode,

		// gendoc:generate(entity=devices, group=unix-char-block, key=path)
		//
		// ---
		//  type: string
		//  shortdesc: Path inside the instance (one of `source` and `path` must be set)
		"path": validate.IsAny,

		// gendoc:generate(entity=devices, group=unix-char-block, key=required)
		//
		// ---
		//  type: bool
		//  default: true
		//  shortdesc: Whether this device is required to start the instance
		"required": validate.Optional(validate.IsBool),

		// gendoc:generate(entity=devices, group=unix-char-block, key=limits.read)
		//
		// ---
		//  type: string
		//  shortdesc: I/O limit in byte/s or IOPS for block-device reads
		"limits.read": validate.IsAny,

		// gendoc:generate(entity=devices, group=unix-char-block, key=limits.write)
		//
		// ---
		//  type: string
		//  shortdesc: I/O limit in byte/s or IOPS for block-device writes
		"limits.write": validate.IsAny,

		// gendoc:generate(entity=devices, group=unix-char-block, key=uid)
		//
		// ---
		//  type: int
		//  default: 0
		//  shortdesc: UID of the device owner in the instance
		"uid": unixValidUserID,
	}

	err := d.config.Validate(rules)
	if err != nil {
		return err
	}

	if d.config["source"] == "" && d.config["path"] == "" {
		return errors.New("Unix device entry is missing the required \"source\" or \"path\" property")
	}

	if d.config["type"] != "unix-block" && (d.config["limits.read"] != "" || d.config["limits.write"] != "") {
		return errors.New("I/O limits are only supported for unix-block devices")
	}

	if !d.isRequired() && (d.config["limits.read"] != "" || d.config["limits.write"] != "") {
		return errors.New("I/O limits require a required unix-block device")
	}

	readBps, readIops, writeBps, writeIops, err := parseDiskLimit(d.config)
	if err != nil {
		return fmt.Errorf("Invalid I/O limit: %w", err)
	}

	if (d.config["limits.read"] != "" && readBps <= 0 && readIops <= 0) || (d.config["limits.write"] != "" && writeBps <= 0 && writeIops <= 0) {
		return errors.New("I/O limits must be greater than zero")
	}

	return nil
}

// Register is run after the device is started or on daemon startup.
func (d *unixCommon) Register() error {
	// Don't register for hot plug events if the device is required.
	if d.isRequired() {
		return nil
	}

	// Extract variables needed to run the event hook so that the reference to this device
	// struct is not needed to be kept in memory.
	devicesPath := d.inst.DevicesPath()
	devConfig := d.config
	deviceName := d.name
	state := d.state

	// Handler for when a Unix event occurs.
	f := func(e UnixEvent) (*deviceConfig.RunConfig, error) {
		// Check if the event is for a device file that this device wants.
		if unixDeviceSourcePath(devConfig) != e.Path {
			return nil, nil
		}

		// Derive the host side path for the instance device file.
		ourPrefix := deviceJoinPath("unix", deviceName)
		relativeDestPath := strings.TrimPrefix(unixDeviceDestPath(devConfig), "/")
		devName := linux.PathNameEncode(deviceJoinPath(ourPrefix, relativeDestPath))
		devPath := filepath.Join(devicesPath, devName)

		runConf := deviceConfig.RunConfig{}

		switch e.Action {
		case "add":
			// Skip if host side instance device file already exists.
			if util.PathExists(devPath) {
				return nil, nil
			}

			// Get the file type and ensure it matches what the user was expecting.
			dType, _, _, err := unixDeviceAttributes(e.Path)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					// Skip if host side source device doesn't exist.
					// This could be an event for the parent directory being added.
					return nil, nil
				}

				return nil, fmt.Errorf("Failed getting device attributes: %w", err)
			}

			if !unixIsOurDeviceType(d.config, dType) {
				return nil, fmt.Errorf("Path specified is not a %s device", d.config["type"])
			}

			err = unixDeviceSetup(state, devicesPath, "unix", deviceName, devConfig, true, &runConf)
			if err != nil {
				return nil, err
			}

		case "remove":
			// Skip if host side instance device file doesn't exist.
			if !util.PathExists(devPath) {
				return nil, nil
			}

			err := unixDeviceRemove(devicesPath, "unix", deviceName, relativeDestPath, &runConf)
			if err != nil {
				return nil, err
			}

			// Add a post hook function to remove the specific USB device file after unmount.
			runConf.PostHooks = []func() error{func() error {
				err := unixDeviceDeleteFiles(state, devicesPath, "unix", deviceName, relativeDestPath)
				if err != nil {
					return fmt.Errorf("Failed to delete files for device '%s': %w", deviceName, err)
				}

				return nil
			}}
		}

		return &runConf, nil
	}

	// Register the handler function against the device's source path.
	subPath := unixDeviceSourcePath(devConfig)
	err := unixRegisterHandler(d.state, d.inst, d.name, subPath, f)
	if err != nil {
		return err
	}

	return nil
}

// Start is run when the device is added to the container.
func (d *unixCommon) Start() (*deviceConfig.RunConfig, error) {
	runConf := deviceConfig.RunConfig{}
	runConf.PostHooks = []func() error{d.Register}
	srcPath := unixDeviceSourcePath(d.config)

	// If device file already exists on system, proceed to add it whether its required or not.
	dType, major, minor, err := unixDeviceAttributes(srcPath)
	if err == nil {
		// Ensure device type matches what the device config is expecting.
		if !unixIsOurDeviceType(d.config, dType) {
			return nil, fmt.Errorf("Path specified is not a %s device", d.config["type"])
		}

		err = unixDeviceSetup(d.state, d.inst.DevicesPath(), "unix", d.name, d.config, true, &runConf)
		if err != nil {
			return nil, err
		}

		err = d.applyBlockLimits(&runConf, major, minor, false)
		if err != nil {
			return nil, err
		}
	} else {
		// If the device file doesn't exist on the system, but major & minor numbers have
		// been provided in the config then we can go ahead and create the device anyway.
		if d.config["major"] != "" && d.config["minor"] != "" {
			err := unixDeviceSetup(d.state, d.inst.DevicesPath(), "unix", d.name, d.config, true, &runConf)
			if err != nil {
				return nil, err
			}

			majorValue, err := strconv.ParseUint(d.config["major"], 10, 32)
			if err != nil {
				return nil, err
			}

			minorValue, err := strconv.ParseUint(d.config["minor"], 10, 32)
			if err != nil {
				return nil, err
			}

			err = d.applyBlockLimits(&runConf, uint32(majorValue), uint32(minorValue), false)
			if err != nil {
				return nil, err
			}
		} else if d.isRequired() {
			// If the file is missing and the device is required then we cannot proceed.
			return nil, errors.New("The required device path doesn't exist and the major and minor settings are not specified")
		}
	}

	return &runConf, nil
}

// Stop is run when the device is removed from the instance.
func (d *unixCommon) Stop() (*deviceConfig.RunConfig, error) {
	// Unregister any Unix event handlers for this device.
	err := unixUnregisterHandler(d.state, d.inst, d.name)
	if err != nil {
		return nil, err
	}

	runConf := deviceConfig.RunConfig{
		PostHooks: []func() error{d.postStop},
	}

	if d.config["type"] == "unix-block" && (d.config["limits.read"] != "" || d.config["limits.write"] != "") {
		volatile := d.volatileGet()
		majorValue, majorErr := strconv.ParseUint(volatile["io.major"], 10, 32)
		minorValue, minorErr := strconv.ParseUint(volatile["io.minor"], 10, 32)
		if majorErr != nil || minorErr != nil {
			return nil, errors.New("Failed loading block device numbers to clear I/O limits")
		}

		err = d.applyBlockLimits(&runConf, uint32(majorValue), uint32(minorValue), true)
		if err != nil {
			return nil, err
		}
	}

	err = unixDeviceRemove(d.inst.DevicesPath(), "unix", d.name, "", &runConf)
	if err != nil {
		return nil, err
	}

	return &runConf, nil
}

func (d *unixCommon) applyBlockLimits(runConf *deviceConfig.RunConfig, major uint32, minor uint32, clear bool) error {
	if d.config["type"] != "unix-block" || (d.config["limits.read"] == "" && d.config["limits.write"] == "") {
		return nil
	}

	if !cgroup.Supports(cgroup.IO) {
		return errors.New("Cannot apply block device limits as IO cgroup controller is missing")
	}

	block := fmt.Sprintf("%d:%d", major, minor)
	if clear {
		runConf.CGroups = append(runConf.CGroups, deviceConfig.RunConfigItem{
			Key:   "io.max",
			Value: fmt.Sprintf("%s rbps=max riops=max wbps=max wiops=max", block),
		})

		return nil
	}

	err := d.volatileSet(map[string]string{
		"io.major": strconv.FormatUint(uint64(major), 10),
		"io.minor": strconv.FormatUint(uint64(minor), 10),
	})
	if err != nil {
		return err
	}

	readBps, readIops, writeBps, writeIops, err := parseDiskLimit(d.config)
	if err != nil {
		return err
	}

	cg, err := cgroup.New(&cgroupWriter{runConf})
	if err != nil {
		return err
	}

	limits := []struct {
		direction string
		unit      string
		value     int64
	}{
		{"read", "bps", readBps},
		{"read", "iops", readIops},
		{"write", "bps", writeBps},
		{"write", "iops", writeIops},
	}

	for _, limit := range limits {
		if limit.value <= 0 {
			continue
		}

		err = cg.SetBlkioLimit(block, limit.direction, limit.unit, limit.value)
		if err != nil {
			return err
		}
	}

	return nil
}

// postStop is run after the device is removed from the instance.
func (d *unixCommon) postStop() error {
	// Remove host files for this device.
	err := unixDeviceDeleteFiles(d.state, d.inst.DevicesPath(), "unix", d.name, "")
	if err != nil {
		return fmt.Errorf("Failed to delete files for device '%s': %w", d.name, err)
	}

	if d.config["limits.read"] != "" || d.config["limits.write"] != "" {
		err = d.volatileSet(map[string]string{"io.major": "", "io.minor": ""})
		if err != nil {
			return fmt.Errorf("Failed clearing I/O limit state for device '%s': %w", d.name, err)
		}
	}

	return nil
}
