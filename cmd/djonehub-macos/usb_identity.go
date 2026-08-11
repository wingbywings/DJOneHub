package main

import (
	"fmt"
	"strings"
)

// usbDeviceLocator identifies one physical USB attachment. Vendor/product IDs
// alone are insufficient because a hub can contain several identical modules.
// PortPath is stable across a normal device re-enumeration as long as the
// module remains connected to the same hub port.
type usbDeviceLocator struct {
	VendorID  uint16
	ProductID uint16
	Bus       uint8
	Address   uint8
	PortPath  []uint8
	Status    *usbDeviceStatus
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
	if a.VendorID != b.VendorID || a.ProductID != b.ProductID || a.Bus != b.Bus {
		return false
	}
	if len(a.PortPath) == 0 || len(b.PortPath) == 0 {
		return a.Address == b.Address
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
