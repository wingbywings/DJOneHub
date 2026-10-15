package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const moduleSetupVerifyWindow = 60 * time.Second

// moduleSetupStatus is persisted per Device ID so a USB identity change and
// Runtime replacement can continue the same setup transaction safely.
type moduleSetupStatus struct {
	Stage                string    `json:"stage,omitempty"`
	OriginalStage        string    `json:"original_stage,omitempty"`
	OriginalError        string    `json:"original_error,omitempty"`
	RestartError         string    `json:"restart_error,omitempty"`
	Rollback             bool      `json:"rollback,omitempty"`
	RestartEnumeration   string    `json:"restart_enumeration,omitempty"`
	RestartPhysicalID    string    `json:"restart_physical_id,omitempty"`
	VerifyDeadline       time.Time `json:"verify_deadline,omitempty"`
	TransactionID        string    `json:"transaction_id,omitempty"`
	State                string    `json:"state"`
	Summary              string    `json:"summary"`
	Detail               string    `json:"detail,omitempty"`
	CanInitialize        bool      `json:"can_initialize"`
	RequiresConfirmation bool      `json:"requires_confirmation"`
	RequiresADBPasscode  bool      `json:"requires_adb_passcode,omitempty"`
	ADBChallenge         string    `json:"adb_challenge,omitempty"`
	BackupPath           string    `json:"backup_path,omitempty"`
	UpdatedAt            string    `json:"updated_at"`
}

type usbComposition struct {
	VendorID  int   `json:"vendor_id"`
	ProductID int   `json:"product_id"`
	Flags     []int `json:"flags"`
}

type moduleSetupBackup struct {
	FormatVersion    int            `json:"format_version"`
	DeviceID         string         `json:"device_id"`
	PhysicalID       string         `json:"physical_id"`
	SavedAt          string         `json:"saved_at"`
	USB              usbComposition `json:"usb"`
	IMSConfiguration int            `json:"ims_configuration"`
	VoLTEDisable     int            `json:"volte_disable"`
}

func (c usbComposition) command() string {
	parts := []string{fmt.Sprintf("0x%04X", c.VendorID), fmt.Sprintf("0x%04X", c.ProductID)}
	for _, flag := range c.Flags {
		parts = append(parts, strconv.Itoa(flag))
	}
	return `AT+QCFG="USBCFG",` + strings.Join(parts, ",")
}

func (c usbComposition) hasUAC() bool {
	return len(c.Flags) == 7 && c.Flags[6] == 1
}

func (c usbComposition) hasADB() bool {
	return len(c.Flags) == 7 && c.Flags[5] == 1
}

func (c usbComposition) isUACTarget() bool {
	return len(c.Flags) == 7 && equalIntSlice(c.Flags, []int{1, 1, 1, 1, 1, 1, 1}) &&
		((c.VendorID == quectelUSBVendorID && c.ProductID == quectelUSBProductID) ||
			(c.VendorID == djiUSBVendorID && c.ProductID == djiUSBProductID))
}

func (c usbComposition) isLegacyUACTarget() bool {
	return len(c.Flags) == 7 && equalIntSlice(c.Flags, []int{1, 1, 1, 1, 1, 0, 1}) &&
		((c.VendorID == quectelUSBVendorID && c.ProductID == quectelUSBProductID) ||
			(c.VendorID == djiUSBVendorID && c.ProductID == djiUSBProductID))
}

func (c usbComposition) isCallAudioCapable() bool {
	return c.isUACTarget() || c.isLegacyUACTarget()
}

func (c usbComposition) isFactoryDJI() bool {
	return c.VendorID == djiUSBVendorID && c.ProductID == djiUSBProductID &&
		len(c.Flags) == 7 && equalIntSlice(c.Flags, []int{1, 1, 1, 1, 1, 0, 0})
}

func (c usbComposition) hasSupportedVoiceUSBIdentity() bool {
	return (c.VendorID == quectelUSBVendorID && c.ProductID == quectelUSBProductID) ||
		(c.VendorID == djiUSBVendorID && c.ProductID == djiUSBProductID)
}

// Factory DJI layouts and legacy UAC layouts can both have their ADB bit pinned
// to zero by QADBKEY. Unlock before the first attempt to write the voice target;
// waiting for UAC to be enabled would make a factory layout fail and roll back
// before the unlock path can ever run.
func (c usbComposition) needsADBUnlockForVoice() bool {
	return len(c.Flags) == 7 && c.hasSupportedVoiceUSBIdentity() && !c.hasADB()
}

func (c usbComposition) isRecoverable() bool {
	if c.VendorID < 0 || c.VendorID > 0xffff || c.ProductID < 0 || c.ProductID > 0xffff || len(c.Flags) != 7 {
		return false
	}
	for _, flag := range c.Flags {
		if flag != 0 && flag != 1 {
			return false
		}
	}
	return true
}

func equalIntSlice(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

// Voice routing deploys and controls the module runtime over ADB. A legacy UAC
// layout with the ADB bit pinned to zero is therefore not equivalent to the
// requested target, even though macOS can already enumerate its audio function.
func usbSetupReadbackMatches(want, actual usbComposition) bool {
	return actual.command() == want.command()
}

var qadbChallengePattern = regexp.MustCompile(`(?im)\+QADBKEY:\s*([0-9]{1,8})\s*$`) // challenge only; never a passcode
var qadbChallengeValuePattern = regexp.MustCompile(`^[0-9]{1,8}$`)
var qadbPasscodePattern = regexp.MustCompile(`^[./A-Za-z0-9]{8,128}$`)

func parseQADBChallenge(response string) (string, error) {
	match := qadbChallengePattern.FindStringSubmatch(response)
	if len(match) != 2 {
		return "", errors.New("模块没有返回可识别的 QADBKEY challenge")
	}
	return match[1], nil
}

func validateQADBPasscode(passcode string) error {
	if !qadbPasscodePattern.MatchString(passcode) {
		return errors.New("QADBKEY passcode 格式无效；只能包含 8–128 位 MD5-crypt 字符")
	}
	return nil
}

func targetVoiceUSB(current usbComposition) usbComposition {
	if current.isUACTarget() {
		return current
	}
	return usbComposition{VendorID: quectelUSBVendorID, ProductID: quectelUSBProductID, Flags: []int{1, 1, 1, 1, 1, 1, 1}}
}

func parseUSBComposition(response string) (usbComposition, error) {
	for _, line := range strings.Split(strings.ReplaceAll(response, "\r", ""), "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(strings.ToLower(line), `+qcfg: "usbcfg",`) {
			continue
		}
		comma := strings.Index(line, ",")
		if comma < 0 {
			break
		}
		fields := strings.Split(line[comma+1:], ",")
		if len(fields) != 9 {
			return usbComposition{}, fmt.Errorf("USBCFG 字段数为 %d，需要 9", len(fields))
		}
		parse := func(raw string) (int, error) {
			value, err := strconv.ParseInt(strings.TrimSpace(raw), 0, 32)
			return int(value), err
		}
		vendorID, err := parse(fields[0])
		if err != nil {
			return usbComposition{}, fmt.Errorf("parse USB vendor: %w", err)
		}
		productID, err := parse(fields[1])
		if err != nil {
			return usbComposition{}, fmt.Errorf("parse USB product: %w", err)
		}
		flags := make([]int, 0, 7)
		for _, field := range fields[2:] {
			value, err := parse(field)
			if err != nil {
				return usbComposition{}, fmt.Errorf("parse USB flag: %w", err)
			}
			flags = append(flags, value)
		}
		return usbComposition{VendorID: vendorID, ProductID: productID, Flags: flags}, nil
	}
	return usbComposition{}, errors.New("模块没有返回可识别的 USBCFG")
}

func parseIMSConfiguration(response string) (configuration, capability int, err error) {
	values, err := parseQCFGIntegers(response, "ims", 2)
	if err != nil {
		return 0, 0, err
	}
	return values[0], values[1], nil
}

func parseVoLTEDisable(response string) (int, error) {
	normalized := strings.ReplaceAll(strings.ToLower(response), "volte_disable", "volte/disable")
	values, err := parseQCFGIntegers(normalized, "volte/disable", 1)
	if err != nil {
		return 0, err
	}
	return values[0], nil
}

func parseQCFGIntegers(response, key string, count int) ([]int, error) {
	needle := `+qcfg: "` + strings.ToLower(key) + `",`
	for _, line := range strings.Split(strings.ReplaceAll(response, "\r", ""), "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(strings.ToLower(line), needle) {
			continue
		}
		comma := strings.Index(line, ",")
		fields := strings.Split(line[comma+1:], ",")
		if len(fields) < count {
			break
		}
		values := make([]int, 0, count)
		for _, field := range fields[:count] {
			value, err := strconv.Atoi(strings.TrimSpace(field))
			if err != nil {
				return nil, fmt.Errorf("parse %s: %w", key, err)
			}
			values = append(values, value)
		}
		return values, nil
	}
	return nil, fmt.Errorf("模块没有返回可识别的 %s 状态", key)
}

func setupATResponseIsError(response string) bool {
	normalized := strings.ToUpper(strings.ReplaceAll(response, "\r\n", "\n"))
	return strings.Contains(normalized, "\nERROR\n") || strings.HasSuffix(normalized, "\nERROR") ||
		strings.Contains(normalized, "+CME ERROR:") || strings.Contains(normalized, "+CMS ERROR:")
}

func (a *app) inspectModuleSetup() moduleSetupStatus {
	now := time.Now().Format(time.RFC3339)
	if a.demo {
		return moduleSetupStatus{State: "ready", Summary: "演示模块已完成初始化", UpdatedAt: now}
	}
	response, err := a.runModuleSetupAT(`AT+QCFG="USBCFG"`, 4*time.Second)
	if err != nil {
		return moduleSetupStatus{State: "disconnected", Summary: "等待 4G 模块连接", Detail: err.Error(), UpdatedAt: now}
	}
	composition, err := parseUSBComposition(response)
	if err != nil {
		state := "unsupported"
		summary := "无法识别模块 USB 配置"
		if setupATResponseIsError(response) {
			state = "reconnecting"
			summary = "正在重新连接并读取 USB 配置"
		}
		return moduleSetupStatus{State: state, Summary: summary, Detail: err.Error(), UpdatedAt: now}
	}
	imsResponse, imsErr := a.runModuleSetupAT(`AT+QCFG="ims"`, 3*time.Second)
	imsConfiguration, imsCapability, imsParseErr := parseIMSConfiguration(imsResponse)
	volteResponse, volteErr := a.runModuleSetupAT(`AT+QCFG="volte_disable"`, 3*time.Second)
	volteDisable, volteParseErr := parseVoLTEDisable(volteResponse)
	if composition.isUACTarget() && imsErr == nil && imsParseErr == nil && volteErr == nil && volteParseErr == nil && imsConfiguration == 1 && imsCapability == 1 && volteDisable == 0 {
		return moduleSetupStatus{State: "ready", Summary: "模块已具备 USB 音频与 VoLTE 能力", Detail: composition.command(), UpdatedAt: now}
	}
	if !composition.isRecoverable() {
		return moduleSetupStatus{State: "unsupported", Summary: "模块 USB 配置无法安全备份和恢复", Detail: composition.command(), UpdatedAt: now}
	}
	detail := composition.command()
	requiresADBPasscode := false
	adbChallenge := ""
	if composition.needsADBUnlockForVoice() {
		if qadbResponse, qadbErr := a.runModuleSetupAT("AT+QADBKEY?", 4*time.Second); qadbErr == nil {
			if challenge, parseErr := parseQADBChallenge(qadbResponse); parseErr == nil {
				requiresADBPasscode = true
				adbChallenge = challenge
				detail += "；ADB 已锁定，初始化时将自动生成 QADBKEY passcode"
			}
		}
	}
	if imsErr != nil || imsParseErr != nil || volteErr != nil || volteParseErr != nil {
		detail = firstSetupDetail(detail, errorText(imsErr), errorText(imsParseErr), errorText(volteErr), errorText(volteParseErr))
	}
	return moduleSetupStatus{
		State: "needs_initialization", Summary: setupInitializationSummary(composition.hasUAC(), requiresADBPasscode), Detail: detail,
		CanInitialize: true, RequiresConfirmation: true, RequiresADBPasscode: requiresADBPasscode,
		ADBChallenge: adbChallenge, UpdatedAt: now,
	}
}

func setupInitializationSummary(hasUAC, requiresADBPasscode bool) string {
	if requiresADBPasscode {
		if hasUAC {
			return "USB 音频已启用，将自动解锁 ADB"
		}
		return "将自动解锁 ADB 并启用 USB 音频"
	}
	return "可备份当前配置并启用通话支持"
}

func moduleSetupIsTransient(state string) bool {
	return state == "initializing" || state == "restarting" || state == "verifying" || state == "rolling_back" || state == "rollback_restarting" || state == "rollback_verifying"
}

func moduleSetupIsCachedTerminal(state string) bool {
	return state == "ready" || state == "failed" || state == "rolled_back"
}

func (a *app) moduleSetupStatusAPI(w http.ResponseWriter, _ *http.Request) {
	a.moduleSetupMu.RLock()
	current := a.moduleSetup
	a.moduleSetupMu.RUnlock()
	if current.State == "failed" || current.State == "rolled_back" {
		a.operationMu.RLock()
		inspection := a.inspectModuleSetup()
		a.operationMu.RUnlock()
		if inspection.State == "ready" {
			inspection.OriginalError = current.OriginalError
			inspection.RestartError = current.RestartError
			writeJSON(w, http.StatusOK, inspection)
			return
		}
		if inspection.CanInitialize {
			current.CanInitialize = true
			current.RequiresConfirmation = true
			current.RequiresADBPasscode = inspection.RequiresADBPasscode
			current.ADBChallenge = inspection.ADBChallenge
			current.UpdatedAt = inspection.UpdatedAt
			writeJSON(w, http.StatusOK, current)
			return
		}
		writeJSON(w, http.StatusOK, inspection)
		return
	}
	if moduleSetupIsTransient(current.State) || moduleSetupIsCachedTerminal(current.State) {
		if current.State == "ready" {
			go a.warmModuleVoiceIfReady()
		}
		writeJSON(w, http.StatusOK, current)
		return
	}
	a.operationMu.RLock()
	status := a.inspectModuleSetup()
	a.operationMu.RUnlock()
	if status.State == "ready" {
		go a.warmModuleVoiceIfReady()
	}
	writeJSON(w, http.StatusOK, status)
}

func (a *app) warmModuleVoiceIfReady() {
	a.moduleSetupMu.RLock()
	state := a.moduleSetup.State
	a.moduleSetupMu.RUnlock()
	// An empty state means the readiness came from a fresh live inspection
	// rather than a persisted setup transaction.
	if state != "" && state != "ready" {
		return
	}
	if a.hasActiveCall() {
		return
	}
	a.moduleVoiceMu.Lock()
	if a.moduleVoicePrepared || a.moduleVoicePreparing {
		a.moduleVoiceMu.Unlock()
		return
	}
	a.moduleVoicePreparing = true
	a.moduleVoiceMu.Unlock()
	defer func() {
		a.moduleVoiceMu.Lock()
		a.moduleVoicePreparing = false
		a.moduleVoiceMu.Unlock()
	}()
	if err := a.prepareModuleVoiceSession(); err != nil {
		// The next status refresh or pre-call check retries. Do not turn an idle
		// background optimization into a user-visible setup failure.
		return
	}
}

func (a *app) moduleSetupStartAPI(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Confirm bool `json:"confirm"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !body.Confirm {
		writeError(w, http.StatusBadRequest, "需要明确确认后才会修改模块 USB、IMS 和 VoLTE 配置")
		return
	}
	if a.hasActiveCall() {
		writeError(w, http.StatusConflict, "通话期间不能修改模块 USB、IMS 或 VoLTE 配置")
		return
	}
	a.moduleSetupMu.RLock()
	running := moduleSetupIsTransient(a.moduleSetup.State)
	a.moduleSetupMu.RUnlock()
	if running {
		writeError(w, http.StatusConflict, "模块通话支持正在初始化")
		return
	}
	a.operationMu.RLock()
	inspection := a.inspectModuleSetup()
	a.operationMu.RUnlock()
	if !inspection.CanInitialize {
		writeError(w, http.StatusConflict, inspection.Summary)
		return
	}
	if inspection.RequiresADBPasscode {
		if !qadbChallengeValuePattern.MatchString(inspection.ADBChallenge) {
			writeError(w, http.StatusConflict, "模块未返回可用于自动解锁的 QADBKEY challenge")
			return
		}
	}
	transactionID := time.Now().UTC().Format("20060102T150405.000000000Z")
	if !a.beginModuleSetup(moduleSetupStatus{
		TransactionID: transactionID, State: "initializing", Stage: "backup", Summary: "正在备份并启用通话支持",
		Detail: inspection.Detail,
	}) {
		writeError(w, http.StatusConflict, "模块通话支持正在初始化")
		return
	}
	go a.runModuleSetup(transactionID)
	a.moduleSetupMu.RLock()
	status := a.moduleSetup
	a.moduleSetupMu.RUnlock()
	writeJSON(w, http.StatusAccepted, status)
}

func (a *app) beginModuleSetup(status moduleSetupStatus) bool {
	a.moduleSetupMu.Lock()
	if moduleSetupIsTransient(a.moduleSetup.State) || a.moduleSetupResuming {
		a.moduleSetupMu.Unlock()
		return false
	}
	previous := a.moduleSetup
	status.UpdatedAt = time.Now().Format(time.RFC3339)
	a.moduleSetup = status
	err := a.persistModuleSetupStateLocked()
	a.moduleSetupMu.Unlock()
	a.logModuleSetupTransition(previous, status, err)
	return true
}

func (a *app) runModuleSetup(transactionID string) {
	a.operationMu.Lock()
	defer func() {
		a.operationMu.Unlock()
		ctx := a.moduleSetupContext
		if ctx == nil {
			ctx = context.Background()
		}
		go a.resumeModuleSetup(ctx)
	}()

	if a.hasActiveCall() {
		a.failModuleSetup(transactionID, "初始化取消，模块已有通话", errors.New("通话期间不能修改模块配置"), "")
		return
	}
	a.moduleSetupStage("backup", "正在读取并备份原始配置")
	backup, err := a.readModuleSetupBackup()
	if err != nil {
		a.failModuleSetup(transactionID, "启用前校验未通过", err, "")
		return
	}
	backupPath, err := a.saveModuleSetupBackup(transactionID, backup)
	if err != nil {
		a.failModuleSetup(transactionID, "无法保存模块回滚备份", err, "")
		return
	}
	a.updateModuleSetup(transactionID, moduleSetupStatus{State: "initializing", Summary: "备份已保存，正在写入并回读配置", BackupPath: backupPath})

	if backup.USB.needsADBUnlockForVoice() {
		a.moduleSetupStage("adb_unlock", "正在自动解锁 ADB")
		if err := a.unlockModuleADBForVoiceIfNeeded(); err != nil {
			a.failModuleSetup(transactionID, "ADB 解锁失败，未修改模块配置", err, backupPath)
			return
		}
	}
	targetUSB := targetVoiceUSB(backup.USB)
	a.moduleSetupStage("configure", "正在写入并回读 USB、IMS 与 VoLTE 配置")
	if err := a.writeAndVerifySetup(targetUSB, 1, 0); err != nil {
		a.rollbackModuleSetup(transactionID, backupPath, backup, "写入或回读验证失败："+err.Error())
		return
	}
	if err := a.requestModuleSetupRestart(transactionID, backupPath, false); err != nil {
		a.rollbackModuleSetup(transactionID, backupPath, backup, "重启命令失败："+err.Error())
	}
}

func (a *app) unlockModuleADBForVoiceIfNeeded() error {
	response, err := a.runModuleSetupAT("AT+QADBKEY?", 4*time.Second)
	if err != nil {
		return errors.New("读取 QADBKEY challenge 时通信失败")
	}
	// Some supported firmware does not implement QADBKEY and allows USBCFG to
	// enable ADB directly. Preserve that path; exact readback still catches a
	// firmware that silently keeps ADB disabled.
	if setupATResponseIsError(response) {
		return nil
	}
	challenge, err := parseQADBChallenge(response)
	if err != nil {
		return err
	}
	passcode, err := generateQADBPasscode(challenge)
	challenge = ""
	if err != nil {
		return err
	}
	err = a.unlockModuleADB(passcode)
	passcode = ""
	return err
}

func (a *app) unlockModuleADB(passcode string) error {
	if err := validateQADBPasscode(passcode); err != nil {
		return err
	}
	// Do not route this through writeSetupCommand: its diagnostic includes the
	// command text, which would leak the device-specific passcode to UI/logs.
	response, err := a.runSecretUSBATCommand(`AT+QADBKEY="`+passcode+`"`, 8*time.Second)
	passcode = ""
	if err != nil {
		return errors.New("提交 QADBKEY passcode 时通信失败")
	}
	if setupATResponseIsError(response) {
		return errors.New("模块拒绝自动生成的 QADBKEY passcode")
	}
	return nil
}

// runSecretUSBATCommand deliberately bypasses modem.Manager because its
// timeout diagnostics include the raw AT command. QADBKEY credentials must
// never enter application logs, including error paths.
func (a *app) runSecretUSBATCommand(command string, timeout time.Duration) (string, error) {
	if a.demo {
		return "OK", nil
	}
	if a.usbAT == nil {
		if err := a.ensureUSBAT(); err != nil {
			return "", err
		}
	}
	if a.usbAT == nil {
		return "", errors.New("安全 USB AT 通道不可用")
	}
	response, err := a.usbAT.Command(command, timeout)
	if err != nil {
		a.resetUSBATIfGone(err)
	}
	return response, err
}

func (a *app) readModuleSetupBackup() (moduleSetupBackup, error) {
	usbResponse, err := a.readModuleSetupSetting(`AT+QCFG="USBCFG"`, 5*time.Second)
	if err != nil {
		return moduleSetupBackup{}, fmt.Errorf("读取USB 配置：%w", err)
	}
	usb, err := parseUSBComposition(usbResponse)
	if err != nil || !usb.isRecoverable() {
		return moduleSetupBackup{}, firstSetupError(err, "模块 USB 配置不完整，拒绝写入")
	}
	imsResponse, err := a.readModuleSetupSetting(`AT+QCFG="ims"`, 4*time.Second)
	if err != nil {
		return moduleSetupBackup{}, fmt.Errorf("读取IMS 配置：%w", err)
	}
	ims, _, err := parseIMSConfiguration(imsResponse)
	if err != nil {
		return moduleSetupBackup{}, err
	}
	volteResponse, err := a.readModuleSetupSetting(`AT+QCFG="volte_disable"`, 4*time.Second)
	if err != nil {
		return moduleSetupBackup{}, fmt.Errorf("读取VoLTE 配置：%w", err)
	}
	volte, err := parseVoLTEDisable(volteResponse)
	if err != nil {
		return moduleSetupBackup{}, err
	}
	physicalID := ""
	if a.usbLocator != nil {
		physicalID = a.usbLocator.PhysicalID()
	}
	return moduleSetupBackup{
		FormatVersion: 1, DeviceID: a.deviceID, PhysicalID: physicalID, SavedAt: time.Now().Format(time.RFC3339),
		USB: usb, IMSConfiguration: ims, VoLTEDisable: volte,
	}, nil
}

func (a *app) writeAndVerifySetup(usb usbComposition, imsConfiguration, volteDisable int) error {
	if err := a.writeSetupCommand(usb.command()); err != nil {
		return err
	}
	if err := a.writeSetupCommand(fmt.Sprintf(`AT+QCFG="volte_disable",%d`, volteDisable)); err != nil {
		return err
	}
	if err := a.writeSetupCommand(fmt.Sprintf(`AT+QCFG="ims",%d`, imsConfiguration)); err != nil {
		return err
	}
	// Check stored settings before reboot. The runtime IMS capability may
	// still be zero until reboot; the post-reboot check requires it to be one.
	return a.verifyModuleSetupValues(usb, imsConfiguration, volteDisable, false)
}

func (a *app) writeSetupCommand(command string) error {
	response, err := a.runModuleSetupAT(command, 8*time.Second)
	if err != nil {
		return fmt.Errorf("写入 %s：%w", command, err)
	}
	if setupATResponseIsError(response) || !atProbeSucceeded(response) {
		return fmt.Errorf("模块拒绝 %s：%s", command, strings.TrimSpace(response))
	}
	return nil
}

func (a *app) verifyModuleSetupValues(wantUSB usbComposition, wantIMS, wantVoLTE int, requireCapability bool) error {
	usbResponse, err := a.readModuleSetupSetting(`AT+QCFG="USBCFG"`, 5*time.Second)
	if err != nil {
		return fmt.Errorf("读取 USB 配置：%w", err)
	}
	actualUSB, err := parseUSBComposition(usbResponse)
	if err != nil || !usbSetupReadbackMatches(wantUSB, actualUSB) {
		return firstSetupError(err, fmt.Sprintf("USB 配置回读不一致：%s", strings.TrimSpace(usbResponse)))
	}
	imsResponse, err := a.readModuleSetupSetting(`AT+QCFG="ims"`, 4*time.Second)
	if err != nil {
		return fmt.Errorf("读取 IMS 配置：%w", err)
	}
	actualIMS, capability, err := parseIMSConfiguration(imsResponse)
	if err != nil || actualIMS != wantIMS || (requireCapability && wantIMS == 1 && capability != 1) {
		return firstSetupError(err, fmt.Sprintf("IMS 回读不一致：%s", strings.TrimSpace(imsResponse)))
	}
	volteResponse, err := a.readModuleSetupSetting(`AT+QCFG="volte_disable"`, 4*time.Second)
	if err != nil {
		return fmt.Errorf("读取 VoLTE 配置：%w", err)
	}
	actualVoLTE, err := parseVoLTEDisable(volteResponse)
	if err != nil || actualVoLTE != wantVoLTE {
		return firstSetupError(err, fmt.Sprintf("VoLTE 回读不一致：%s", strings.TrimSpace(volteResponse)))
	}
	return nil
}

// runModuleSetupAT is the transport boundary for setup's non-secret commands.
func (a *app) runModuleSetupAT(command string, timeout time.Duration) (string, error) {
	if a.moduleSetupAT != nil {
		return a.moduleSetupAT(command, timeout)
	}
	return a.runATCommand(command, timeout)
}

func (a *app) readModuleSetupSetting(command string, timeout time.Duration) (string, error) {
	response, err := a.runModuleSetupAT(command, timeout)
	if err != nil {
		return "", err
	}
	if setupATResponseIsError(response) || !atProbeSucceeded(response) {
		return "", fmt.Errorf("%s 未返回完整 OK 回复：%s", command, strings.TrimSpace(response))
	}
	return response, nil
}

func (a *app) moduleSetupStage(stage, summary string) {
	a.moduleSetupMu.RLock()
	current := a.moduleSetup
	a.moduleSetupMu.RUnlock()
	current.Stage, current.Summary = stage, summary
	a.updateModuleSetup(current.TransactionID, current)
}

func setupEnumeration(locator *usbDeviceLocator) string {
	if locator == nil {
		return ""
	}
	return fmt.Sprintf("%s/%04x:%04x/%d", locator.PhysicalID(), locator.VendorID, locator.ProductID, locator.Address)
}

// A USB transfer error during CFUN is ambiguous, not proof of failure or
// success. Persist a pending state, release the old handle, and require a new
// enumeration on the same port plus an exact configuration readback.
func (a *app) requestModuleSetupRestart(transactionID, backupPath string, rollback bool) error {
	state, stage, summary := "restarting", "restart", "正在重启，等待 USB 重新枚举和配置验证"
	if rollback {
		state, stage, summary = "rollback_restarting", "rollback_restart", "原始配置已回读，等待重启后再次验证"
	}
	physicalID := ""
	if a.usbLocator != nil {
		physicalID = a.usbLocator.PhysicalID()
	}
	next := moduleSetupStatus{
		State: state, Stage: stage, Summary: summary, Rollback: rollback, BackupPath: backupPath,
		RestartEnumeration: setupEnumeration(a.usbLocator), RestartPhysicalID: physicalID,
		VerifyDeadline: time.Now().Add(moduleSetupVerifyWindow),
	}
	a.updateModuleSetup(transactionID, next)
	response, err := a.runModuleSetupAT("AT+CFUN=1,1", 5*time.Second)
	if err == nil && (setupATResponseIsError(response) || !atProbeSucceeded(response)) {
		return fmt.Errorf("模块未确认重启命令：%s", strings.TrimSpace(response))
	}
	if err != nil {
		if !isLikelyUSBRestartError(err) {
			return err
		}
		next.RestartError = err.Error()
		next.Detail = "重启时 USB 通信中断，正在等待重新枚举后核验"
		a.updateModuleSetup(transactionID, next)
	}
	a.markUSBATDetached("voice setup awaiting reboot verification")
	return nil
}

func (a *app) rollbackModuleSetup(transactionID, backupPath string, backup moduleSetupBackup, reason string) {
	a.moduleSetupMu.RLock()
	originalStage := a.moduleSetup.Stage
	a.moduleSetupMu.RUnlock()
	a.updateModuleSetup(transactionID, moduleSetupStatus{
		OriginalStage: originalStage,
		State:         "rolling_back", Stage: "rollback", Summary: "正在恢复并回读全部原始配置",
		OriginalError: reason, Detail: reason, Rollback: true, BackupPath: backupPath,
	})
	if err := a.writeAndVerifySetup(backup.USB, backup.IMSConfiguration, backup.VoLTEDisable); err != nil {
		a.failModuleSetup(transactionID, "自动回滚失败，需要人工检查模块", err, backupPath)
		return
	}
	if err := a.requestModuleSetupRestart(transactionID, backupPath, true); err != nil {
		a.failModuleSetup(transactionID, "原始配置已回读，但重启命令未确认", err, backupPath)
	}
}

func moduleSetupAwaitsVerification(state string) bool {
	return state == "restarting" || state == "verifying" || state == "rollback_restarting" || state == "rollback_verifying"
}

func (a *app) resumeModuleSetup(ctx context.Context) {
	a.moduleSetupMu.Lock()
	if a.moduleSetupResuming || !moduleSetupAwaitsVerification(a.moduleSetup.State) {
		a.moduleSetupMu.Unlock()
		return
	}
	a.moduleSetupResuming = true
	a.moduleSetupMu.Unlock()
	defer func() {
		a.moduleSetupMu.Lock()
		a.moduleSetupResuming = false
		a.moduleSetupMu.Unlock()
	}()

	for {
		a.moduleSetupMu.RLock()
		status := a.moduleSetup
		a.moduleSetupMu.RUnlock()
		if !moduleSetupAwaitsVerification(status.State) || ctx.Err() != nil {
			return
		}
		// Old persisted transactions did not have a deadline. Give them one
		// bounded verification window, without resending any write or reboot.
		if status.VerifyDeadline.IsZero() {
			status.VerifyDeadline = time.Now().Add(moduleSetupVerifyWindow)
			a.updateModuleSetup(status.TransactionID, status)
		}
		err := a.waitModuleSetupVerification(ctx, status)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			return
		}
		if status.Rollback {
			a.failModuleSetup(status.TransactionID, "回滚后重启或配置验证未通过，需要检查模块", err, status.BackupPath)
			return
		}
		backup, backupErr := loadModuleSetupBackup(status.BackupPath, a.deviceID)
		if backupErr != nil {
			a.failModuleSetup(status.TransactionID, "重启后验证失败且无法读取回滚备份", fmt.Errorf("%v；%w", err, backupErr), status.BackupPath)
			return
		}
		a.operationMu.Lock()
		a.rollbackModuleSetup(status.TransactionID, status.BackupPath, backup, "重启后验证失败："+err.Error())
		a.operationMu.Unlock()
		// The same worker verifies the rollback reboot; it must not publish
		// rolled_back until the original values are observed after reconnect.
	}
}

func (a *app) waitModuleSetupVerification(ctx context.Context, status moduleSetupStatus) error {
	var backup moduleSetupBackup
	if status.Rollback {
		var err error
		backup, err = loadModuleSetupBackup(status.BackupPath, a.deviceID)
		if err != nil {
			return err
		}
	}
	lastErr := errors.New("尚未确认 USB 重新枚举")
	for time.Now().Before(status.VerifyDeadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if a.operationMu.TryLock() {
			lastErr = a.verifyReconnectedModuleSetup(status, backup)
			if lastErr == nil {
				a.completeModuleSetupVerification(status)
			}
			a.operationMu.Unlock()
			if lastErr == nil {
				return nil
			}
		}
		state, stage := "verifying", "verify"
		if status.Rollback {
			state, stage = "rollback_verifying", "rollback_verify"
		}
		a.updateModuleSetup(status.TransactionID, moduleSetupStatus{
			State: state, Stage: stage, Rollback: status.Rollback, Summary: "正在等待重连并回读配置",
			Detail: lastErr.Error(), BackupPath: status.BackupPath,
		})
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("验证等待超时：%w", lastErr)
}

// Caller holds operationMu so the hub cannot replace the locator/AT handle
// between checking the physical enumeration and reading its configuration.
func (a *app) verifyReconnectedModuleSetup(status moduleSetupStatus, backup moduleSetupBackup) error {
	if status.RestartEnumeration != "" && (a.usbLocator == nil ||
		a.usbLocator.PhysicalID() != status.RestartPhysicalID ||
		setupEnumeration(a.usbLocator) == status.RestartEnumeration) {
		return errors.New("尚未确认原 USB 端口上的新枚举")
	}
	if status.Rollback {
		return a.verifyModuleSetupValues(backup.USB, backup.IMSConfiguration, backup.VoLTEDisable, true)
	}
	return a.verifyCurrentVoiceSetup()
}

func (a *app) completeModuleSetupVerification(status moduleSetupStatus) {
	if status.Rollback {
		a.updateModuleSetup(status.TransactionID, moduleSetupStatus{
			State: "rolled_back", Stage: "complete", Rollback: true,
			Summary: "已重连并确认 USB、IMS 与 VoLTE 原始配置恢复", Detail: status.OriginalError,
			CanInitialize: true, RequiresConfirmation: true, BackupPath: status.BackupPath,
		})
		return
	}
	state, summary := "ready", "模块配置与精确 ADB 绑定已验证"
	detail := "USB、IMS 与 VoLTE 已回读确认"
	if adbDetail, adbErr := a.verifyTargetADB(); adbErr == nil {
		detail += "；" + adbDetail
	} else {
		state, summary = "configured", "模块音频与 VoLTE 已配置，ADB 路由尚未就绪"
		detail += "；ADB 尚未就绪：" + adbErr.Error()
	}
	a.updateModuleSetup(status.TransactionID, moduleSetupStatus{State: state, Stage: "complete", Summary: summary, Detail: detail, BackupPath: status.BackupPath})
	if state == "ready" {
		go a.warmModuleVoiceIfReady()
	}
}

func (a *app) verifyCurrentVoiceSetup() error {
	usbResponse, err := a.readModuleSetupSetting(`AT+QCFG="USBCFG"`, 5*time.Second)
	if err != nil {
		return fmt.Errorf("读取 USB 配置：%w", err)
	}
	usb, err := parseUSBComposition(usbResponse)
	if err != nil || !usb.isUACTarget() {
		return firstSetupError(err, "USB 音频或 ADB 配置尚未生效")
	}
	imsResponse, err := a.readModuleSetupSetting(`AT+QCFG="ims"`, 4*time.Second)
	if err != nil {
		return fmt.Errorf("读取 IMS 配置：%w", err)
	}
	ims, capability, err := parseIMSConfiguration(imsResponse)
	if err != nil || ims != 1 || capability != 1 {
		return firstSetupError(err, "IMS/VoLTE capability 尚未生效")
	}
	volteResponse, err := a.readModuleSetupSetting(`AT+QCFG="volte_disable"`, 4*time.Second)
	if err != nil {
		return fmt.Errorf("读取 VoLTE 配置：%w", err)
	}
	volte, err := parseVoLTEDisable(volteResponse)
	if err != nil || volte != 0 {
		return firstSetupError(err, "VoLTE 仍处于禁用状态")
	}
	return nil
}

func (a *app) verifyTargetADB() (string, error) {
	if a.usbLocator == nil {
		return "", errors.New("缺少目标 USB locator")
	}
	adb, err := openDJIUSBADB(*a.usbLocator)
	if err != nil {
		return "", err
	}
	defer adb.Close()
	output, status, err := adb.shellChecked("id -u; uname -r", 8*time.Second)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(output)
	if status != 0 || len(fields) < 2 || fields[0] != "0" {
		return "", fmt.Errorf("ADB root/内核探测失败：%s", strings.TrimSpace(output))
	}
	return "ADB root，内核 " + fields[1], nil
}

func (a *app) setModuleSetup(status moduleSetupStatus) {
	status.UpdatedAt = time.Now().Format(time.RFC3339)
	a.moduleSetupMu.Lock()
	previous := a.moduleSetup
	a.moduleSetup = status
	err := a.persistModuleSetupStateLocked()
	a.moduleSetupMu.Unlock()
	a.logModuleSetupTransition(previous, status, err)
}

func (a *app) logModuleSetupTransition(previous, status moduleSetupStatus, persistErr error) {
	if previous.State != status.State || previous.Stage != status.Stage || previous.Detail != status.Detail || previous.OriginalError != status.OriginalError || previous.RestartError != status.RestartError {
		log.Printf("module setup device=%s transaction=%s state=%s stage=%s summary=%q detail=%q original_stage=%s original_error=%q restart_error=%q", a.deviceID, status.TransactionID, status.State, status.Stage, status.Summary, status.Detail, status.OriginalStage, status.OriginalError, status.RestartError)
	}
	if persistErr != nil {
		logSetupPersistenceError(a.deviceID, persistErr)
	}
}

func (a *app) updateModuleSetup(transactionID string, next moduleSetupStatus) {
	// Compare and publish under one lock so a late recovery worker cannot
	// overwrite a newer transaction after passing a stale ID check.
	a.moduleSetupMu.Lock()
	current := a.moduleSetup
	if current.TransactionID != "" && current.TransactionID != transactionID {
		a.moduleSetupMu.Unlock()
		return
	}
	next.TransactionID = transactionID
	if next.Stage == "" {
		next.Stage = current.Stage
	}
	if current.OriginalError != "" {
		next.OriginalError = current.OriginalError
		next.OriginalStage = current.OriginalStage
	}
	if next.RestartError == "" {
		next.RestartError = current.RestartError
	}
	if next.RestartEnumeration == "" {
		next.RestartEnumeration = current.RestartEnumeration
		next.RestartPhysicalID = current.RestartPhysicalID
	}
	if next.VerifyDeadline.IsZero() {
		next.VerifyDeadline = current.VerifyDeadline
	}
	next.Rollback = next.Rollback || current.Rollback
	next.UpdatedAt = time.Now().Format(time.RFC3339)
	a.moduleSetup = next
	err := a.persistModuleSetupStateLocked()
	a.moduleSetupMu.Unlock()
	a.logModuleSetupTransition(current, next, err)
}

func (a *app) failModuleSetup(transactionID, summary string, err error, backupPath string) {
	a.moduleSetupMu.RLock()
	original, stage := a.moduleSetup.OriginalError, a.moduleSetup.Stage
	a.moduleSetupMu.RUnlock()
	if original == "" {
		original = errorText(err)
	}
	a.updateModuleSetup(transactionID, moduleSetupStatus{State: "failed", Summary: summary, Detail: errorText(err), OriginalStage: stage, OriginalError: original, BackupPath: backupPath})
}

func (a *app) loadModuleSetupState() error {
	if a.moduleSetupPath == "" {
		return nil
	}
	data, err := os.ReadFile(a.moduleSetupPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var status moduleSetupStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return err
	}
	a.moduleSetupMu.Lock()
	a.moduleSetup = status
	a.moduleSetupMu.Unlock()
	return nil
}

func (a *app) persistModuleSetupStateLocked() error {
	if a.moduleSetupPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(a.moduleSetupPath), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(a.moduleSetup, "", "  ")
	if err != nil {
		return err
	}
	return atomicWritePrivateFile(a.moduleSetupPath, data)
}

func (a *app) saveModuleSetupBackup(transactionID string, backup moduleSetupBackup) (string, error) {
	if a.moduleSetupBackupDir == "" {
		return "", errors.New("模块备份目录未配置")
	}
	if err := os.MkdirAll(a.moduleSetupBackupDir, 0o700); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(backup, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(a.moduleSetupBackupDir, "voice-setup-"+transactionID+".json")
	return path, atomicWritePrivateFile(path, data)
}

func loadModuleSetupBackup(path, deviceID string) (moduleSetupBackup, error) {
	if path == "" {
		return moduleSetupBackup{}, errors.New("回滚备份路径为空")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return moduleSetupBackup{}, err
	}
	var backup moduleSetupBackup
	if err := json.Unmarshal(data, &backup); err != nil {
		return moduleSetupBackup{}, err
	}
	if backup.FormatVersion != 1 || backup.DeviceID != deviceID || !backup.USB.isRecoverable() {
		return moduleSetupBackup{}, errors.New("回滚备份与当前设备不匹配")
	}
	return backup, nil
}

func atomicWritePrivateFile(path string, data []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".djonehub-setup-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func isLikelyUSBDetachError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToUpper(err.Error())
	return strings.Contains(text, "NO_DEVICE") || strings.Contains(text, "NOT_FOUND") || strings.Contains(text, "TIMED OUT") || strings.Contains(text, "TIMEOUT")
}

// Only use this around a reboot command. IO/PIPE are not a successful write,
// and must never be used to accept a configuration change without readback.
func isLikelyUSBRestartError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToUpper(err.Error())
	return isLikelyUSBDetachError(err) || strings.Contains(text, "LIBUSB_ERROR_IO") || strings.Contains(text, "LIBUSB_ERROR_PIPE")
}

func firstSetupError(err error, fallback string) error {
	if err != nil {
		return err
	}
	return errors.New(fallback)
}

func firstSetupDetail(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func logSetupPersistenceError(deviceID string, err error) {
	fmt.Fprintf(os.Stderr, "DJOneHub: persist voice setup for %s: %v\n", deviceID, err)
}
