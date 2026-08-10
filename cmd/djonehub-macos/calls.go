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
	})
}
