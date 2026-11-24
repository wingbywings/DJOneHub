package relay

import (
	"net/http"

	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/voippush"
)

func smsRoutes(mux *http.ServeMux, store *identity.Store, sender voippush.SMSSender) {
	slots := make(chan struct{}, 32)
	mux.HandleFunc("POST /v1/push/alerts", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Environment string `json:"environment"`
			Token       string `json:"token"`
		}
		if !decode(w, r, &body) {
			return
		}
		if sender == nil || sender.Environment() != body.Environment {
			respond(w, http.StatusServiceUnavailable, map[string]string{"error": "sms_push_unavailable"})
			return
		}
		err := store.SetAlertPushEndpoint(r.Context(), identity.Bearer(r.Header.Get("Authorization")), body.Environment, body.Token)
		result(w, http.StatusOK, map[string]bool{"registered": body.Token != ""}, err)
	})
	mux.HandleFunc("POST /v1/sms-notifications", func(w http.ResponseWriter, r *http.Request) {
		var sms voippush.SMS
		if !decodeLimit(w, r, &sms, 256<<10) {
			return
		}
		if !sms.Valid() {
			result(w, 0, nil, identity.ErrInvalid)
			return
		}
		token := identity.Bearer(r.Header.Get("Authorization"))
		p, err := store.Authenticate(r.Context(), token)
		if err != nil || p.Kind != "hub" {
			result(w, 0, nil, identity.ErrUnauthorized)
			return
		}
		endpoint, err := store.AlertPushEndpoint(r.Context(), token, sms.InstallationID, sms.ModuleID)
		if err != nil {
			result(w, 0, nil, err)
			return
		}
		if sender == nil || endpoint.Environment != sender.Environment() {
			respond(w, http.StatusServiceUnavailable, map[string]string{"error": "sms_push_unavailable"})
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			result(w, 0, nil, identity.ErrCapacity)
			return
		}
		sms.HubID = p.ID
		delivery, err := sender.SendSMS(r.Context(), endpoint.Token, sms)
		if delivery.InvalidToken {
			_ = store.InvalidatePushEndpoint(r.Context(), endpoint, delivery.InvalidatedAt)
		}
		if err != nil {
			respond(w, http.StatusBadGateway, map[string]string{"error": "sms_push_failed"})
			return
		}
		respond(w, http.StatusOK, map[string]bool{"accepted": true})
	})
}
