package main

import "testing"

func TestUSBDeviceLocatorPhysicalIDUsesPortChain(t *testing.T) {
	locator := usbDeviceLocator{VendorID: 0x2ca3, ProductID: 0x4006, Bus: 2, Address: 9, PortPath: []uint8{3, 2}}
	if got, want := locator.PhysicalID(), "usb-2-3.2"; got != want {
		t.Fatalf("PhysicalID() = %q, want %q", got, want)
	}
}

func TestSameUSBDeviceIgnoresTransientAddressWhenPortPathExists(t *testing.T) {
	a := usbDeviceLocator{VendorID: 0x2ca3, ProductID: 0x4006, Bus: 1, Address: 4, PortPath: []uint8{5, 1}}
	b := usbDeviceLocator{VendorID: 0x2ca3, ProductID: 0x4006, Bus: 1, Address: 8, PortPath: []uint8{5, 1}}
	if !sameUSBDevice(a, b) {
		t.Fatal("same physical port should survive USB address changes")
	}
	b.PortPath = []uint8{5, 2}
	if sameUSBDevice(a, b) {
		t.Fatal("different hub ports must not be treated as one module")
	}
}

func TestDeviceIDUsesStableIdentity(t *testing.T) {
	first := deviceIDFromIdentity("867400000000001")
	second := deviceIDFromIdentity("867400000000001")
	other := deviceIDFromIdentity("867400000000002")
	if first != second || first == other {
		t.Fatalf("device IDs are not stable and distinct: %q %q %q", first, second, other)
	}
}
