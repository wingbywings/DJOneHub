package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	adbMaxPayload = 4096
	adbVersion    = 0x01000001
)

var (
	errADBAuthRequired = errors.New("模块 ADB 要求认证，无法自动控制通话组件")
	errADBTimeout      = errors.New("adb timeout")
)

type adbMessage struct {
	command uint32
	arg0    uint32
	arg1    uint32
	payload []byte
}

func adbCommand(text string) uint32 {
	if len(text) != 4 {
		panic("ADB command must contain exactly four bytes")
	}
	b := []byte(text)
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func adbChecksum(payload []byte) uint32 {
	var sum uint32
	for _, b := range payload {
		sum += uint32(b)
	}
	return sum
}

func adbEncodeHeader(cmd, arg0, arg1 uint32, payload []byte) []byte {
	header := make([]byte, 24)
	lePutUint32(header[0:], cmd)
	lePutUint32(header[4:], arg0)
	lePutUint32(header[8:], arg1)
	lePutUint32(header[12:], uint32(len(payload)))
	lePutUint32(header[16:], adbChecksum(payload))
	lePutUint32(header[20:], cmd^0xffffffff)
	return header
}

func adbDecodeHeader(header []byte) (adbMessage, uint32, error) {
	if len(header) != 24 {
		return adbMessage{}, 0, fmt.Errorf("ADB header length is %d, want 24", len(header))
	}
	command := leUint32(header[0:])
	if leUint32(header[20:]) != command^0xffffffff {
		return adbMessage{}, 0, errors.New("ADB 消息 magic 无效")
	}
	length := leUint32(header[12:])
	if length > adbMaxPayload {
		return adbMessage{}, 0, fmt.Errorf("ADB 消息长度无效: %d", length)
	}
	return adbMessage{
		command: command,
		arg0:    leUint32(header[4:]),
		arg1:    leUint32(header[8:]),
	}, leUint32(header[16:]), nil
}

func lePutUint32(b []byte, value uint32) {
	b[0] = byte(value)
	b[1] = byte(value >> 8)
	b[2] = byte(value >> 16)
	b[3] = byte(value >> 24)
}

func leUint32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func adbToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", b)
}

func parseADBStatus(raw, token string) (int, bool) {
	prefix := "__DJONEHUB_STATUS_" + token + "_"
	idx := strings.LastIndex(raw, prefix)
	if idx == -1 {
		return 0, false
	}
	rest := raw[idx+len(prefix):]
	end := strings.Index(rest, "__")
	if end == -1 {
		return 0, false
	}
	var status int
	if _, err := fmt.Sscanf(rest[:end], "%d", &status); err != nil {
		return 0, false
	}
	return status, true
}
