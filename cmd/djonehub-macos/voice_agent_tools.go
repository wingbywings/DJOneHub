package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/iniwex5/vohive/internal/voiceagent"
)

func voiceAgentTools() []voiceagent.Tool {
	return []voiceagent.Tool{
		{Name: "get_call_status", Description: "读取当前电话的方向、状态和已脱敏号码。此操作不会改变电话。", Parameters: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)},
		{Name: "hang_up_call", Description: "安排结束当前电话。对方明确道别、要求结束或任务已完成时，应在当前轮立即调用，不要等待对方再次确认。调用后请说一句简短告别语；系统会等这句语音实际播放完毕后自动挂断，无需操作员确认。", Parameters: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)},
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
	if call.Name == "hang_up_call" {
		a.scheduleVoiceAgentHangup(controller, session, callID, call)
		return
	}
	if call.Name == "get_call_status" {
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

func (a *app) scheduleVoiceAgentHangup(controller *voiceAgentController, session voiceagent.Session, callID string, call *voiceagent.ToolCall) {
	a.callMu.RLock()
	activeCallID := ""
	if a.activeCall != nil && a.activeCall.State == "active" {
		activeCallID = a.activeCall.ID
	}
	a.callMu.RUnlock()
	if activeCallID == "" || activeCallID != callID {
		a.submitVoiceAgentToolResult(controller, session, callID, call, nil, fmt.Errorf("当前没有可挂断的通话"), "tool.rejected")
		return
	}
	copyCall := *call
	copyCall.Arguments = append(json.RawMessage(nil), call.Arguments...)
	controller.runtime.mu.Lock()
	controller.runtime.pendingHangups[callID] = &pendingVoiceAgentHangup{
		CallID: callID, ToolCall: &copyCall, Requested: time.Now(),
	}
	controller.runtime.mu.Unlock()
	a.submitVoiceAgentToolResult(controller, session, callID, call, map[string]any{
		"scheduled": true,
		"message":   "请立即向对方说一句简短的告别语；告别语播放完毕后系统会自动挂断",
	}, nil, "tool.scheduled")
}

// observeVoiceAgentHangupEvent advances a scheduled hang-up only with audio
// produced after the tool call. This prevents the audio.done boundary of the
// tool-calling response itself from hanging up before the farewell response.
func (a *app) observeVoiceAgentHangupEvent(controller *voiceAgentController, callID string, event voiceagent.Event) {
	controller.runtime.mu.Lock()
	defer controller.runtime.mu.Unlock()
	pending := controller.runtime.pendingHangups[callID]
	if pending == nil {
		return
	}
	switch event.Type {
	case voiceagent.EventSpeechStarted:
		pending.SawAudio = false
		pending.Armed = false
	case voiceagent.EventAudio:
		pending.SawAudio = true
	case voiceagent.EventAudioDone:
		if pending.SawAudio {
			pending.Armed = true
		}
	}
}

func (a *app) completeVoiceAgentHangup(controller *voiceAgentController, callID string) {
	controller.runtime.mu.Lock()
	pending := controller.runtime.pendingHangups[callID]
	if pending == nil || !pending.Armed {
		controller.runtime.mu.Unlock()
		return
	}
	delete(controller.runtime.pendingHangups, callID)
	controller.runtime.mu.Unlock()

	result, err := a.executeVoiceAgentTool("hang_up_call", pending.ToolCall.Arguments)
	event := voiceagent.Event{Type: "tool.completed", Provider: controller.snapshot().ActiveProvider, ToolCall: pending.ToolCall, At: time.Now()}
	if err != nil {
		event.Type = "tool.rejected"
		event.Err = err
	} else {
		encoded, _ := json.Marshal(result)
		event.Text = string(encoded)
	}
	controller.recordEvent(callID, event)
}

func (c *voiceAgentController) cancelPendingHangup(callID string) {
	if c.runtime == nil || callID == "" {
		return
	}
	c.runtime.mu.Lock()
	delete(c.runtime.pendingHangups, callID)
	c.runtime.mu.Unlock()
}

func describeVoiceAgentTool(call *voiceagent.ToolCall) (string, error) {
	return "", fmt.Errorf("tool %q is not allowed", call.Name)
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
