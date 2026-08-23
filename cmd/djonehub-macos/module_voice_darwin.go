//go:build darwin && cgo

package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

const (
	voiceRemoteDir        = "/tmp/djonehub-call"
	voiceRoutePIDFile     = "/run/djonehub-voice-route.pid"
	voiceRouteLogFile     = "/run/djonehub-voice-route.log"
	voiceCalibrationPID   = "/run/djonehub-alsaucm.pid"
	voiceCalibrationLog   = "/run/djonehub-alsaucm.log"
	voiceCalibrationOK    = "ACDB -> Sent VocProc Cal!"
	voiceRouteRetryWindow = 30 * time.Second
)

// moduleVoiceSession keeps one prepared ADB transport alive across ATD/ATA.
// Some QDC507 firmware stops accepting a fresh ADB CNXN after CLCC becomes
// active, while an already established transport continues to work.
type moduleVoiceSession struct {
	adb      *adbClient
	manifest voiceRuntimeManifest
}

func (a *app) voiceStatus() map[string]any {
	a.moduleVoiceMu.Lock()
	defer a.moduleVoiceMu.Unlock()
	installed, installDetail := upstreamVoiceRuntimeInstalled()
	return map[string]any{
		"preparing": a.moduleVoicePreparing,
		"prepared":  a.moduleVoicePrepared,
		"ready":     a.moduleVoiceReady, "last_attempt": a.moduleVoiceLast,
		"last_error": a.moduleVoiceErr, "detail": a.moduleVoiceDetail,
		"runtime_included": false, "runtime_installed": installed,
		"runtime_source": upstreamVoiceRuntimeSource, "runtime_detail": installDetail,
	}
}

func (a *app) prepareModuleVoiceSession() error {
	a.moduleVoiceOpMu.Lock()
	defer a.moduleVoiceOpMu.Unlock()
	return a.prepareModuleVoiceSessionLocked()
}

func (a *app) prepareModuleVoiceSessionBudgeted(budget time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- a.prepareModuleVoiceSession() }()
	select {
	case err := <-done:
		return err
	case <-time.After(budget):
		return errors.New("模块语音运行时仍在准备中")
	}
}

func (a *app) prepareModuleVoiceSessionLocked() error {
	if session := a.moduleVoiceSession; session != nil && session.adb != nil {
		if _, status, err := session.adb.shellChecked("true", 3*time.Second); err == nil && status == 0 {
			return nil
		}
		a.closeModuleVoiceSessionLocked()
	}
	if installed, detail := upstreamVoiceRuntimeInstalled(); !installed {
		return errors.New(detail)
	}
	manifest := loadVoiceManifest()
	adb, err := a.targetADB()
	if err != nil {
		return err
	}
	if err := prepareModuleVoiceRuntime(adb, manifest); err != nil {
		adb.Close()
		return err
	}
	a.moduleVoiceSession = &moduleVoiceSession{adb: adb, manifest: manifest}
	a.moduleVoiceMu.Lock()
	a.moduleVoicePrepared = true
	a.moduleVoiceErr = ""
	a.moduleVoiceDetail = "模块运行时与持久 ADB session 已准备，等待通话接通"
	a.moduleVoiceMu.Unlock()
	return nil
}

func (a *app) resetModuleVoiceSession() {
	a.moduleVoiceOpMu.Lock()
	a.closeModuleVoiceSessionLocked()
	a.moduleVoiceOpMu.Unlock()
	if a.releaseVoiceRoute != nil {
		a.releaseVoiceRoute()
	}
}

func (a *app) closeModuleVoiceSessionLocked() {
	if a.moduleVoiceSession != nil && a.moduleVoiceSession.adb != nil {
		a.moduleVoiceSession.adb.Close()
	}
	a.moduleVoiceSession = nil
	a.moduleVoiceMu.Lock()
	a.moduleVoicePrepared = false
	a.moduleVoiceReady = false
	a.moduleVoiceLast = time.Time{}
	a.moduleVoiceMu.Unlock()
}

func (a *app) setVoiceStatus(ready bool, err error, detail string) {
	a.moduleVoiceMu.Lock()
	defer a.moduleVoiceMu.Unlock()
	a.moduleVoiceReady = ready
	a.moduleVoiceLast = time.Now()
	if err != nil {
		a.moduleVoiceErr = err.Error()
	} else {
		a.moduleVoiceErr = ""
	}
	if len(detail) > 2000 {
		detail = detail[len(detail)-2000:]
	}
	a.moduleVoiceDetail = detail
}

func (a *app) ensureModuleVoiceRoute() error {
	a.moduleVoiceOpMu.Lock()
	defer a.moduleVoiceOpMu.Unlock()
	a.moduleVoiceMu.Lock()
	if a.moduleVoiceReady {
		a.moduleVoiceMu.Unlock()
		return nil
	}
	if !a.moduleVoiceLast.IsZero() && time.Since(a.moduleVoiceLast) < voiceRouteRetryWindow && a.moduleVoiceErr != "" {
		errText := a.moduleVoiceErr
		a.moduleVoiceMu.Unlock()
		return fmt.Errorf("语音路由暂不可用：%s", errText)
	}
	a.moduleVoiceMu.Unlock()
	if a.authorizeVoiceRoute != nil {
		if err := a.authorizeVoiceRoute(); err != nil {
			a.setVoiceStatus(false, err, "")
			return err
		}
	}
	if err := a.prepareModuleVoiceSessionLocked(); err != nil {
		if a.releaseVoiceRoute != nil {
			a.releaseVoiceRoute()
		}
		a.setVoiceStatus(false, err, "")
		return err
	}
	err := a.startModuleVoiceRouteLocked()
	if err != nil {
		if a.releaseVoiceRoute != nil {
			a.releaseVoiceRoute()
		}
		a.setVoiceStatus(false, err, "")
		return err
	}
	a.setVoiceStatus(true, nil, "模块 D4/UAC 路由正在运行")
	return nil
}

func (a *app) ensureModuleVoiceRouteBudgeted(budget time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- a.ensureModuleVoiceRoute() }()
	select {
	case err := <-done:
		return err
	case <-time.After(budget):
		return errors.New("语音路由仍在准备中")
	}
}

func (a *app) stopModuleVoiceRoute() {
	a.moduleVoiceOpMu.Lock()
	defer a.moduleVoiceOpMu.Unlock()
	defer a.closeModuleVoiceSessionLocked()
	a.moduleVoiceMu.Lock()
	wasReady := a.moduleVoiceReady
	a.moduleVoiceReady = false
	a.moduleVoiceLast = time.Time{}
	a.moduleVoiceMu.Unlock()
	if wasReady {
		if err := a.stopModuleVoiceRouteLocked(); err != nil {
			log.Printf("module voice stop failed: %v", err)
			a.setVoiceStatus(false, err, "")
		}
	}
	if a.releaseVoiceRoute != nil {
		a.releaseVoiceRoute()
	}
}

func (a *app) targetADB() (*adbClient, error) {
	if a.usbLocator == nil {
		return nil, errors.New("当前模块缺少 USB locator，拒绝打开 ADB")
	}
	return openDJIUSBADB(*a.usbLocator)
}

func prepareModuleVoiceRuntime(adb *adbClient, manifest voiceRuntimeManifest) error {
	identity, status, err := adb.shellChecked("id -u; uname -r", 8*time.Second)
	if err != nil {
		return fmt.Errorf("ADB 探测失败: %w", err)
	}
	fields := strings.Fields(identity)
	if status != 0 || len(fields) < 2 || fields[0] != "0" {
		return fmt.Errorf("模块 ADB 没有 root 权限：%s", strings.TrimSpace(identity))
	}
	if !strings.Contains(fields[1], manifest.KernelRelease) {
		return fmt.Errorf("模块内核不匹配：需要 %s，实际 %s", manifest.KernelRelease, fields[1])
	}
	if err := voiceShell(adb, "mkdir -p '"+voiceRemoteDir+"' && chmod 700 '"+voiceRemoteDir+"'", 8*time.Second); err != nil {
		return err
	}
	for _, entry := range manifest.Files {
		data, err := readUpstreamVoiceRuntimeFile(entry.Name)
		if err != nil {
			return err
		}
		if err := adb.push(data, voiceRemoteDir+"/"+entry.Name, 0o100000|entry.Mode, 30*time.Second); err != nil {
			return fmt.Errorf("推送 %s 失败: %w", entry.Name, err)
		}
	}

	ready, err := voiceSoundDevicesReady(adb, manifest)
	if err != nil {
		return err
	}
	if !ready {
		for _, module := range manifest.Modules {
			_, present, _ := adb.shellChecked("grep -q '^"+module.Name+" ' /proc/modules", 5*time.Second)
			if present == 0 {
				continue
			}
			if err := voiceShell(adb, "insmod '"+voiceRemoteDir+"/"+module.File+"'", 20*time.Second); err != nil {
				dmesg, _, _ := adb.shellChecked("dmesg | tail -n 80", 8*time.Second)
				return fmt.Errorf("加载 %s 失败：%s", module.File, strings.TrimSpace(dmesg))
			}
		}
	}
	if ready, err = voiceWaitSoundDevices(adb, manifest); err != nil || !ready {
		return firstSetupError(err, "模块 ALSA 语音设备没有出现")
	}
	if err := voiceEnsureCalibration(adb); err != nil {
		return err
	}
	if err := voiceShell(adb, "test -c /dev/ttyGS0 && test -p /run/voc_svr", 8*time.Second); err != nil {
		return errors.New("模块缺少 ttyGS0 或 voc_svr")
	}
	helperPath := voiceRemoteDir + "/" + manifest.Helper
	if err := voiceShell(adb, "'"+helperPath+"' --check", 15*time.Second); err != nil {
		return fmt.Errorf("模块 PCM 桥自检失败: %w", err)
	}
	return nil
}

func (a *app) startModuleVoiceRouteLocked() error {
	if a.moduleVoiceSession == nil || a.moduleVoiceSession.adb == nil {
		return errors.New("模块语音 ADB session 尚未准备")
	}
	adb := a.moduleVoiceSession.adb
	manifest := a.moduleVoiceSession.manifest
	helperPath := voiceRemoteDir + "/" + manifest.Helper
	if ready, err := voiceRouteIsReady(adb, manifest); err == nil && ready {
		return nil
	}
	launch := "rm -f '" + voiceRoutePIDFile + "' '" + voiceRouteLogFile + "'; " +
		"nohup '" + helperPath + "' --voice-route-session --verbose </dev/null >> '" + voiceRouteLogFile + "' 2>&1 & pid=$!; " +
		"start=$(cut -d ' ' -f 22 \"/proc/$pid/stat\" 2>/dev/null); " +
		"test -n \"$start\" && printf '%s %s\\n' \"$pid\" \"$start\" > '" + voiceRoutePIDFile + "'"
	launchErr := voiceShell(adb, launch, 8*time.Second)
	if launchErr != nil {
		adb.Close()
		a.moduleVoiceSession.adb = nil
		var err error
		for attempt := 0; attempt < 10; attempt++ {
			time.Sleep(300 * time.Millisecond)
			adb, err = a.targetADB()
			if err == nil {
				break
			}
		}
		if err != nil {
			return fmt.Errorf("路由启动后 ADB 重连失败: %w", err)
		}
		a.moduleVoiceSession.adb = adb
	}
	for attempt := 0; attempt < 30; attempt++ {
		ready, checkErr := voiceRouteIsReady(adb, manifest)
		if checkErr == nil && ready {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	detail, _, _ := adb.shellChecked("test ! -f '"+voiceRouteLogFile+"' || tail -n 120 '"+voiceRouteLogFile+"'", 8*time.Second)
	return fmt.Errorf("模块语音路由未进入 RUNNING：%s", strings.TrimSpace(detail))
}

func (a *app) stopModuleVoiceRouteLocked() error {
	if a.moduleVoiceSession == nil || a.moduleVoiceSession.adb == nil {
		return errors.New("模块语音 ADB session 不可用")
	}
	manifest := a.moduleVoiceSession.manifest
	adb := a.moduleVoiceSession.adb
	helperPath := voiceRemoteDir + "/" + manifest.Helper
	stop := "if test -s '" + voiceRoutePIDFile + "'; then " +
		"read pid expected < '" + voiceRoutePIDFile + "' || true; " +
		"start=$(cut -d ' ' -f 22 \"/proc/$pid/stat\" 2>/dev/null); " +
		"argv0=$(tr '\\000' '\\n' < \"/proc/$pid/cmdline\" 2>/dev/null | sed -n '1p'); " +
		"if test \"$start\" = \"$expected\" && test \"$argv0\" = '" + helperPath + "'; then kill -TERM \"$pid\"; fi; fi; " +
		"rm -f '" + voiceRoutePIDFile + "'; " +
		"echo 0 > /sys/class/android_usb/f_audio/audio_enable; " +
		"if test -p /run/voc_svr; then printf 'T\\nT\\nB\\n' > /run/voc_svr; fi; " +
		"test \"$(cat /sys/class/android_usb/f_audio/audio_enable)\" = 0"
	return voiceShell(adb, stop, 10*time.Second)
}

func voiceShell(adb *adbClient, command string, timeout time.Duration) error {
	output, status, err := adb.shellChecked(command, timeout)
	if err != nil {
		return err
	}
	if status != 0 {
		detail := strings.TrimSpace(output)
		if len(detail) > 600 {
			detail = detail[len(detail)-600:]
		}
		if detail == "" {
			detail = fmt.Sprintf("返回状态 %d", status)
		}
		return errors.New(detail)
	}
	return nil
}

func voiceDeviceChecks(manifest voiceRuntimeManifest) string {
	parts := make([]string, 0, len(manifest.RequiredDevices)+1)
	for _, device := range manifest.RequiredDevices {
		parts = append(parts, "test -c '"+device+"'")
	}
	parts = append(parts, "grep -Fq '"+manifest.CardName+"' /proc/asound/cards")
	return strings.Join(parts, " && ")
}

func voiceSoundDevicesReady(adb *adbClient, manifest voiceRuntimeManifest) (bool, error) {
	_, status, err := adb.shellChecked(voiceDeviceChecks(manifest), 8*time.Second)
	return status == 0, err
}

func voiceWaitSoundDevices(adb *adbClient, manifest voiceRuntimeManifest) (bool, error) {
	command := "ready=0; n=0; while test \"$n\" -lt 100; do if " + voiceDeviceChecks(manifest) +
		"; then ready=1; break; fi; sleep 0.2; n=$((n+1)); done; test \"$ready\" -eq 1"
	_, status, err := adb.shellChecked(command, 25*time.Second)
	return status == 0, err
}

func voiceEnsureCalibration(adb *adbClient) error {
	command := "if test -s '" + voiceCalibrationPID + "'; then read pid expected < '" + voiceCalibrationPID + "' || true; " +
		"start=$(cut -d ' ' -f 22 \"/proc/$pid/stat\" 2>/dev/null); test \"$start\" = \"$expected\" || rm -f '" + voiceCalibrationPID + "'; fi; " +
		"if test ! -s '" + voiceCalibrationPID + "'; then rm -f '" + voiceCalibrationLog + "'; " +
		"nohup /usr/bin/alsaucm_test </dev/null >> '" + voiceCalibrationLog + "' 2>&1 & pid=$!; " +
		"start=$(cut -d ' ' -f 22 \"/proc/$pid/stat\"); printf '%s %s\\n' \"$pid\" \"$start\" > '" + voiceCalibrationPID + "'; sleep 1; fi; " +
		"printf 'open snd_soc_msm_9x07_Tomtom_I2S\\nset _verb VoLTE\\nset _enadev Auxpcm Rx\\nset _enadev Auxpcm Tx\\n' > /run/alsaucm_test; " +
		"n=0; while test \"$n\" -lt 300; do grep -Fq '" + voiceCalibrationOK + "' '" + voiceCalibrationLog + "' && exit 0; sleep 0.2; n=$((n+1)); done; exit 1"
	if err := voiceShell(adb, command, 70*time.Second); err != nil {
		detail, _, _ := adb.shellChecked("tail -n 80 '"+voiceCalibrationLog+"'", 8*time.Second)
		// QDC507 can append the completion marker immediately after the wait
		// command reaches its deadline. Treat the authoritative ACDB marker as
		// success even when the polling shell returned a timeout/status error.
		if voiceCalibrationLogSucceeded(detail) {
			return nil
		}
		return fmt.Errorf("VoLTE ACDB 校准未就绪：%s", strings.TrimSpace(detail))
	}
	return nil
}

func voiceCalibrationLogSucceeded(logText string) bool {
	return strings.Contains(logText, voiceCalibrationOK)
}

func voiceRouteIsReady(adb *adbClient, manifest voiceRuntimeManifest) (bool, error) {
	helperPath := voiceRemoteDir + "/" + manifest.Helper
	command := "test -s '" + voiceRoutePIDFile + "' && read pid expected < '" + voiceRoutePIDFile + "' && " +
		"test \"$(cut -d ' ' -f 22 \"/proc/$pid/stat\" 2>/dev/null)\" = \"$expected\" && " +
		"test \"$(tr '\\000' '\\n' < \"/proc/$pid/cmdline\" 2>/dev/null | sed -n '1p')\" = '" + helperPath + "' && " +
		"grep -q 'VoLTE route session active on hw:0,4' '" + voiceRouteLogFile + "' && " +
		"test \"$(cat /sys/class/android_usb/f_audio/audio_enable)\" = 1 && " +
		"grep -q '^state: RUNNING' /proc/asound/card0/pcm4p/sub0/status && " +
		"grep -q '^state: RUNNING' /proc/asound/card0/pcm4c/sub0/status"
	_, status, err := adb.shellChecked(command, 8*time.Second)
	return status == 0, err
}

func (a *app) voiceStatusAPI(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.voiceStatus())
}

func (a *app) voiceProvisionAPI(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Confirm bool `json:"confirm"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !body.Confirm {
		writeError(w, http.StatusBadRequest, "需要确认后才会从固定上游获取模块侧语音运行时")
		return
	}
	if err := provisionUpstreamVoiceRuntime(r.Context()); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	go a.warmModuleVoiceIfReady()
	writeJSON(w, http.StatusOK, a.voiceStatus())
}

func (a *app) voiceStartAPI(w http.ResponseWriter, _ *http.Request) {
	if !a.callStateAllows("active") {
		writeError(w, http.StatusConflict, "语音媒体只能在通话已接通后启动")
		return
	}
	if err := a.ensureModuleVoiceRoute(); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"started": true, "status": a.voiceStatus()})
}

func (a *app) voiceStopAPI(w http.ResponseWriter, _ *http.Request) {
	a.stopModuleVoiceRoute()
	writeJSON(w, http.StatusOK, map[string]any{"stopped": true, "status": a.voiceStatus()})
}
