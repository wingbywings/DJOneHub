package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const defaultCallPollInterval = 3 * time.Second

type callRecord struct {
	ID        string     `json:"id"`
	Index     int        `json:"index"`
	Direction string     `json:"direction"`
	State     string     `json:"state"`
	Number    string     `json:"number,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	Missed    bool       `json:"missed"`
	AIHandled bool       `json:"ai_handled,omitempty"`
}

type parsedCall struct {
	Index     int
	Direction string
	State     string
	Number    string
}

var clccPattern = regexp.MustCompile(`\+CLCC:\s*(\d+),(\d+),(\d+),(\d+),(\d+)(?:,"([^"]*)",(\d+))?`)

func parseCLCC(response string) []parsedCall {
	matches := clccPattern.FindAllStringSubmatch(response, -1)
	out := make([]parsedCall, 0, len(matches))
	for _, match := range matches {
		// CLCC mode 0 is a voice call; data sessions must not appear as calls.
		if match[4] != "0" {
			continue
		}
		index, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}
		out = append(out, parsedCall{
			Index:     index,
			Direction: mapCallDirection(match[2]),
			State:     mapCallState(match[3]),
			Number:    strings.TrimSpace(match[6]),
		})
	}
	return out
}

func mapCallDirection(raw string) string {
	if raw == "1" {
		return "incoming"
	}
	return "outgoing"
}

func mapCallState(raw string) string {
	switch raw {
	case "0":
		return "active"
	case "1":
		return "held"
	case "2":
		return "dialing"
	case "3":
		return "alerting"
	case "4":
		return "incoming"
	case "5":
		return "waiting"
	default:
		return "unknown"
	}
}

func (a *app) callInterval() time.Duration {
	if a.callPollInterval > 0 {
		return a.callPollInterval
	}
	return defaultCallPollInterval
}

func (a *app) startCallPoller(ctx context.Context) {
	if a.demo {
		return
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if err := a.pollCallOnce(); err != nil {
				log.Printf("call poll failed: %v", err)
			}
			timer.Reset(a.callInterval())
		}
	}
}

func (a *app) pollCallOnce() error {
	if a.demo {
		return nil
	}
	if !a.operationMu.TryRLock() {
		return nil
	}
	defer a.operationMu.RUnlock()
	if a.modem == nil && a.currentUSBDevice() == nil {
		a.setCallPollStatus(fmt.Errorf("DJI USB device is not connected"))
		return nil
	}

	a.callMu.RLock()
	configured := a.callConfigured
	a.callMu.RUnlock()
	if !configured {
		if _, err := a.runATCommand("AT+CLIP=1", 3*time.Second); err != nil {
			a.setCallPollStatus(err)
			return err
		}
		a.callMu.Lock()
		a.callConfigured = true
		a.callMu.Unlock()
	}

	response, err := a.runATCommand("AT+CLCC", 3*time.Second)
	if err != nil {
		a.setCallPollStatus(err)
		return err
	}
	a.applyCallPoll(parseCLCC(response), time.Now())
	a.setCallPollStatus(nil)
	return nil
}

func (a *app) applyCallPoll(calls []parsedCall, now time.Time) {
	var selected *parsedCall
	for i := range calls {
		candidate := &calls[i]
		if selected == nil || callStatePriority(candidate.State) > callStatePriority(selected.State) {
			selected = candidate
		}
	}

	var notify *callRecord
	a.callMu.Lock()
	previousState := ""
	if a.activeCall != nil {
		previousState = a.activeCall.State
	}
	if selected == nil {
		if a.activeCall != nil {
			ended := now
			a.activeCall.EndedAt = &ended
			a.activeCall.UpdatedAt = now
			a.activeCall.Missed = a.activeCall.Direction == "incoming" &&
				(a.activeCall.State == "incoming" || a.activeCall.State == "waiting")
			completed := *a.activeCall
			a.callHistory = append([]callRecord{completed}, a.callHistory...)
			if len(a.callHistory) > 100 {
				a.callHistory = a.callHistory[:100]
			}
			if completed.Missed || completed.AIHandled {
				notify = &completed
			}
			a.activeCall = nil
		}
		a.callMu.Unlock()
		a.syncVoiceAgentPreparation()
		if notify != nil {
			a.forwardCallBark(*notify)
		}
		a.syncVoiceRouteForCall(previousState, "")
		return
	}

	if a.activeCall == nil || a.activeCall.Index != selected.Index || a.activeCall.Direction != selected.Direction {
		a.activeCall = &callRecord{
			ID:        fmt.Sprintf("%d-%d", now.UnixMilli(), selected.Index),
			Index:     selected.Index,
			Direction: selected.Direction,
			State:     selected.State,
			Number:    selected.Number,
			StartedAt: now,
			UpdatedAt: now,
		}
	} else {
		a.activeCall.State = selected.State
		a.activeCall.UpdatedAt = now
		if selected.Number != "" {
			a.activeCall.Number = selected.Number
		}
	}
	a.callMu.Unlock()
	a.syncVoiceAgentPreparation()
	a.syncVoiceRouteForCall(previousState, selected.State)
	a.syncVoiceAgentRecording(previousState, selected.State)
	a.scheduleVoiceAgentAutoAnswer(previousState, selected.State)
}

func (a *app) syncVoiceRouteForCall(previousState, currentState string) {
	if previousState == "active" && currentState != "active" {
		a.resetAudioHostIntent()
		if a.demo || a.usbLocator == nil {
			return
		}
		go a.stopModuleVoiceRoute()
		return
	}
	if a.demo || a.usbLocator == nil {
		return
	}
	if currentState == "active" && previousState != "active" {
		go func() {
			if err := a.ensureModuleVoiceRoute(); err != nil {
				log.Printf("module voice route start failed: %v", err)
			}
		}()
		return
	}
	if previousState == "" && (currentState == "incoming" || currentState == "waiting" || currentState == "dialing" || currentState == "alerting") {
		go func() {
			if err := a.prepareModuleVoiceSessionBudgeted(90 * time.Second); err != nil {
				log.Printf("module voice preflight did not finish before call activation: %v", err)
			}
		}()
	}
}

// syncVoiceAgentRecording marks incoming calls whose media is handled by the
// Voice Agent and enables full-call recording as soon as they become active.
func (a *app) syncVoiceAgentRecording(previousState, currentState string) {
	if previousState == "active" || currentState != "active" || !a.ensureVoiceAgent().snapshot().Enabled {
		return
	}
	a.callMu.Lock()
	if a.activeCall == nil || a.activeCall.State != "active" || a.activeCall.Direction != "incoming" {
		a.callMu.Unlock()
		return
	}
	a.activeCall.AIHandled = true
	a.callMu.Unlock()

	a.audioHostMu.Lock()
	a.audioHost.WantRecording = true
	a.audioHostMu.Unlock()
}

func callStatePriority(state string) int {
	switch state {
	case "incoming", "waiting":
		return 5
	case "active":
		return 4
	case "alerting":
		return 3
	case "dialing":
		return 2
	case "held":
		return 1
	default:
		return 0
	}
}

func (a *app) setCallPollStatus(err error) {
	a.callMu.Lock()
	defer a.callMu.Unlock()
	a.callLastPoll = time.Now()
	if err != nil {
		a.callLastPollError = err.Error()
		return
	}
	a.callLastPollError = ""
}

func (a *app) callStatus(w http.ResponseWriter, _ *http.Request) {
	a.callMu.RLock()
	defer a.callMu.RUnlock()
	var active *callRecord
	if a.activeCall != nil {
		copy := *a.activeCall
		active = &copy
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"active":          active,
		"history":         append([]callRecord(nil), a.callHistory...),
		"polling":         !a.demo,
		"poll_interval_s": int(a.callInterval().Seconds()),
		"last_poll":       a.callLastPoll,
		"last_poll_error": a.callLastPollError,
		"audio_host":      a.audioHostSnapshot(),
	})
}

func (a *app) rejectCall(w http.ResponseWriter, _ *http.Request) {
	if !a.callStateAllows("incoming", "waiting") {
		writeError(w, http.StatusConflict, "当前没有可拒接的来电")
		return
	}
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	if a.demo {
		a.applyCallPoll(nil, time.Now())
		writeJSON(w, http.StatusOK, map[string]bool{"rejected": true})
		return
	}
	response, err := a.runATCommand("AT+CHUP", 5*time.Second)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := validateCallATResponse(response); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rejected": true, "response": response})
}

func (a *app) answerCall(w http.ResponseWriter, _ *http.Request) {
	if !a.callStateAllows("incoming", "waiting") {
		writeError(w, http.StatusConflict, "当前没有可接听的来电")
		return
	}
	response, err := a.answerCurrentCall()
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"answered": true, "response": response})
}

func (a *app) answerCurrentCall() (string, error) {
	if !a.callStateAllows("incoming", "waiting") {
		return "", fmt.Errorf("当前没有可接听的来电")
	}
	a.callMu.Lock()
	if time.Since(a.lastAnswerAt) < 2*time.Second {
		a.callMu.Unlock()
		return "", nil
	}
	a.lastAnswerAt = time.Now()
	a.callMu.Unlock()
	if a.demo {
		a.setActiveCallState("active", time.Now())
		return "", nil
	}
	if err := a.prepareModuleVoiceSessionBudgeted(20 * time.Second); err != nil {
		return "", fmt.Errorf("接听前语音运行时准备失败：%w", err)
	}
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	response, err := a.runATCommand("ATA", 5*time.Second)
	if err != nil {
		return "", err
	}
	if err := validateCallATResponse(response); err != nil {
		return "", err
	}
	return response, nil
}

func (a *app) hangupCall(w http.ResponseWriter, _ *http.Request) {
	if !a.hasActiveCall() {
		writeError(w, http.StatusConflict, "当前没有可挂断的通话")
		return
	}
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	if a.demo {
		a.applyCallPoll(nil, time.Now())
		writeJSON(w, http.StatusOK, map[string]bool{"hung_up": true})
		return
	}
	response, err := a.runATCommand("ATH", 5*time.Second)
	if err != nil || validateCallATResponse(response) != nil {
		response, err = a.runATCommand("AT+CHUP", 5*time.Second)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	if err := validateCallATResponse(response); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hung_up": true, "response": response})
}

func (a *app) dtmfCall(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Digit string `json:"digit"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if len(body.Digit) != 1 || !strings.ContainsRune("0123456789*#", rune(body.Digit[0])) {
		writeError(w, http.StatusBadRequest, "DTMF 仅支持 0-9、* 和 #")
		return
	}
	if !a.callStateAllows("active") {
		writeError(w, http.StatusConflict, "DTMF 只能在已接通的通话中发送")
		return
	}
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	if a.demo {
		writeJSON(w, http.StatusOK, map[string]bool{"sent": true})
		return
	}
	response, err := a.runATCommand(fmt.Sprintf("AT+VTS=\"%s\"", body.Digit), 3*time.Second)
	if err != nil || validateCallATResponse(response) != nil {
		response, err = a.runATCommand(fmt.Sprintf("AT+CLDTMF=1,%s", body.Digit), 3*time.Second)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	if err := validateCallATResponse(response); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"sent": true})
}

func (a *app) dialCall(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Number string `json:"number"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	number := normalizeDialNumber(body.Number)
	if number == "" {
		writeError(w, http.StatusBadRequest, "号码为空或包含非法字符")
		return
	}
	a.callMu.Lock()
	if a.activeCall != nil {
		a.callMu.Unlock()
		writeError(w, http.StatusConflict, "当前模块已有通话")
		return
	}
	if time.Since(a.lastDialAt) < 2*time.Second {
		a.callMu.Unlock()
		writeError(w, http.StatusConflict, "拨号请求过于频繁，请稍后重试")
		return
	}
	a.lastDialAt = time.Now()
	a.callMu.Unlock()

	if a.demo {
		a.applyCallPoll([]parsedCall{{Index: 1, Direction: "outgoing", State: "dialing", Number: number}}, time.Now())
		writeJSON(w, http.StatusOK, map[string]any{"dialing": true, "number": number})
		return
	}
	if err := a.prepareModuleVoiceSessionBudgeted(90 * time.Second); err != nil {
		writeError(w, http.StatusBadGateway, "拨号前语音运行时准备失败："+err.Error())
		return
	}
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	response, err := a.runATCommand("ATD"+number+";", 8*time.Second)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := validateCallATResponse(response); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	a.applyCallPoll([]parsedCall{{Index: 0, Direction: "outgoing", State: "dialing", Number: number}}, time.Now())
	writeJSON(w, http.StatusOK, map[string]any{"dialing": true, "number": number, "response": response})
}

func (a *app) hasActiveCall() bool {
	a.callMu.RLock()
	defer a.callMu.RUnlock()
	return a.activeCall != nil
}

func (a *app) callStateAllows(states ...string) bool {
	a.callMu.RLock()
	defer a.callMu.RUnlock()
	if a.activeCall == nil {
		return false
	}
	for _, state := range states {
		if a.activeCall.State == state {
			return true
		}
	}
	return false
}

func (a *app) setActiveCallState(state string, now time.Time) {
	a.callMu.Lock()
	if a.activeCall == nil {
		a.callMu.Unlock()
		return
	}
	previousState := a.activeCall.State
	a.activeCall.State = state
	a.activeCall.UpdatedAt = now
	a.callMu.Unlock()
	a.syncVoiceAgentRecording(previousState, state)
}

func validateCallATResponse(response string) error {
	if atResponseIsError(response) {
		return fmt.Errorf("模块拒绝通话命令（ERROR）")
	}
	return nil
}

func normalizeDialNumber(raw string) string {
	var normalized strings.Builder
	for _, char := range strings.TrimSpace(raw) {
		switch {
		case char >= '0' && char <= '9':
			normalized.WriteRune(char)
		case char == '+' || char == '*' || char == '#':
			normalized.WriteRune(char)
		case char == ' ' || char == '-' || char == '(' || char == ')':
			// Common display formatting is ignored.
		default:
			return ""
		}
	}
	return normalized.String()
}
