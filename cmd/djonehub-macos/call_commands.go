package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/iniwex5/vohive/internal/callcontrol"
)

type callCommandError struct{ Code, Message string }

func (e *callCommandError) Error() string     { return e.Message }
func commandError(code, message string) error { return &callCommandError{code, message} }

// callContext and answerCancel are guarded by callMu. They cancel preparatory
// work immediately on replacement, hangup, detach, or a request deadline.
func (a *app) ensureCallIdentityLocked() {
	if a.activeCall == nil {
		return
	}
	if a.activeCall.UUID == "" {
		a.activeCall.UUID = callcontrol.NewCallUUID()
	}
	if a.activeCall.Revision == 0 {
		a.activeCall.Revision = 1
	}
	if a.callContext == nil {
		a.callContext, a.callCancel = context.WithCancel(context.Background())
	}
}

func (a *app) cancelCallWorkLocked() {
	a.remoteCallOwner = nil
	if a.answerCancel != nil {
		a.answerCancel()
		a.answerCancel = nil
	}
	if a.callCancel != nil {
		a.callCancel()
	}
	a.callContext, a.callCancel = nil, nil
}

func (a *app) currentCallIdentity() (callRecord, bool) {
	a.callMu.Lock()
	defer a.callMu.Unlock()
	if a.activeCall == nil {
		return callRecord{}, false
	}
	a.ensureCallIdentityLocked()
	return *a.activeCall, true
}

func (a *app) audioArbiter() *callcontrol.AudioArbiter {
	a.callAudioOnce.Do(func() {
		if a.callAudio == nil {
			a.callAudio = &callcontrol.AudioArbiter{}
		}
	})
	return a.callAudio
}

func (a *app) reserveCallAudio(uuid string) (callcontrol.AudioLease, error) {
	arbiter := a.audioArbiter()
	a.callMu.Lock()
	defer a.callMu.Unlock()
	if a.activeCall == nil || a.activeCall.UUID != uuid {
		return callcontrol.AudioLease{}, commandError("call_ended", "原通话已结束或已被替换")
	}
	lease, err := arbiter.Acquire(callcontrol.AudioOwner{DeviceID: a.deviceID, CallUUID: uuid})
	if err == nil {
		a.callAudioLease = lease
	}
	return lease, err
}

func (a *app) releaseCallAudio(uuid string) {
	arbiter := a.audioArbiter()
	a.callMu.Lock()
	defer a.callMu.Unlock()
	if a.callAudioLease.Owner.CallUUID == uuid {
		arbiter.Release(a.callAudioLease)
		a.callAudioLease = callcontrol.AudioLease{}
	}
}

func (a *app) callMatches(uuid string, states ...string) bool {
	a.callMu.RLock()
	defer a.callMu.RUnlock()
	if a.activeCall == nil || a.activeCall.UUID != uuid {
		return false
	}
	if len(states) == 0 {
		return true
	}
	for _, state := range states {
		if a.activeCall.State == state {
			return true
		}
	}
	return false
}

// TryLock allows a cancelled request to leave the AT queue without later
// obtaining the lock and executing against a newer call.
func lockWithContext(ctx context.Context, mu interface {
	TryLock() bool
	Unlock()
}) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if mu.TryLock() {
			if err := ctx.Err(); err != nil {
				mu.Unlock()
				return err
			}
			return nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (a *app) executeCallCommand(ctx context.Context, command callcontrol.Command) (callcontrol.Result, error) {
	if command.CallUUID == "" || command.RequestID == "" || len(command.CallUUID) > 128 || len(command.RequestID) > 128 {
		return callcontrol.Result{}, commandError("invalid_command", "call_uuid 和 request_id 必填且不能超过 128 字节")
	}
	if command.Actor == "remote" {
		owner, _ := ctx.Value(remoteOwnerContextKey{}).(*remoteCallOwner)
		if owner == nil || owner.app != a || owner.binding.CallUUID != command.CallUUID {
			return callcontrol.Result{}, commandError("remote_unauthorized", "远程命令缺少当前通话授权")
		}
	} else if command.Actor != "local" && command.Actor != "agent" {
		return callcontrol.Result{}, commandError("actor_unavailable", "远程控制尚未接入")
	}
	switch command.Type {
	case "answer", "reject", "hangup":
	case "dtmf":
		if len(command.Digit) != 1 || !strings.Contains("0123456789*#", command.Digit) {
			return callcontrol.Result{}, commandError("invalid_command", "DTMF 仅支持 0-9、* 和 #")
		}
	default:
		return callcontrol.Result{}, commandError("invalid_command", "不支持的通话命令")
	}
	return a.callCommands.Do(ctx, command, func(ctx context.Context) (callcontrol.Result, error) { return a.runBoundCallCommand(ctx, command) })
}

func (a *app) runBoundCallCommand(parent context.Context, command callcontrol.Command) (callcontrol.Result, error) {
	result := callcontrol.Result{CallUUID: command.CallUUID, Type: command.Type}
	agent := a.ensureVoiceAgent().snapshot()
	a.callMu.Lock()
	if a.activeCall == nil || a.activeCall.UUID != command.CallUUID {
		// Only a known ended call permits idempotent hangup; never target the
		// currently active replacement call or invent a successful unknown call.
		ended := false
		for _, call := range a.callHistory {
			if call.UUID == command.CallUUID {
				ended = true
				break
			}
		}
		a.callMu.Unlock()
		if command.Type == "hangup" && ended {
			result.Accepted = true
			return result, nil
		}
		return result, commandError("call_ended", "原通话已结束或已被替换")
	}
	a.ensureCallIdentityLocked()
	call := *a.activeCall
	if call.RecoveryRequired && command.Type != "hangup" && command.Type != "reject" {
		a.callMu.Unlock()
		return result, commandError("recovery_required", "重启前通话归属未确认，请先在本机挂断")
	}
	if command.Actor == "remote" {
		owner, _ := parent.Value(remoteOwnerContextKey{}).(*remoteCallOwner)
		if owner == nil || a.remoteCallOwner != owner || call.Route.Mode != callcontrol.Remote || call.Route.TargetInstallationID != owner.binding.InstallationID {
			a.callMu.Unlock()
			return result, commandError("remote_unauthorized", "远程通话授权已失效")
		}
	} else if command.Type == "answer" && a.remoteCallOwner != nil {
		a.callMu.Unlock()
		return result, commandError("answer_in_progress", "远程接听已获得该通话的接听权")
	}
	if command.ExpectedRevision != 0 && command.ExpectedRevision != call.Revision {
		a.callMu.Unlock()
		return result, commandError("stale_revision", "通话状态已更新，请刷新后重试")
	}
	if command.Actor == "agent" && !callUsesAgent(&call, agent) {
		a.callMu.Unlock()
		return result, commandError("route_mismatch", "该通话不属于 AI 接听")
	}
	if (command.Type == "answer" || command.Type == "reject") && call.State != "incoming" {
		a.callMu.Unlock()
		return result, commandError("not_ringing", "当前没有可操作的单路来电")
	}
	if command.Type == "dtmf" && call.State != "active" {
		a.callMu.Unlock()
		return result, commandError("not_active", "DTMF 只能在已接通的通话中发送")
	}
	if command.Type == "answer" && call.ControlState != "" {
		a.callMu.Unlock()
		return result, commandError("answer_in_progress", "该来电已有接听或结束操作")
	}
	if command.Type == "dtmf" && call.ControlState != "" && call.ControlState != "answered" {
		a.callMu.Unlock()
		return result, commandError("ending_pending", "通话正在结束")
	}
	ctx, cancel := context.WithTimeout(parent, 12*time.Second)
	stop := context.AfterFunc(a.callContext, cancel)
	if command.Type == "answer" {
		a.answerCancel = cancel
		a.answerGeneration++
		a.activeCall.ControlState = "answering"
		a.activeCall.ControlOwner = command.Actor
		a.activeCall.Revision++
	} else if command.Type == "hangup" || command.Type == "reject" {
		if a.answerCancel != nil {
			a.answerCancel()
			a.answerCancel = nil
		}
		a.activeCall.ControlState = "ending_pending"
		a.activeCall.Revision++
	}
	if command.Type != "dtmf" {
		a.controlGeneration++
	}
	gateGeneration := a.controlGeneration
	answerGeneration := a.answerGeneration
	a.callMu.Unlock()
	defer func() { stop(); cancel() }()
	if command.Type == "answer" {
		if command.Actor != "remote" {
			a.cancelRemoteInvitation(call.UUID)
		}
		return a.answerBoundCall(ctx, call, result, answerGeneration)
	}
	sent := false
	defer func() {
		if sent || command.Type == "dtmf" {
			return
		}
		a.callMu.Lock()
		defer a.callMu.Unlock()
		if a.activeCall != nil && a.activeCall.UUID == call.UUID && a.controlGeneration == gateGeneration && a.activeCall.ControlState == "ending_pending" {
			a.activeCall.ControlState = call.ControlState
			if call.ControlState == "answering" {
				a.activeCall.ControlState = ""
			}
			a.activeCall.Revision++
		}
	}()
	if err := lockWithContext(ctx, &a.operationMu); err != nil {
		return result, err
	}
	defer a.operationMu.Unlock()
	if err := a.validateBoundCall(ctx, call, command.Type); err != nil {
		return result, err
	}
	if !a.demo {
		at := "ATH"
		if command.Type == "reject" {
			at = "AT+CHUP"
		}
		if command.Type == "dtmf" {
			at = fmt.Sprintf("AT+VTS=\"%s\"", command.Digit)
		}
		sent = true
		response, err := a.sendCallAT(ctx, at)
		if err == nil && validateCallATResponse(response) != nil && command.Type != "reject" {
			sent = false
			if checkErr := a.validateBoundCall(ctx, call, command.Type); checkErr != nil {
				return result, checkErr
			}
			fallback := "AT+CHUP"
			if command.Type == "dtmf" {
				fallback = fmt.Sprintf("AT+CLDTMF=1,%s", command.Digit)
			}
			sent = true
			response, err = a.sendCallAT(ctx, fallback)
		}
		if err != nil {
			return result, err
		}
		if err := validateCallATResponse(response); err != nil {
			sent = false
			return result, err
		}
		result.Response = response
	} else if command.Type != "dtmf" {
		a.applyCallPoll(nil, time.Now())
	}
	result.Accepted = true
	return result, nil
}

func (a *app) answerBoundCall(ctx context.Context, call callRecord, result callcontrol.Result, generation uint64) (callcontrol.Result, error) {
	sent := false
	defer func() {
		a.callMu.Lock()
		if a.answerGeneration != generation && a.activeCall != nil && a.activeCall.UUID == call.UUID {
			a.callMu.Unlock()
			return
		}
		if !sent && a.callAudioLease.Owner.CallUUID == call.UUID && (a.activeCall == nil || a.activeCall.UUID != call.UUID || a.activeCall.State != "active") {
			a.audioArbiter().Release(a.callAudioLease)
			a.callAudioLease = callcontrol.AudioLease{}
		}
		if a.activeCall != nil && a.activeCall.UUID == call.UUID {
			a.answerCancel = nil
		}
		if a.activeCall != nil && a.activeCall.UUID == call.UUID && a.activeCall.ControlState == "answering" {
			// Once ATA was sent, retain the reservation until CLCC resolves it.
			if sent {
				a.activeCall.ControlState = "answer_pending"
			} else {
				a.activeCall.ControlState = ""
				if a.activeCall.ControlOwner != "remote" {
					a.activeCall.ControlOwner = ""
				}
			}
			a.activeCall.Revision++
			a.answerCancel = nil
		}
		a.callMu.Unlock()
	}()
	if _, err := a.reserveCallAudio(call.UUID); err != nil {
		return result, err
	}
	if a.prepareCallVoice != nil {
		if err := a.prepareCallVoice(ctx); err != nil {
			return result, err
		}
	} else if !a.demo {
		if err := a.prepareModuleVoiceSessionForCall(ctx, call.UUID); err != nil {
			return result, fmt.Errorf("接听前语音运行时准备失败: %w", err)
		}
	}
	if err := lockWithContext(ctx, &a.operationMu); err != nil {
		return result, err
	}
	defer a.operationMu.Unlock()
	if err := a.validateBoundCall(ctx, call, "answer"); err != nil {
		return result, err
	}
	a.callMu.Lock()
	a.lastAnswerAt = time.Now()
	a.callMu.Unlock()
	if a.demo {
		sent = true
		result.MayHaveAnswered = true
		a.setActiveCallState("active", time.Now())
	} else {
		response, attempted, err := a.sendCallATTracked(ctx, "ATA")
		sent, result.MayHaveAnswered = attempted, attempted
		result.Response = response
		if err != nil {
			return result, err
		}
		if err := validateCallATResponse(response); err != nil {
			sent = false
			result.MayHaveAnswered = false
			return result, err
		}
	}
	result.Accepted = true
	return result, nil
}

// Caller owns operationMu so polling cannot replace the application snapshot
// between the final CLCC check and the AT write. AT itself cannot carry a UUID;
// ambiguous multi-call reports are refused rather than guessing at an index.
func (a *app) validateBoundCall(ctx context.Context, call callRecord, action string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !a.callMatches(call.UUID) {
		return commandError("call_ended", "原通话已结束或已被替换")
	}
	if a.demo {
		return nil
	}
	response, err := a.sendCallAT(ctx, "AT+CLCC")
	if err != nil {
		return err
	}
	if err := validateCallATResponse(response); err != nil {
		return err
	}
	calls := parseCLCC(response)
	if len(calls) != 1 {
		return commandError("ambiguous_call", "模块未报告唯一通话，拒绝执行控制命令")
	}
	current := calls[0]
	if current.Index != call.Index || current.Direction != call.Direction || (call.Number != "" && current.Number != "" && call.Number != current.Number) {
		return commandError("call_changed", "模块中的通话已变化")
	}
	if (action == "answer" || action == "reject") && current.State != "incoming" {
		return commandError("not_ringing", "原来电已不在振铃")
	}
	if action == "dtmf" && current.State != "active" {
		return commandError("not_active", "通话未接通")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !a.callMatches(call.UUID) {
		return commandError("call_ended", "原通话已结束或已被替换")
	}
	return nil
}

func (a *app) sendCallAT(ctx context.Context, command string) (string, error) {
	response, _, err := a.sendCallATTracked(ctx, command)
	return response, err
}

// Report whether the transport was invoked. Cancellation while still waiting
// must not be mistaken for an unknown ATA write and hang up a ringing call.
func (a *app) sendCallATTracked(ctx context.Context, command string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	budget := 5 * time.Second
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < budget {
		budget = time.Until(deadline)
	}
	if budget <= 0 {
		return "", false, context.DeadlineExceeded
	}
	var response string
	var err error
	if a.callAT != nil {
		response, err = a.callAT(command, budget)
	} else {
		response, err = a.runATCommand(command, budget)
	}
	if err == nil && validateCallATResponse(response) == nil {
		confirmed := false
		for _, line := range strings.Split(strings.ReplaceAll(response, "\r", "\n"), "\n") {
			if strings.TrimSpace(line) == "OK" {
				confirmed = true
				break
			}
		}
		if !confirmed {
			err = errors.New("模块未返回命令确认，结果未知")
		}
	}
	return response, true, err
}

func writeCallCommandError(w http.ResponseWriter, err error) {
	status, code := http.StatusBadGateway, "modem_error"
	var ce *callCommandError
	switch {
	case errors.As(err, &ce):
		code, status = ce.Code, http.StatusConflict
		if code == "invalid_command" {
			status = http.StatusBadRequest
		}
	case errors.Is(err, callcontrol.ErrAudioBusy):
		status, code = http.StatusConflict, "audio_busy"
	case errors.Is(err, callcontrol.ErrRequestConflict):
		status, code = http.StatusConflict, "request_id_conflict"
	case errors.Is(err, callcontrol.ErrCommandCapacity):
		status, code = http.StatusTooManyRequests, "command_capacity"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		status, code = http.StatusRequestTimeout, "command_cancelled"
	}
	writeJSON(w, status, map[string]string{"code": code, "error": err.Error()})
}

func (a *app) callCommandAPI(w http.ResponseWriter, r *http.Request) {
	var command callcontrol.Command
	if !decodeJSON(w, r, &command) {
		return
	}
	command.CallUUID, command.Actor = r.PathValue("callUUID"), "local"
	if command.ExpectedRevision == 0 {
		writeCallCommandError(w, commandError("invalid_command", "expected_revision 必须为当前通话版本"))
		return
	}
	result, err := a.executeCallCommand(r.Context(), command)
	if err != nil {
		writeCallCommandError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// Legacy endpoints snapshot the call at request arrival. Updated clients send
// the UUID they actually displayed, so delayed UI actions are rejected too.
func (a *app) legacyCallCommand(w http.ResponseWriter, r *http.Request, action string) {
	var command callcontrol.Command
	if r.Body != nil && r.Body != http.NoBody {
		data, err := io.ReadAll(io.LimitReader(r.Body, 4097))
		if err != nil || len(data) > 4096 {
			writeCallCommandError(w, commandError("invalid_command", "请求体无效"))
			return
		}
		if len(strings.TrimSpace(string(data))) != 0 {
			r.Body = io.NopCloser(strings.NewReader(string(data)))
			if !decodeJSON(w, r, &command) {
				return
			}
		}
	}
	if command.CallUUID == "" {
		call, ok := a.currentCallIdentity()
		if !ok {
			writeCallCommandError(w, commandError("call_ended", "当前没有可操作的通话"))
			return
		}
		command.CallUUID = call.UUID
	}
	if command.RequestID == "" {
		command.RequestID = callcontrol.NewCallUUID()
	}
	command.Type, command.Actor = action, "local"
	result, err := a.executeCallCommand(r.Context(), command)
	if err != nil {
		writeCallCommandError(w, err)
		return
	}
	key := map[string]string{"answer": "answered", "reject": "rejected", "hangup": "hung_up", "dtmf": "sent"}[action]
	writeJSON(w, http.StatusOK, map[string]any{key: result.Accepted, "call_uuid": result.CallUUID, "response": result.Response})
}
