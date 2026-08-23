package main

import (
	"fmt"
	"strings"
)

const (
	djiUSBVendorID      = 0x2ca3
	djiUSBProductID     = 0x4006
	quectelUSBVendorID  = 0x2c7c
	quectelUSBProductID = 0x0125
)

func isSupportedUSBModuleIdentity(vendorID, productID uint16) bool {
	return vendorID == djiUSBVendorID && productID == djiUSBProductID ||
		vendorID == quectelUSBVendorID && productID == quectelUSBProductID
}

// usbDeviceLocator identifies one physical USB attachment. Vendor/product IDs
// alone are insufficient because a hub can contain several identical modules.
// PortPath is stable across a normal device re-enumeration as long as the
// module remains connected to the same hub port.
type usbDeviceLocator struct {
	VendorID   uint16
	ProductID  uint16
	Bus        uint8
	Address    uint8
	PortPath   []uint8
	LocationID uint32
	Status     *usbDeviceStatus
}

// macOSUSBLocationID reproduces the IOUSBHost locationID encoding used by
// CoreAudio's USB registry parents: bus in the high byte followed by up to six
// four-bit port numbers. Keeping this value beside the libusb port chain lets
// AT, ADB and UAC bind to the same physical module without running a separate
// first-match ioreg scan.
func macOSUSBLocationID(bus uint8, portPath []uint8) (uint32, bool) {
	if bus == 0 || len(portPath) == 0 || len(portPath) > 6 {
		return 0, false
	}
	locationID := uint32(bus) << 24
	shift := 20
	for _, port := range portPath {
		if port == 0 || port > 0x0f {
			return 0, false
		}
		locationID |= uint32(port) << shift
		shift -= 4
	}
	return locationID, true
}

func (l usbDeviceLocator) PhysicalID() string {
	parts := make([]string, 0, len(l.PortPath))
	for _, port := range l.PortPath {
		parts = append(parts, fmt.Sprintf("%d", port))
	}
	path := strings.Join(parts, ".")
	if path == "" {
		path = fmt.Sprintf("address-%d", l.Address)
	}
	return fmt.Sprintf("usb-%d-%s", l.Bus, path)
}

func sameUSBDevice(a, b usbDeviceLocator) bool {
	if a.Bus != b.Bus {
		return false
	}
	if len(a.PortPath) == 0 || len(b.PortPath) == 0 {
		return a.VendorID == b.VendorID && a.ProductID == b.ProductID && a.Address == b.Address
	}
	if len(a.PortPath) != len(b.PortPath) {
		return false
	}
	for i := range a.PortPath {
		if a.PortPath[i] != b.PortPath[i] {
			return false
		}
	}
	return true
}

func discoverDJIUSBDevices() []usbDeviceLocator {
	devices, err := listDJIUSBDevices()
	if err != nil {
		return nil
	}
	return devices
}

func discoverDJIUSBDeviceByLocator(locator usbDeviceLocator) *usbDeviceStatus {
	for _, candidate := range discoverDJIUSBDevices() {
		if sameUSBDevice(locator, candidate) {
			return candidate.Status
		}
	}
	return nil
}
