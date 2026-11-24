package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"strings"
	"time"

	"github.com/iniwex5/vohive/internal/voippush"
)

// The module's saved switch controls notifications even while its phone is
// offline or AI takes priority for calls. Never erase the user's Bark settings.
func (a *app) appNotificationTarget() string {
	config, loadError := a.ensureRemoteCalls().snapshot()
	if loadError == "" && config.Enabled {
		return config.TargetInstallationID
	}
	return ""
}

func (a *app) forwardSMSNotification(message receivedSMS) {
	if strings.HasPrefix(message.Sender, "已发送至 ") {
		return
	}
	if target := a.appNotificationTarget(); target != "" {
		if a.remoteSMSDispatch != nil {
			a.remoteSMSDispatch(message, target)
		}
		return
	}
	a.forwardSMSBark(message)
}

var remoteSMSPushSlots = make(chan struct{}, 32)

func (h *usbDeviceHub) dispatchRemoteSMS(a *app, message receivedSMS, target string) {
	h.mu.RLock()
	manager := h.remoteHub
	h.mu.RUnlock()
	if manager == nil || manager.sms == nil {
		log.Printf("SMS app notification skipped (module %s, reason hub_offline)", a.deviceID)
		return
	}
	select {
	case remoteSMSPushSlots <- struct{}{}:
	default:
		log.Printf("SMS app notification skipped (module %s, reason push_capacity)", a.deviceID)
		return
	}
	digest := sha256.Sum256([]byte(a.deviceID + "\x00" + smsCacheKey(message)))
	sms := voippush.SMS{ID: hex.EncodeToString(digest[:]), ModuleID: a.deviceID,
		InstallationID: target, Sender: message.Sender, Content: message.Content, Timestamp: message.Timestamp}
	go func() {
		defer func() { <-remoteSMSPushSlots }()
		// A queued notification must not follow a changed/disabled destination.
		if a.appNotificationTarget() != target {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := manager.sms(ctx, sms); err != nil {
			log.Printf("SMS app notification failed (module %s)", a.deviceID)
		}
	}()
}

type barkChannelStatus struct {
	barkChannelSettings
	PausedByApp bool `json:"paused_by_app"`
}

func (a *app) barkStatus(settings barkChannelSettings) barkChannelStatus {
	return barkChannelStatus{settings, a.appNotificationTarget() != ""}
}
