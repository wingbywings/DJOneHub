//go:build darwin && cgo

package main

/*
#cgo pkg-config: libusb-1.0
#include <libusb.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unsafe"
)

// adbClient speaks the ADB wire protocol directly over the selected module's
// USB ADB interface. The locator is mandatory so identical modules cannot be
// mixed up when AT, ADB and (later) CoreAudio are opened independently.
type adbClient struct {
	ctx              *C.libusb_context
	handle           *C.libusb_device_handle
	iface            int
	endpointIn       byte
	endpointOut      byte
	mu               sync.Mutex
	readBuffer       []byte
	remoteMaxPayload int
	nextLocalID      uint32
	connected        bool
}

type adbStream struct {
	localID  uint32
	remoteID uint32
}

type adbUSBInterface struct {
	iface       int
	endpointIn  byte
	endpointOut byte
}

func openDJIUSBADB(target usbDeviceLocator) (*adbClient, error) {
	var ctx *C.libusb_context
	if rc := C.libusb_init(&ctx); rc != 0 {
		return nil, fmt.Errorf("libusb init: %s", usbErrorName(rc))
	}
	var list **C.libusb_device
	count := C.libusb_get_device_list(ctx, &list)
	if count < 0 {
		C.libusb_exit(ctx)
		return nil, fmt.Errorf("list USB devices: %s", usbErrorName(C.int(count)))
	}
	defer C.libusb_free_device_list(list, 1)

	var handle *C.libusb_device_handle
	for _, device := range unsafe.Slice(list, int(count)) {
		var descriptor C.struct_libusb_device_descriptor
		if rc := C.libusb_get_device_descriptor(device, &descriptor); rc != 0 {
			continue
		}
		if !isSupportedUSBModuleIdentity(uint16(descriptor.idVendor), uint16(descriptor.idProduct)) {
			continue
		}
		candidate := locatorForUSBDevice(device, descriptor)
		if !sameUSBDevice(target, candidate) {
			continue
		}
		if rc := C.libusb_open(device, &handle); rc != 0 {
			C.libusb_exit(ctx)
			return nil, fmt.Errorf("open USB ADB device %s: %s", target.PhysicalID(), usbErrorName(rc))
		}
		break
	}
	if handle == nil {
		C.libusb_exit(ctx)
		return nil, fmt.Errorf("USB ADB device %s not found", target.PhysicalID())
	}

	selected, err := findADBUSBInterface(handle)
	if err != nil {
		C.libusb_close(handle)
		C.libusb_exit(ctx)
		return nil, fmt.Errorf("USB ADB device %s: %w", target.PhysicalID(), err)
	}
	if rc := C.libusb_claim_interface(handle, C.int(selected.iface)); rc != 0 {
		C.libusb_close(handle)
		C.libusb_exit(ctx)
		return nil, fmt.Errorf("claim USB ADB interface %d on %s: %s", selected.iface, target.PhysicalID(), usbErrorName(rc))
	}
	return &adbClient{
		ctx:              ctx,
		handle:           handle,
		iface:            selected.iface,
		endpointIn:       selected.endpointIn,
		endpointOut:      selected.endpointOut,
		remoteMaxPayload: adbMaxPayload,
		nextLocalID:      1,
	}, nil
}

func findADBUSBInterface(handle *C.libusb_device_handle) (adbUSBInterface, error) {
	device := C.libusb_get_device(handle)
	if device == nil {
		return adbUSBInterface{}, errors.New("libusb device handle has no device")
	}
	var config *C.struct_libusb_config_descriptor
	if rc := C.libusb_get_active_config_descriptor(device, &config); rc != 0 {
		return adbUSBInterface{}, fmt.Errorf("get active USB config descriptor: %s", usbErrorName(rc))
	}
	defer C.libusb_free_config_descriptor(config)

	interfaces := unsafe.Slice(config._interface, int(config.bNumInterfaces))
	for _, intf := range interfaces {
		for _, alt := range unsafe.Slice(intf.altsetting, int(intf.num_altsetting)) {
			// Android ADB identifies itself as ff/42/01. Interface number 6 is
			// common on QDC507 but is not an identity: legacy UAC also uses that
			// number for an audio interface.
			if byte(alt.bInterfaceClass) != 0xff || byte(alt.bInterfaceSubClass) != 0x42 || byte(alt.bInterfaceProtocol) != 0x01 {
				continue
			}
			var endpointIn, endpointOut byte
			for _, endpoint := range unsafe.Slice(alt.endpoint, int(alt.bNumEndpoints)) {
				attributes := byte(endpoint.bmAttributes) & byte(C.LIBUSB_TRANSFER_TYPE_MASK)
				if attributes != byte(C.LIBUSB_TRANSFER_TYPE_BULK) {
					continue
				}
				address := byte(endpoint.bEndpointAddress)
				if address&byte(C.LIBUSB_ENDPOINT_IN) != 0 {
					endpointIn = address
				} else {
					endpointOut = address
				}
			}
			if endpointIn != 0 && endpointOut != 0 {
				return adbUSBInterface{iface: int(alt.bInterfaceNumber), endpointIn: endpointIn, endpointOut: endpointOut}, nil
			}
		}
	}
	return adbUSBInterface{}, errors.New("ADB bulk interface (ff/42/01 with bulk IN/OUT) not found")
}

func (a *adbClient) Close() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.handle == nil {
		return
	}
	C.libusb_release_interface(a.handle, C.int(a.iface))
	C.libusb_close(a.handle)
	C.libusb_exit(a.ctx)
	a.handle = nil
	a.ctx = nil
	a.connected = false
	a.readBuffer = nil
}

func (a *adbClient) isOpen() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.handle != nil
}

func (a *adbClient) bulkWrite(payload []byte, timeout time.Duration) error {
	if len(payload) == 0 {
		return nil
	}
	var transferred C.int
	rc := C.libusb_bulk_transfer(a.handle, C.uchar(a.endpointOut), (*C.uchar)(unsafe.Pointer(&payload[0])), C.int(len(payload)), &transferred, C.uint(timeout.Milliseconds()))
	if rc != 0 {
		return fmt.Errorf("USB ADB bulk write: %s", usbErrorName(rc))
	}
	if int(transferred) != len(payload) {
		return fmt.Errorf("USB ADB bulk write short transfer: %d/%d", int(transferred), len(payload))
	}
	return nil
}

func (a *adbClient) bulkRead(buffer []byte, timeout time.Duration) (int, error) {
	var transferred C.int
	rc := C.libusb_bulk_transfer(a.handle, C.uchar(a.endpointIn), (*C.uchar)(unsafe.Pointer(&buffer[0])), C.int(len(buffer)), &transferred, C.uint(timeout.Milliseconds()))
	if rc != 0 {
		if rc == C.LIBUSB_ERROR_TIMEOUT {
			return 0, errADBTimeout
		}
		return 0, fmt.Errorf("USB ADB bulk read: %s", usbErrorName(rc))
	}
	return int(transferred), nil
}

func (a *adbClient) sendLocked(command, arg0, arg1 uint32, payload []byte, timeout time.Duration) error {
	if err := a.bulkWrite(adbEncodeHeader(command, arg0, arg1, payload), timeout); err != nil {
		return err
	}
	return a.bulkWrite(payload, timeout)
}

func (a *adbClient) readExactlyLocked(size int, deadline time.Time) ([]byte, error) {
	output := make([]byte, 0, size)
	if len(a.readBuffer) > 0 {
		take := size
		if take > len(a.readBuffer) {
			take = len(a.readBuffer)
		}
		output = append(output, a.readBuffer[:take]...)
		a.readBuffer = a.readBuffer[take:]
	}
	buffer := make([]byte, adbMaxPayload+24)
	for len(output) < size && time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		if remaining > 100*time.Millisecond {
			remaining = 100 * time.Millisecond
		}
		got, err := a.bulkRead(buffer, remaining)
		if errors.Is(err, errADBTimeout) {
			continue
		}
		if err != nil {
			return nil, err
		}
		need := size - len(output)
		if got <= need {
			output = append(output, buffer[:got]...)
			continue
		}
		output = append(output, buffer[:need]...)
		a.readBuffer = append(a.readBuffer, buffer[need:got]...)
	}
	if len(output) != size {
		return nil, fmt.Errorf("等待模块 ADB 数据超时（需要 %d 字节，得到 %d）", size, len(output))
	}
	return output, nil
}

func (a *adbClient) receiveLocked(deadline time.Time) (adbMessage, error) {
	header, err := a.readExactlyLocked(24, deadline)
	if err != nil {
		return adbMessage{}, err
	}
	message, expectedChecksum, err := adbDecodeHeader(header)
	if err != nil {
		return adbMessage{}, err
	}
	length := int(leUint32(header[12:]))
	message.payload, err = a.readExactlyLocked(length, deadline)
	if err != nil {
		return adbMessage{}, err
	}
	if adbChecksum(message.payload) != expectedChecksum {
		return adbMessage{}, errors.New("ADB 消息校验和不匹配")
	}
	return message, nil
}

func (a *adbClient) connectLocked() error {
	if a.connected {
		return nil
	}
	cmdCNXN := adbCommand("CNXN")
	cmdAUTH := adbCommand("AUTH")
	cmdWRTE := adbCommand("WRTE")
	cmdOKAY := adbCommand("OKAY")
	cmdCLSE := adbCommand("CLSE")
	banner := append([]byte("host::DJOneHub"), 0)
	sendConnect := func() error {
		return a.sendLocked(cmdCNXN, adbVersion, adbMaxPayload, banner, 2*time.Second)
	}
	if err := sendConnect(); err != nil {
		return err
	}
	deadline := time.Now().Add(8 * time.Second)
	stale := 0
	for time.Now().Before(deadline) {
		message, err := a.receiveLocked(deadline)
		if err != nil {
			return err
		}
		switch message.command {
		case cmdAUTH:
			return errADBAuthRequired
		case cmdCNXN:
			if message.arg1 > 0 {
				a.remoteMaxPayload = min(a.remoteMaxPayload, int(message.arg1))
				a.connected = true
				return nil
			}
		case cmdWRTE, cmdOKAY, cmdCLSE:
			stale++
			if stale > 64 {
				return errors.New("ADB 旧流无法清理")
			}
			if message.arg0 != 0 && message.arg1 != 0 {
				if err := a.sendLocked(cmdCLSE, message.arg1, message.arg0, nil, 2*time.Second); err != nil {
					return err
				}
			}
			if err := sendConnect(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("模块未接受 ADB CNXN（command=0x%08x）", message.command)
		}
	}
	return errors.New("等待模块接受 ADB CNXN 超时")
}

func (a *adbClient) openServiceLocked(service string) (adbStream, error) {
	payload := append([]byte(service), 0)
	if len(payload) > a.remoteMaxPayload {
		return adbStream{}, errors.New("ADB 服务命令过长")
	}
	localID := a.nextLocalID
	a.nextLocalID++
	if a.nextLocalID == 0 {
		a.nextLocalID = 1
	}
	if err := a.sendLocked(adbCommand("OPEN"), localID, 0, payload, 2*time.Second); err != nil {
		return adbStream{}, err
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		message, err := a.receiveLocked(deadline)
		if err != nil {
			return adbStream{}, err
		}
		switch message.command {
		case adbCommand("OKAY"):
			if message.arg0 != 0 && message.arg1 == localID && len(message.payload) == 0 {
				return adbStream{localID: localID, remoteID: message.arg0}, nil
			}
		case adbCommand("CNXN"):
			if message.arg1 > 0 {
				a.remoteMaxPayload = min(a.remoteMaxPayload, int(message.arg1))
			}
		case adbCommand("CLSE"):
			if message.arg1 == localID {
				return adbStream{}, errors.New("模块拒绝 ADB 服务")
			}
			if err := a.closeForeignStreamLocked(message); err != nil {
				return adbStream{}, err
			}
		case adbCommand("WRTE"):
			if err := a.closeForeignStreamLocked(message); err != nil {
				return adbStream{}, err
			}
		default:
			return adbStream{}, errors.New("模块拒绝 ADB 服务")
		}
	}
	return adbStream{}, errors.New("等待模块打开 ADB 服务超时")
}

func (a *adbClient) closeForeignStreamLocked(message adbMessage) error {
	if message.arg0 == 0 || message.arg1 == 0 {
		return nil
	}
	return a.sendLocked(adbCommand("CLSE"), message.arg1, message.arg0, nil, 2*time.Second)
}

func (a *adbClient) writeStreamLocked(stream adbStream, data []byte, timeout time.Duration) error {
	if len(data) > a.remoteMaxPayload {
		return errors.New("ADB sync 数据块过大")
	}
	if err := a.sendLocked(adbCommand("WRTE"), stream.localID, stream.remoteID, data, timeout); err != nil {
		return err
	}
	message, err := a.receiveLocked(time.Now().Add(10 * time.Second))
	if err != nil {
		return err
	}
	if message.command != adbCommand("OKAY") || message.arg0 != stream.remoteID || message.arg1 != stream.localID || len(message.payload) != 0 {
		return errors.New("模块未确认 ADB 数据块")
	}
	return nil
}

func (a *adbClient) closeStreamLocked(stream adbStream) error {
	if err := a.sendLocked(adbCommand("CLSE"), stream.localID, stream.remoteID, nil, 2*time.Second); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		message, err := a.receiveLocked(deadline)
		if err != nil {
			return err
		}
		if message.command == adbCommand("CLSE") && message.arg0 == stream.remoteID && message.arg1 == stream.localID && len(message.payload) == 0 {
			return nil
		}
		if message.command == adbCommand("WRTE") && message.arg0 == stream.remoteID && message.arg1 == stream.localID {
			if err := a.sendLocked(adbCommand("OKAY"), stream.localID, stream.remoteID, nil, 2*time.Second); err != nil {
				return err
			}
			continue
		}
		return errors.New("ADB 流关闭响应无效")
	}
	return errors.New("等待模块关闭 ADB 流超时")
}

// shellChecked runs a module shell command and returns both output and exit status.
func (a *adbClient) shellChecked(command string, timeout time.Duration) (string, int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.handle == nil {
		return "", 0, errors.New("ADB 通道未打开")
	}
	if err := a.connectLocked(); err != nil {
		return "", 0, err
	}
	token := adbToken()
	wrapped := "{ " + command + "; }; __djonehub_status=$?; printf '\\n__DJONEHUB_STATUS_" + token + "_%u__\\n' \"$__djonehub_status\""
	stream, err := a.openServiceLocked("shell:" + wrapped)
	if err != nil {
		return "", 0, err
	}
	var output strings.Builder
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		message, err := a.receiveLocked(deadline)
		if err != nil {
			a.connected = false
			return output.String(), 0, err
		}
		switch message.command {
		case adbCommand("WRTE"):
			if message.arg0 == stream.remoteID && message.arg1 == stream.localID {
				output.Write(message.payload)
				if err := a.sendLocked(adbCommand("OKAY"), stream.localID, stream.remoteID, nil, 2*time.Second); err != nil {
					return output.String(), 0, err
				}
			}
		case adbCommand("CLSE"):
			if message.arg1 != stream.localID {
				continue
			}
			if message.arg0 != 0 {
				_ = a.sendLocked(adbCommand("CLSE"), stream.localID, stream.remoteID, nil, 2*time.Second)
			}
			status, ok := parseADBStatus(output.String(), token)
			if !ok {
				a.connected = false
				return output.String(), 0, errors.New("模块 shell 没有返回退出状态")
			}
			return output.String(), status, nil
		}
	}
	a.connected = false
	return output.String(), 0, errors.New("等待模块 shell 超时")
}

// push copies an in-memory file to the module through the ADB sync service.
func (a *adbClient) push(data []byte, remotePath string, mode uint32, timeout time.Duration) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.handle == nil {
		return errors.New("ADB 通道未打开")
	}
	if strings.ContainsAny(remotePath, ",\x00") {
		return errors.New("ADB push 目标路径无效")
	}
	if err := a.connectLocked(); err != nil {
		return err
	}
	stream, err := a.openServiceLocked("sync:")
	if err != nil {
		return err
	}
	fail := func(err error) error {
		_ = a.closeStreamLocked(stream)
		return err
	}
	if err := a.writeSyncLocked(stream, "SEND", []byte(fmt.Sprintf("%s,%d", remotePath, mode)), timeout); err != nil {
		return fail(err)
	}
	chunkSize := min(adbMaxPayload-8, a.remoteMaxPayload-8)
	for offset := 0; offset < len(data); offset += chunkSize {
		end := min(offset+chunkSize, len(data))
		if err := a.writeSyncLocked(stream, "DATA", data[offset:end], timeout); err != nil {
			return fail(err)
		}
	}
	done := make([]byte, 4)
	lePutUint32(done, uint32(time.Now().Unix()))
	if err := a.writeSyncLocked(stream, "DONE", done, timeout); err != nil {
		return fail(err)
	}

	var response []byte
	deadline := time.Now().Add(20 * time.Second)
	for len(response) < 8 && time.Now().Before(deadline) {
		message, err := a.receiveLocked(deadline)
		if err != nil {
			return fail(err)
		}
		if message.command == adbCommand("CLSE") {
			return errors.New("ADB sync 提前关闭")
		}
		if message.command != adbCommand("WRTE") || message.arg0 != stream.remoteID || message.arg1 != stream.localID {
			continue
		}
		response = append(response, message.payload...)
		if err := a.sendLocked(adbCommand("OKAY"), stream.localID, stream.remoteID, nil, 2*time.Second); err != nil {
			return fail(err)
		}
	}
	if len(response) < 8 {
		return fail(errors.New("等待模块 ADB sync 响应超时"))
	}
	id := string(response[:4])
	value := leUint32(response[4:8])
	if id == "FAIL" {
		detail := ""
		if int(value) > 0 && len(response) >= 8+int(value) {
			detail = string(response[8 : 8+value])
		}
		return fail(fmt.Errorf("模块拒绝文件传输：%s", detail))
	}
	if id != "OKAY" || value != 0 {
		return fail(errors.New("ADB sync 返回无效状态"))
	}
	return a.closeStreamLocked(stream)
}

func (a *adbClient) writeSyncLocked(stream adbStream, id string, payload []byte, timeout time.Duration) error {
	packet := make([]byte, 8+len(payload))
	copy(packet[:4], id)
	lePutUint32(packet[4:8], uint32(len(payload)))
	copy(packet[8:], payload)
	return a.writeStreamLocked(stream, packet, timeout)
}
