//go:build !darwin || !cgo

package main

import (
	"errors"
	"time"
)

type adbClient struct{}

func openDJIUSBADB(_ usbDeviceLocator) (*adbClient, error) {
	return nil, errors.New("USB ADB requires a macOS cgo build with libusb")
}

func (a *adbClient) Close() {}

func (a *adbClient) isOpen() bool { return false }

func (a *adbClient) shellChecked(_ string, _ time.Duration) (string, int, error) {
	return "", 0, errors.New("USB ADB is unavailable in this build")
}

func (a *adbClient) push(_ []byte, _ string, _ uint32, _ time.Duration) error {
	return errors.New("USB ADB is unavailable in this build")
}
