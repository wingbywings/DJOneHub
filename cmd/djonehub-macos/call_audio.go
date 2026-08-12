package main

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	callAudioUSBConfigQuery       = `AT+QCFG="USBCFG"`
	callAudioUSBConfigLegacyQuery = `AT+QCFG="USBCFG"?`
	callAudioSupportQuery         = "AT+QPCMV=?"
	callAudioRuntimeQuery         = "AT+QPCMV?"
	callAudioEnableCommand        = "AT+QPCMV=1,2"
	callAudioUSBConfigUACIndex    = 9
)

var qpcmvStatusPattern = regexp.MustCompile(`(?i)\+QPCMV:\s*([01])(?:\s*,\s*(\d+))?`)

type callAudioCapability struct {
	Supported               bool     `json:"supported"`
	USBConfigKnown          bool     `json:"usb_config_known"`
	USBFunctionCount        int      `json:"usb_function_count"`
	UACConfigured           bool     `json:"uac_configured"`
	NeedsUSBReconfigure     bool     `json:"needs_usb_reconfigure"`
	RuntimeControlAvailable bool     `json:"runtime_control_available"`
	RuntimeStatusKnown      bool     `json:"runtime_status_known"`
	Enabled                 bool     `json:"enabled"`
	Mode                    int      `json:"mode"`
	Diagnostics             []string `json:"diagnostics"`
}

type callAudioEnableResult struct {
	OK              bool                 `json:"ok"`
	Status          string               `json:"status"`
	RestartRequired bool                 `json:"restart_required"`
	Capability      *callAudioCapability `json:"capability,omitempty"`
}

func parseUSBCFG(response string) ([]string, bool) {
	for _, line := range strings.Split(strings.ReplaceAll(response, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToUpper(line), "+QCFG:") ||
			!strings.Contains(strings.ToLower(line), `"usbcfg"`) {
			continue
		}
		separator := strings.Index(line, ":")
		if separator < 0 {
			return nil, false
		}
		parts := strings.Split(strings.TrimSpace(line[separator+1:]), ",")
		if len(parts) < 2 {
			return nil, false
		}
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		return parts, true
	}
	return nil, false
}

func usbConfigUACEnabled(parts []string) (bool, bool) {
	// USBCFG consists of the command name, VID, PID and seven USB function
	// parameters. Quectel documents the seventh function parameter as UAC.
	// Firmware returning only six function parameters cannot enable UAC via AT.
	if len(parts) <= callAudioUSBConfigUACIndex {
		return false, false
	}
	switch strings.TrimSpace(parts[callAudioUSBConfigUACIndex]) {
	case "0":
		return false, true
	case "1":
		return true, true
	default:
		return false, false
	}
}

func enableUACInUSBCFG(parts []string) (string, bool) {
	if _, known := usbConfigUACEnabled(parts); !known {
		return "", false
	}
	updated := append([]string(nil), parts...)
	updated[callAudioUSBConfigUACIndex] = "1"
	return "AT+QCFG=" + strings.Join(updated, ","), true
}

func usbFunctionCount(parts []string) int {
	if len(parts) <= 3 {
		return 0
	}
	return len(parts) - 3
}

func parseQPCMVStatus(response string) (enabled bool, mode int, ok bool) {
	match := qpcmvStatusPattern.FindStringSubmatch(response)
	if len(match) != 3 {
		return false, 0, false
	}
	mode = 0
	if match[2] != "" {
		parsedMode, err := strconv.Atoi(match[2])
		if err != nil {
			return false, 0, false
		}
		mode = parsedMode
	}
	return match[1] == "1", mode, true
}

func callAudioUnavailable(err error) error {
	return &callControlError{
		Code:       "call_audio_unavailable",
		HTTPStatus: http.StatusServiceUnavailable,
		Message:    "无法读取模块音频能力，请检查模块连接和 AT 通道",
		Cause:      err,
	}
}

func callAudioCommandError(stage, command, response string, err error) error {
	if err != nil {
		return &callControlError{
			Code:       "call_audio_unavailable",
			HTTPStatus: http.StatusServiceUnavailable,
			Message:    "模块音频配置暂时不可用，请检查模块连接和 AT 通道",
			Detail:     stage + "：" + command,
			Cause:      err,
		}
	}
	if !callATResponseRejected(response) {
		return nil
	}
	modemResponse := strings.TrimSpace(strings.ReplaceAll(response, "\r\n", " "))
	if len(modemResponse) > 160 {
		modemResponse = modemResponse[:160] + "…"
	}
	if stage == "usb_config" {
		return &callControlError{
			Code:       "uac_usb_config_rejected",
			HTTPStatus: http.StatusConflict,
			Message:    "模块拒绝写入 UAC USB 组合，当前固件可能未开放 UAC",
			Detail:     command + " → " + modemResponse,
		}
	}
	return &callControlError{
		Code:       "uac_runtime_enable_rejected",
		HTTPStatus: http.StatusConflict,
		Message:    "USB Audio 接口已配置，但模块拒绝开启 QPCMV UAC 模式",
		Detail:     command + " → " + modemResponse,
	}
}

func (a *app) probeCallAudioSupportLocked() (bool, error) {
	response, err := a.runCallAT(callAudioSupportQuery, 3*time.Second)
	if err != nil {
		return false, callAudioUnavailable(err)
	}
	return !callATResponseRejected(response) &&
		strings.Contains(strings.ToUpper(response), "+QPCMV:"), nil
}

func (a *app) readUSBCFGLocked() ([]string, error) {
	response, err := a.runCallAT(callAudioUSBConfigQuery, 3*time.Second)
	if err != nil {
		return nil, callAudioUnavailable(err)
	}
	if parts, ok := parseUSBCFG(response); ok {
		return parts, nil
	}
	if !callATResponseRejected(response) {
		return nil, &callControlError{
			Code:       "uac_config_unavailable",
			HTTPStatus: http.StatusConflict,
			Message:    "模块返回了无法识别的 USB 配置",
		}
	}

	response, err = a.runCallAT(callAudioUSBConfigLegacyQuery, 3*time.Second)
	if err != nil {
		return nil, callAudioUnavailable(err)
	}
	if parts, ok := parseUSBCFG(response); ok {
		return parts, nil
	}
	return nil, &callControlError{
		Code:       "uac_config_unavailable",
		HTTPStatus: http.StatusConflict,
		Message:    "当前模块固件未提供可识别的 UAC USB 配置",
	}
}

func (a *app) callAudioStatusLocked() (callAudioCapability, error) {
	capability := callAudioCapability{Diagnostics: []string{}}
	parts, configErr := a.readUSBCFGLocked()
	if configErr == nil {
		configured, known := usbConfigUACEnabled(parts)
		capability.USBFunctionCount = usbFunctionCount(parts)
		capability.USBConfigKnown = known
		capability.UACConfigured = configured
		capability.NeedsUSBReconfigure = known && !configured
		if !known && capability.USBFunctionCount == 6 {
			capability.Diagnostics = append(capability.Diagnostics,
				"USBCFG 仅返回 6 个 USB 功能参数；当前固件不支持通过 AT 启用 UAC 声卡")
		}
	} else {
		var controlErr *callControlError
		if !errors.As(configErr, &controlErr) || controlErr.Code == "call_audio_unavailable" {
			return capability, configErr
		}
		capability.Diagnostics = append(capability.Diagnostics, controlErr.Message)
	}

	supported, err := a.probeCallAudioSupportLocked()
	if err != nil {
		return capability, err
	}
	capability.Supported = supported
	if !capability.Supported {
		capability.Diagnostics = append(capability.Diagnostics, "模块固件未报告 QPCMV/UAC 支持")
		return capability, nil
	}

	runtimeResponse, err := a.runCallAT(callAudioRuntimeQuery, 3*time.Second)
	if err != nil {
		return capability, callAudioUnavailable(err)
	}
	if callATResponseRejected(runtimeResponse) {
		capability.Diagnostics = append(capability.Diagnostics,
			"固件虽然声明 QPCMV 命令，但拒绝读取运行状态，无法启用 UAC 语音转发")
		return capability, nil
	}
	capability.Enabled, capability.Mode, capability.RuntimeStatusKnown = parseQPCMVStatus(runtimeResponse)
	capability.RuntimeControlAvailable = capability.RuntimeStatusKnown
	if !capability.RuntimeStatusKnown {
		capability.Diagnostics = append(capability.Diagnostics, "模块返回了无法识别的 QPCMV 状态")
	}
	return capability, nil
}

func (a *app) callAudioStatus() (callAudioCapability, error) {
	a.callActionMu.Lock()
	defer a.callActionMu.Unlock()
	return a.callAudioStatusLocked()
}

func (a *app) enableCallAudio(allowUSBReconfigure bool) (callAudioEnableResult, error) {
	a.callActionMu.Lock()
	defer a.callActionMu.Unlock()
	if a.activeCallCopy() != nil {
		return callAudioEnableResult{}, &callControlError{
			Code:       "call_state_conflict",
			HTTPStatus: http.StatusConflict,
			Message:    "请先结束当前通话，再修改模块音频配置",
		}
	}

	parts, err := a.readUSBCFGLocked()
	if err != nil {
		return callAudioEnableResult{}, err
	}
	configured, known := usbConfigUACEnabled(parts)
	if !known {
		return callAudioEnableResult{}, &callControlError{
			Code:       "uac_usb_config_unsupported",
			HTTPStatus: http.StatusConflict,
			Message:    "当前 USBCFG 没有第 7 个 USB 功能参数，固件不支持通过 AT 启用 UAC",
			Detail:     "检测到 " + strconv.Itoa(usbFunctionCount(parts)) + " 个 USB 功能参数",
		}
	}
	supported, err := a.probeCallAudioSupportLocked()
	if err != nil {
		return callAudioEnableResult{}, err
	}
	if !supported {
		return callAudioEnableResult{}, &callControlError{
			Code:       "call_audio_unsupported",
			HTTPStatus: http.StatusConflict,
			Message:    "当前模块固件未报告 QPCMV/UAC 支持，未修改 USB 配置",
		}
	}
	if !configured {
		if !allowUSBReconfigure {
			return callAudioEnableResult{}, &callControlError{
				Code:       "uac_reconfigure_confirmation_required",
				HTTPStatus: http.StatusConflict,
				Message:    "启用 UAC 会修改模块 USB 组合并需要重启，请先确认该操作",
			}
		}
		command, ok := enableUACInUSBCFG(parts)
		if !ok {
			return callAudioEnableResult{}, &callControlError{
				Code:       "uac_config_unavailable",
				HTTPStatus: http.StatusConflict,
				Message:    "无法生成安全的 UAC USB 配置",
			}
		}
		response, runErr := a.runCallAT(command, 5*time.Second)
		if err := callAudioCommandError("usb_config", command, response, runErr); err != nil {
			return callAudioEnableResult{}, err
		}
		return callAudioEnableResult{
			OK: true, Status: "restart_required", RestartRequired: true,
		}, nil
	}

	runtimeResponse, runErr := a.runCallAT(callAudioRuntimeQuery, 3*time.Second)
	if runErr != nil {
		return callAudioEnableResult{}, callAudioUnavailable(runErr)
	}
	if callATResponseRejected(runtimeResponse) {
		return callAudioEnableResult{}, &callControlError{
			Code:       "uac_runtime_control_unavailable",
			HTTPStatus: http.StatusConflict,
			Message:    "当前固件声明了 QPCMV，但禁用了 UAC 运行状态读写",
			Detail:     callAudioRuntimeQuery + " → " + strings.TrimSpace(strings.ReplaceAll(runtimeResponse, "\r\n", " ")),
		}
	}
	enabled, mode, known := parseQPCMVStatus(runtimeResponse)
	if !known {
		return callAudioEnableResult{}, &callControlError{
			Code:       "uac_runtime_control_unavailable",
			HTTPStatus: http.StatusConflict,
			Message:    "模块返回了无法识别的 QPCMV 运行状态，未发送启用命令",
			Detail:     callAudioRuntimeQuery + " → " + strings.TrimSpace(strings.ReplaceAll(runtimeResponse, "\r\n", " ")),
		}
	}
	if enabled && mode == 2 {
		capability, statusErr := a.callAudioStatusLocked()
		if statusErr != nil {
			return callAudioEnableResult{}, statusErr
		}
		return callAudioEnableResult{
			OK: true, Status: "enabled", Capability: &capability,
		}, nil
	}

	response, runErr := a.runCallAT(callAudioEnableCommand, 5*time.Second)
	if err := callAudioCommandError("runtime_enable", callAudioEnableCommand, response, runErr); err != nil {
		return callAudioEnableResult{}, err
	}
	capability, err := a.callAudioStatusLocked()
	if err != nil {
		return callAudioEnableResult{}, err
	}
	return callAudioEnableResult{
		OK: true, Status: "enabled", Capability: &capability,
	}, nil
}

func (a *app) callAudioStatusHTTP(w http.ResponseWriter, _ *http.Request) {
	capability, err := a.callAudioStatus()
	if err != nil {
		writeCallControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, capability)
}

func (a *app) enableCallAudioHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AllowUSBReconfigure bool `json:"allow_usb_reconfigure"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	result, err := a.enableCallAudio(body.AllowUSBReconfigure)
	if err != nil {
		writeCallControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
