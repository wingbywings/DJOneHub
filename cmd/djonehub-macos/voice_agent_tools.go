package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/iniwex5/vohive/internal/voiceagent"
)

func voiceAgentTools() []voiceagent.Tool {
	return []voiceagent.Tool{
		{Name: "get_call_status", Description: "读取当前电话的方向、状态和已脱敏号码。此操作不会改变电话。", Parameters: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)},
		{Name: "send_dtmf", Description: "在已接通电话中发送一个 DTMF 按键。执行前必须由操作员确认。", Parameters: json.RawMessage(`{"type":"object","properties":{"digit":{"type":"string","enum":["0","1","2","3","4","5","6","7","8","9","*","#"]}},"required":["digit"],"additionalProperties":false}`)},
		{Name: "hang_up_call", Description: "挂断当前电话。此操作会直接执行，无需操作员确认。", Parameters: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)},
	}
}

func (a *app) handleVoiceAgentTool(controller *voiceAgentController, session voiceagent.Session, callID string, call *voiceagent.ToolCall) {
	if call == nil {
		return
	}
	if !controller.snapshot().ToolsEnabled {
		_ = session.SubmitToolResult(context.Background(), call.ID, map[string]any{"ok": false, "error": "tools are disabled by the operator"})
		return
	}
	if call.Name == "get_call_status" || call.Name == "hang_up_call" {
		result, err := a.executeVoiceAgentTool(call.Name, call.Arguments)
		a.submitVoiceAgentToolResult(controller, session, callID, call, result, err, "tool.completed")
		return
	}
	summary, err := describeVoiceAgentTool(call)
	if err != nil {
		a.submitVoiceAgentToolResult(controller, session, callID, call, nil, err, "tool.rejected")
		return
	}
	pending := &pendingVoiceAgentTool{ID: call.ID, CallID: callID, Name: call.Name, Arguments: append(json.RawMessage(nil), call.Arguments...), Summary: summary, ExpiresAt: time.Now().Add(45 * time.Second), session: session}
	controller.runtime.mu.Lock()
	controller.runtime.pendingTools[pending.ID] = pending
	controller.runtime.mu.Unlock()
	controller.recordEvent(callID, voiceagent.Event{Type: "tool.pending", Provider: controller.snapshot().ActiveProvider, Text: summary, At: time.Now()})
	go func() {
		timer := time.NewTimer(time.Until(pending.ExpiresAt))
		defer timer.Stop()
		<-timer.C
		controller.runtime.mu.Lock()
		current, ok := controller.runtime.pendingTools[pending.ID]
		if ok && current == pending {
			delete(controller.runtime.pendingTools, pending.ID)
		}
		controller.runtime.mu.Unlock()
		if ok {
			a.submitVoiceAgentToolResult(controller, session, callID, call, nil, fmt.Errorf("operator confirmation timed out"), "tool.expired")
		}
	}()
}

func describeVoiceAgentTool(call *voiceagent.ToolCall) (string, error) {
	switch call.Name {
	case "send_dtmf":
		var arguments struct {
			Digit string `json:"digit"`
		}
		if err := json.Unmarshal(call.Arguments, &arguments); err != nil {
			return "", fmt.Errorf("invalid DTMF arguments: %w", err)
		}
		if len(arguments.Digit) != 1 || !strings.Contains("0123456789*#", arguments.Digit) {
			return "", fmt.Errorf("invalid DTMF digit")
		}
		return "AI 请求发送 DTMF 按键 " + arguments.Digit, nil
	case "hang_up_call":
		return "AI 请求挂断当前电话", nil
	default:
		return "", fmt.Errorf("tool %q is not allowed", call.Name)
	}
}

func (a *app) executeVoiceAgentTool(name string, arguments json.RawMessage) (any, error) {
	switch name {
	case "get_call_status":
		a.callMu.RLock()
		defer a.callMu.RUnlock()
		if a.activeCall == nil {
			return map[string]any{"active": false}, nil
		}
		return map[string]any{"active": true, "direction": a.activeCall.Direction, "state": a.activeCall.State, "number": redactVoiceAgentText(a.activeCall.Number)}, nil
	case "send_dtmf":
		var body struct {
			Digit string `json:"digit"`
		}
		if err := json.Unmarshal(arguments, &body); err != nil {
			return nil, err
		}
		if len(body.Digit) != 1 || !strings.Contains("0123456789*#", body.Digit) {
			return nil, fmt.Errorf("DTMF 仅支持 0-9、* 和 #")
		}
		if !a.callStateAllows("active") {
			return nil, fmt.Errorf("DTMF 只能在已接通的通话中发送")
		}
		if a.demo {
			return map[string]any{"sent": true, "digit": body.Digit}, nil
		}
		a.operationMu.Lock()
		defer a.operationMu.Unlock()
		response, err := a.runATCommand(fmt.Sprintf("AT+VTS=\"%s\"", body.Digit), 3*time.Second)
		if err != nil || validateCallATResponse(response) != nil {
			response, err = a.runATCommand(fmt.Sprintf("AT+CLDTMF=1,%s", body.Digit), 3*time.Second)
		}
		if err != nil {
			return nil, err
		}
		if err := validateCallATResponse(response); err != nil {
			return nil, err
		}
		return map[string]any{"sent": true, "digit": body.Digit}, nil
	case "hang_up_call":
		if !a.hasActiveCall() {
			return nil, fmt.Errorf("当前没有可挂断的通话")
		}
		if a.demo {
			a.applyCallPoll(nil, time.Now())
			return map[string]any{"hung_up": true}, nil
		}
		a.operationMu.Lock()
		defer a.operationMu.Unlock()
		response, err := a.runATCommand("ATH", 5*time.Second)
		if err != nil || validateCallATResponse(response) != nil {
			response, err = a.runATCommand("AT+CHUP", 5*time.Second)
		}
		if err != nil {
			return nil, err
		}
		if err := validateCallATResponse(response); err != nil {
			return nil, err
		}
		return map[string]any{"hung_up": true}, nil
	default:
		return nil, fmt.Errorf("tool %q is not allowed", name)
	}
}

func (a *app) submitVoiceAgentToolResult(controller *voiceAgentController, session voiceagent.Session, callID string, call *voiceagent.ToolCall, result any, err error, eventType voiceagent.EventType) {
	payload := map[string]any{"ok": err == nil}
	if err != nil {
		payload["error"] = err.Error()
	} else {
		payload["result"] = result
	}
	if submitErr := session.SubmitToolResult(context.Background(), call.ID, payload); submitErr != nil {
		controller.setError(submitErr)
	}
	event := voiceagent.Event{Type: eventType, Provider: controller.snapshot().ActiveProvider, ToolCall: call, At: time.Now()}
	if err != nil {
		event.Err = err
	}
	controller.recordEvent(callID, event)
}

func (a *app) voiceAgentPendingTools(w http.ResponseWriter, _ *http.Request) {
	controller := a.ensureVoiceAgent()
	now := time.Now()
	controller.runtime.mu.Lock()
	items := make([]pendingVoiceAgentTool, 0, len(controller.runtime.pendingTools))
	for id, pending := range controller.runtime.pendingTools {
		if pending.ExpiresAt.Before(now) {
			delete(controller.runtime.pendingTools, id)
			continue
		}
		items = append(items, pendingVoiceAgentTool{ID: pending.ID, CallID: pending.CallID, Name: pending.Name, Arguments: pending.Arguments, Summary: pending.Summary, ExpiresAt: pending.ExpiresAt})
	}
	controller.runtime.mu.Unlock()
	sort.Slice(items, func(i, j int) bool { return items[i].ExpiresAt.Before(items[j].ExpiresAt) })
	writeJSON(w, http.StatusOK, map[string]any{"tools": items})
}

func (a *app) voiceAgentToolDecision(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Approve bool `json:"approve"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	controller := a.ensureVoiceAgent()
	id := r.PathValue("toolID")
	controller.runtime.mu.Lock()
	pending := controller.runtime.pendingTools[id]
	delete(controller.runtime.pendingTools, id)
	controller.runtime.mu.Unlock()
	if pending == nil || pending.ExpiresAt.Before(time.Now()) {
		writeError(w, http.StatusNotFound, "pending tool request was not found or has expired")
		return
	}
	call := &voiceagent.ToolCall{ID: pending.ID, Name: pending.Name, Arguments: pending.Arguments}
	if !body.Approve {
		err := fmt.Errorf("operator rejected the tool request")
		a.submitVoiceAgentToolResult(controller, pending.session, pending.CallID, call, nil, err, "tool.rejected")
		writeJSON(w, http.StatusOK, map[string]any{"approved": false})
		return
	}
	result, err := a.executeVoiceAgentTool(pending.Name, pending.Arguments)
	a.submitVoiceAgentToolResult(controller, pending.session, pending.CallID, call, result, err, "tool.completed")
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"approved": true, "result": result})
}
