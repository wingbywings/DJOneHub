package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const defaultCallPollInterval = 3 * time.Second

const activeCallPollInterval = 750 * time.Millisecond

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
}

type parsedCall struct {
	Index     int
	Direction string
	State     string
	Number    string
}

var clccPattern = regexp.MustCompile(`\+CLCC:\s*(\d+),(\d+),(\d+),(\d+),(\d+)(?:,"([^"]*)",(\d+))?`)

var callNumberPattern = regexp.MustCompile(`^\+?\d{1,32}$`)

type callControlError struct {
	Code       string
	HTTPStatus int
	Message    string
	Detail     string
	Cause      error
}

func (e *callControlError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

func (e *callControlError) Unwrap() error { return e.Cause }

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
	a.callMu.RLock()
	active := a.activeCall != nil
	a.callMu.RUnlock()
	if active {
		return activeCallPollInterval
	}
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
	if a.modem == nil && a.currentUSBDevice() == nil {
		a.setCallPollStatus(fmt.Errorf("DJI USB device is not connected"))
		return nil
	}

	a.callActionMu.Lock()
	defer a.callActionMu.Unlock()

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

	var missed *callRecord
	a.callMu.Lock()
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
			if completed.Missed {
				missed = &completed
			}
			a.activeCall = nil
		}
		a.callMu.Unlock()
		if missed != nil {
			a.forwardMissedCallBark(*missed)
		}
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
		"active":          active,
		"history":         append([]callRecord(nil), a.callHistory...),
		"polling":         !a.demo,
		"poll_interval_s": interval.Seconds(),
		"last_poll":       a.callLastPoll,
		"last_poll_error": a.callLastPollError,
		"capabilities": map[string]any{
			"signaling":    true,
			"audio":        false,
			"audio_status": "pending_hardware_validation",
		},
	})
}

func (a *app) runCallAT(command string, timeout time.Duration) (string, error) {
	if a.callATRunner != nil {
		return a.callATRunner(command, timeout)
	}
	return a.runATCommand(command, timeout)
}

func validateCallNumber(number string) (string, error) {
	number = strings.TrimSpace(number)
	if !callNumberPattern.MatchString(number) {
		return "", &callControlError{
			Code:       "invalid_number",
			HTTPStatus: http.StatusUnprocessableEntity,
			Message:    "号码只能包含数字和一个可选的前导 +，长度为 1 到 32 位",
		}
	}
	return number, nil
}

func validateCallATResponse(response string, err error) error {
	if err != nil {
		return &callControlError{
			Code:       "call_unavailable",
			HTTPStatus: http.StatusServiceUnavailable,
			Message:    "电话控制暂时不可用，请检查模块连接和 AT 通道",
			Cause:      err,
		}
	}
	if callATResponseRejected(response) {
		return &callControlError{
			Code:       "modem_rejected",
			HTTPStatus: http.StatusBadGateway,
			Message:    "模块拒绝了电话操作，请确认语音网络和当前通话状态",
		}
	}
	return nil
}

func callATResponseRejected(response string) bool {
	upper := strings.ToUpper(strings.ReplaceAll(response, "\r\n", "\n"))
	return strings.Contains(upper, "\nERROR\n") || strings.HasSuffix(strings.TrimSpace(upper), "ERROR") ||
		strings.Contains(upper, "+CME ERROR:") || strings.Contains(upper, "+CMS ERROR:")
}

func (a *app) activeCallCopy() *callRecord {
	a.callMu.RLock()
	defer a.callMu.RUnlock()
	if a.activeCall == nil {
		return nil
	}
	copy := *a.activeCall
	return &copy
}

func (a *app) dialCall(number string) (*callRecord, error) {
	number, err := validateCallNumber(number)
	if err != nil {
		return nil, err
	}

	a.callActionMu.Lock()
	defer a.callActionMu.Unlock()
	if active := a.activeCallCopy(); active != nil {
		return nil, &callControlError{
			Code:       "call_state_conflict",
			HTTPStatus: http.StatusConflict,
			Message:    "当前已有通话，结束后才能再次拨号",
		}
	}

	response, runErr := a.runCallAT("ATD"+number+";", 15*time.Second)
	if err := validateCallATResponse(response, runErr); err != nil {
		return nil, err
	}
	a.applyCallPoll([]parsedCall{{
		Index: 1, Direction: "outgoing", State: "dialing", Number: number,
	}}, time.Now())
	return a.activeCallCopy(), nil
}

func (a *app) answerCall() (*callRecord, error) {
	a.callActionMu.Lock()
	defer a.callActionMu.Unlock()
	active := a.activeCallCopy()
	if active == nil || (active.State != "incoming" && active.State != "waiting") {
		return active, &callControlError{
			Code:       "call_state_conflict",
			HTTPStatus: http.StatusConflict,
			Message:    "当前没有可接听的来电",
		}
	}
	response, runErr := a.runCallAT("ATA", 5*time.Second)
	if err := validateCallATResponse(response, runErr); err != nil {
		return active, err
	}
	return a.activeCallCopy(), nil
}

func (a *app) hangupCall() (bool, error) {
	a.callActionMu.Lock()
	defer a.callActionMu.Unlock()
	if a.activeCallCopy() == nil {
		return true, nil
	}
	response, runErr := a.runCallAT("ATH", 5*time.Second)
	if err := validateCallATResponse(response, runErr); err != nil {
		return false, err
	}
	a.applyCallPoll(nil, time.Now())
	return false, nil
}

func writeCallControlError(w http.ResponseWriter, err error) {
	var controlErr *callControlError
	if errors.As(err, &controlErr) {
		body := map[string]string{
			"error": controlErr.Message,
			"code":  controlErr.Code,
		}
		if controlErr.Detail != "" {
			body["detail"] = controlErr.Detail
		}
		writeJSON(w, controlErr.HTTPStatus, body)
		return
	}
	log.Printf("unexpected call control error: %v", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{
		"error": "电话操作失败",
		"code":  "call_internal_error",
	})
}

func (a *app) dialCallHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Number string `json:"number"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	call, err := a.dialCall(body.Number)
	if err != nil {
		writeCallControlError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": true, "status": "dialing", "call": call, "audio": false,
	})
}

func (a *app) answerCallHTTP(w http.ResponseWriter, _ *http.Request) {
	call, err := a.answerCall()
	if err != nil {
		writeCallControlError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": true, "status": "answering", "call": call, "audio": false,
	})
}

func (a *app) hangupCallHTTP(w http.ResponseWriter, _ *http.Request) {
	noop, err := a.hangupCall()
	if err != nil {
		writeCallControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "status": "idle", "noop": noop, "audio": false,
	})
}
