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

	"github.com/iniwex5/vohive/internal/callcontrol"
)

const defaultCallPollInterval = 3 * time.Second

type callRecord struct {
	RecoveryRequired bool                      `json:"recovery_required,omitempty"`
	Revision         uint64                    `json:"revision"`
	ControlState     string                    `json:"control_state,omitempty"`
	ControlOwner     string                    `json:"control_owner,omitempty"`
	UUID             string                    `json:"call_uuid,omitempty"`
	Route            callcontrol.RouteSnapshot `json:"route"`
	ID               string                    `json:"id"`
	Index            int                       `json:"index"`
	Direction        string                    `json:"direction"`
	State            string                    `json:"state"`
	Number           string                    `json:"number,omitempty"`
	StartedAt        time.Time                 `json:"started_at"`
	UpdatedAt        time.Time                 `json:"updated_at"`
	EndedAt          *time.Time                `json:"ended_at,omitempty"`
	Missed           bool                      `json:"missed"`
	AIHandled        bool                      `json:"ai_handled,omitempty"`
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

// A partial/error CLCC reply is not evidence of idle. In particular, malformed
// +CLCC rows must not disappear through the tolerant display parser.
func validatedCallPoll(response string) ([]parsedCall, error) {
	if validateCallATResponse(response) != nil || !strings.HasSuffix(strings.TrimSpace(response), "OK") {
		return nil, fmt.Errorf("incomplete CLCC response")
	}
	ok := false
	for _, line := range strings.Split(strings.ReplaceAll(response, "\r", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "OK" {
			ok = true
		}
		if !strings.HasPrefix(line, "+CLCC") {
			continue
		}
		match := clccPattern.FindStringSubmatch(line)
		if match == nil || match[0] != line || match[1] == "0" || (match[2] != "0" && match[2] != "1") || mapCallState(match[3]) == "unknown" {
			return nil, fmt.Errorf("invalid CLCC row")
		}
	}
	if !ok {
		return nil, fmt.Errorf("missing CLCC completion")
	}
	return parseCLCC(response), nil
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
	base := defaultCallPollInterval
	if a.callPollInterval > 0 {
		base = a.callPollInterval
	}
	// Snapshot the immutable per-call route before consulting idle settings.
	// Do not hold callMu while loading configuration or acquiring route locks.
	a.callMu.RLock()
	var active *callRecord
	if a.activeCall != nil {
		copied := *a.activeCall
		active = &copied
	}
	a.callMu.RUnlock()
	interval := base
	if active != nil {
		if active.Route.Mode != callcontrol.Remote {
			return base
		}
		interval = time.Second
		if active.State != "active" && active.State != "held" {
			interval = 250 * time.Millisecond
		} else if active.ControlState == "answering" || strings.HasSuffix(active.ControlState, "_pending") {
			interval = 250 * time.Millisecond
		}
	} else {
		a.callRouteMu.Lock()
		remote, loadError := a.ensureRemoteCalls().snapshot()
		agent := a.ensureVoiceAgent().snapshot()
		a.callRouteMu.Unlock()
		if loadError == "" && !agent.Enabled && remote.Enabled {
			interval = time.Second
		}
	}
	// Preserve a faster explicitly configured interval.
	if base < interval {
		return base
	}
	return interval
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
	calls, err := validatedCallPoll(response)
	if err != nil {
		a.setCallPollStatus(err)
		return err
	}
	a.applyObservedCallPoll(calls, time.Now(), true)
	a.setCallPollStatus(nil)
	return nil
}

func (a *app) applyCallPoll(calls []parsedCall, now time.Time) {
	a.applyObservedCallPoll(calls, now, a.demo)
}

func (a *app) applyObservedCallPoll(calls []parsedCall, now time.Time, confirmed bool) {
	var selected *parsedCall
	for i := range calls {
		candidate := &calls[i]
		if selected == nil || callStatePriority(candidate.State) > callStatePriority(selected.State) {
			selected = candidate
		}
	}

	var notify *callRecord
	var finishedAIRecord *callRecord
	a.callRouteMu.Lock()
	agent := a.ensureVoiceAgent().snapshot()
	remote, _ := a.ensureRemoteCalls().snapshot()
	a.callMu.Lock()
	if confirmed && !a.demo {
		a.ensureRemoteJournal().observeStartup(calls)
	}
	previousState := ""
	var previous *callRecord
	if a.activeCall != nil {
		a.ensureCallIdentityLocked()
		copy := *a.activeCall
		previous = &copy
		previousState = a.activeCall.State
	}
	replacement := selected != nil && a.activeCall != nil && ((a.activeCall.Index != selected.Index && !(a.activeCall.Index == 0 && a.activeCall.Direction == "outgoing" && selected.Direction == "outgoing")) ||
		a.activeCall.Direction != selected.Direction ||
		(a.activeCall.Number != "" && selected.Number != "" && a.activeCall.Number != selected.Number) ||
		(a.activeCall.State == "active" && selected.State == "incoming"))
	// A remote outgoing placeholder precedes ATD. An empty CLCC poll during
	// media/voice preparation must not cancel the reserved UUID. The short
	// post-write grace lets the modem publish its new index on a later poll.
	if selected == nil && a.activeCall != nil && a.activeCall.Direction == "outgoing" && a.activeCall.ControlOwner == "remote" && (a.activeCall.ControlState == "dial_preparing" || (a.activeCall.ControlState == "dial_pending" && a.activeCall.Index == 0 && now.Sub(a.activeCall.UpdatedAt) < 5*time.Second)) {
		a.callMu.Unlock()
		a.callRouteMu.Unlock()
		return
	}
	if selected == nil || replacement {
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
			if completed.AIHandled {
				finishedAIRecord = &completed
			}
			a.cancelCallWorkLocked()
			a.activeCall = nil
		}
		if selected == nil {
			if confirmed {
				_ = a.ensureRemoteJournal().confirmedIdle()
			} else if previous != nil {
				a.ensureRemoteJournal().uncertain()
			}
			a.callMu.Unlock()
			a.callRouteMu.Unlock()
			if finishedAIRecord != nil {
				a.ensureVoiceAgent().finishCallRecord(finishedAIRecord.ID, now)
			}
			a.syncVoiceAgentPreparation()
			if notify != nil {
				a.forwardCallBark(*notify)
			}
			a.syncVoiceRouteForCall(previous, nil)
			if previous != nil {
				a.notifyNative()
			}
			return
		}
	}

	if a.activeCall == nil {
		a.cancelCallWorkLocked()
		// Remote answering applies only to incoming calls; preserve local/AI
		// outgoing behavior until the separate remote dialing feature exists.
		if selected.Direction != "incoming" {
			remote.Enabled = false
		}
		recovery := a.ensureRemoteJournal().status().RecoveryRequired
		route := callcontrol.SelectRoute(agent.Enabled, agent.Revision, remote)
		if recovery {
			route = callcontrol.RouteSnapshot{Mode: callcontrol.Local}
		}
		a.activeCall = &callRecord{
			RecoveryRequired: recovery,
			Revision:         1,
			UUID:             callcontrol.NewCallUUID(),
			Route:            route,
			ID:               fmt.Sprintf("%d-%d", now.UnixMilli(), selected.Index),
			Index:            selected.Index,
			Direction:        selected.Direction,
			State:            selected.State,
			Number:           selected.Number,
			StartedAt:        now,
			UpdatedAt:        now,
		}
	} else {
		if a.activeCall.State != selected.State || (selected.Number != "" && a.activeCall.Number != selected.Number) {
			a.activeCall.Revision++
		}
		if a.activeCall.Index != selected.Index {
			a.activeCall.Index = selected.Index
			a.activeCall.Revision++
		}
		a.activeCall.State = selected.State
		a.activeCall.UpdatedAt = now
		if selected.Number != "" {
			a.activeCall.Number = selected.Number
		}
	}
	a.ensureCallIdentityLocked()
	if selected.State == "active" && (a.activeCall.ControlState == "answering" || a.activeCall.ControlState == "answer_pending") {
		a.activeCall.ControlState = "answered"
	}
	current := *a.activeCall
	a.callMu.Unlock()
	a.callRouteMu.Unlock()
	if previous == nil || previous.UUID != current.UUID || previous.State != current.State || previous.ControlState != current.ControlState || previous.RecoveryRequired != current.RecoveryRequired {
		a.notifyNative()
	}
	if finishedAIRecord != nil {
		a.ensureVoiceAgent().finishCallRecord(finishedAIRecord.ID, now)
	}
	if notify != nil {
		a.forwardCallBark(*notify)
	}
	if replacement {
		previousState = ""
	}
	a.syncVoiceAgentPreparation()
	a.syncVoiceRouteForCall(previous, &current)
	a.syncVoiceAgentRecording(previousState, selected.State)
	a.scheduleVoiceAgentAutoAnswer(previousState, selected.State)
	a.dispatchRemoteIncoming()
}

func (a *app) syncVoiceRouteForCall(previous, current *callRecord) {
	changed := previous != nil && (current == nil || current.UUID != previous.UUID)
	if previous != nil && (changed || (previous.State == "active" && current.State != "active")) {
		a.resetAudioHostIntent()
		if a.demo || a.usbLocator == nil {
			if changed {
				a.releaseCallAudio(previous.UUID)
			}
		} else {
			go a.stopModuleVoiceRouteForCall(previous.UUID)
		}
	}
	if current == nil || current.RecoveryRequired || a.demo || a.usbLocator == nil {
		return
	}
	if current.State == "active" && (previous == nil || changed || previous.State != "active") {
		go func() {
			if err := a.ensureModuleVoiceRouteForCall(current.UUID); err != nil {
				log.Printf("module voice route start failed: %v", err)
			}
		}()
		return
	}
	if (previous == nil || changed) && (current.State == "incoming" || current.State == "dialing" || current.State == "alerting") {
		a.callMu.RLock()
		callCtx := a.callContext
		if a.activeCall == nil || a.activeCall.UUID != current.UUID {
			callCtx = nil
		}
		a.callMu.RUnlock()
		if callCtx == nil {
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(callCtx, 90*time.Second)
			defer cancel()
			if !a.callMatches(current.UUID) {
				return
			}
			if err := a.prepareModuleVoiceSessionForCall(ctx, current.UUID); err != nil {
				log.Printf("module voice preflight did not finish before call activation: %v", err)
			}
		}()
	}
}

// syncVoiceAgentRecording marks incoming calls whose media is handled by the
// Voice Agent and enables full-call recording as soon as they become active.
func (a *app) syncVoiceAgentRecording(previousState, currentState string) {
	agent := a.ensureVoiceAgent().snapshot()
	if previousState == "active" || currentState != "active" || !agent.Enabled {
		return
	}
	a.callMu.Lock()
	if !callUsesAgent(a.activeCall, agent) || a.activeCall.State != "active" || a.activeCall.Direction != "incoming" {
		a.callMu.Unlock()
		return
	}
	a.activeCall.AIHandled = true
	call := *a.activeCall
	a.callMu.Unlock()
	a.ensureVoiceAgent().startCallRecord(call, a.ensureVoiceAgent().snapshot().Provider)

	a.audioHostMu.Lock()
	a.audioHost.WantRecording = true
	a.audioHostMu.Unlock()
	a.notifyNative()
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
	interval := a.callInterval()
	a.callMu.RLock()
	defer a.callMu.RUnlock()
	var active *callRecord
	if a.activeCall != nil {
		copy := *a.activeCall
		active = &copy
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"active":           active,
		"history":          append([]callRecord(nil), a.callHistory...),
		"polling":          !a.demo,
		"poll_interval_s":  interval.Seconds(),
		"poll_interval_ms": interval.Milliseconds(),
		"last_poll":        a.callLastPoll,
		"last_poll_error":  a.callLastPollError,
		"audio_host":       a.audioHostSnapshot(),
		"recovery":         a.ensureRemoteJournal().status(),
	})
}

func (a *app) rejectCall(w http.ResponseWriter, r *http.Request) {
	a.legacyCallCommand(w, r, "reject")
}

func (a *app) answerCall(w http.ResponseWriter, r *http.Request) {
	a.legacyCallCommand(w, r, "answer")
}

func (a *app) hangupCall(w http.ResponseWriter, r *http.Request) {
	a.legacyCallCommand(w, r, "hangup")
}

func (a *app) dtmfCall(w http.ResponseWriter, r *http.Request) {
	a.legacyCallCommand(w, r, "dtmf")
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
	if previousState != state {
		a.activeCall.Revision++
	}
	a.activeCall.State = state
	if state == "active" && a.activeCall.ControlState == "answering" {
		a.activeCall.ControlState = "answered"
	}
	a.activeCall.UpdatedAt = now
	a.callMu.Unlock()
	if previousState != state {
		a.notifyNative()
	}
	a.syncVoiceAgentRecording(previousState, state)
}

func validateCallATResponse(response string) error {
	if atResponseIsError("\n" + strings.TrimSpace(response) + "\n") {
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
