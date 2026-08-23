package main

import "testing"

func TestUSBDeviceLocatorPhysicalIDUsesPortChain(t *testing.T) {
	locator := usbDeviceLocator{VendorID: 0x2ca3, ProductID: 0x4006, Bus: 2, Address: 9, PortPath: []uint8{3, 2}}
	if got, want := locator.PhysicalID(), "usb-2-3.2"; got != want {
		t.Fatalf("PhysicalID() = %q, want %q", got, want)
	}
}

func TestMacOSUSBLocationIDMatchesObservedHubTopology(t *testing.T) {
	for _, test := range []struct {
		ports []uint8
		want  uint32
	}{
		{ports: []uint8{1, 1, 1}, want: 0x02111000},
		{ports: []uint8{1, 1, 2}, want: 0x02112000},
		{ports: []uint8{1, 1, 3}, want: 0x02113000},
		{ports: []uint8{1, 1, 4}, want: 0x02114000},
	} {
		got, ok := macOSUSBLocationID(2, test.ports)
		if !ok || got != test.want {
			t.Fatalf("macOSUSBLocationID(2, %v) = 0x%08x, %v; want 0x%08x, true", test.ports, got, ok, test.want)
		}
	}
}

func TestMacOSUSBLocationIDRejectsUnrepresentablePaths(t *testing.T) {
	for _, ports := range [][]uint8{nil, {0}, {16}, {1, 2, 3, 4, 5, 6, 7}} {
		if got, ok := macOSUSBLocationID(2, ports); ok || got != 0 {
			t.Fatalf("macOSUSBLocationID accepted invalid path %v as 0x%08x", ports, got)
		}
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

func TestSameUSBDeviceSurvivesSupportedIdentityChangeOnSamePort(t *testing.T) {
	dji := usbDeviceLocator{VendorID: djiUSBVendorID, ProductID: djiUSBProductID, Bus: 1, Address: 4, PortPath: []uint8{5, 1}}
	quectel := usbDeviceLocator{VendorID: quectelUSBVendorID, ProductID: quectelUSBProductID, Bus: 1, Address: 9, PortPath: []uint8{5, 1}}
	if !sameUSBDevice(dji, quectel) {
		t.Fatal("USB identity changes on the same physical port should keep the same module")
	}
	quectel.PortPath = []uint8{5, 2}
	if sameUSBDevice(dji, quectel) {
		t.Fatal("identity changes must not merge modules on different physical ports")
	}
}

func TestSameUSBEnumerationDetectsIdentityAndAddressChanges(t *testing.T) {
	before := usbDeviceLocator{VendorID: djiUSBVendorID, ProductID: djiUSBProductID, Bus: 2, Address: 4, PortPath: []uint8{1, 1, 1}}
	same := before
	if !sameUSBEnumeration(before, same) {
		t.Fatal("unchanged enumeration should match")
	}
	after := before
	after.Address = 9
	if sameUSBEnumeration(before, after) {
		t.Fatal("a new USB address should trigger handle reopening")
	}
	after = before
	after.VendorID = quectelUSBVendorID
	after.ProductID = quectelUSBProductID
	if sameUSBEnumeration(before, after) {
		t.Fatal("a USB mode identity change should trigger handle reopening")
	}
	if !sameUSBDevice(before, after) {
		t.Fatal("the physical attachment should still match across mode changes")
	}
}

func TestSupportedUSBModuleIdentities(t *testing.T) {
	if !isSupportedUSBModuleIdentity(djiUSBVendorID, djiUSBProductID) {
		t.Fatal("DJI factory identity should be supported")
	}
	if !isSupportedUSBModuleIdentity(quectelUSBVendorID, quectelUSBProductID) {
		t.Fatal("Quectel UAC identity should be supported")
	}
	if isSupportedUSBModuleIdentity(0xffff, 0xffff) {
		t.Fatal("unknown USB identity should not be supported")
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
