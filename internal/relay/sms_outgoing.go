package relay

import (
	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/smsout"
	"net/http"
)

func outgoingSMSRoutes(mux *http.ServeMux, store *identity.Store) {
	mux.HandleFunc("POST /v1/sms/outgoing", func(w http.ResponseWriter, r *http.Request) {
		var request smsout.Request
		if !decodeLimit(w, r, &request, 65536) {
			return
		}
		value, err := store.SubmitSMS(r.Context(), identity.Bearer(r.Header.Get("Authorization")), request)
		result(w, http.StatusOK, value, err)
	})
	mux.HandleFunc("POST /v1/sms/outgoing/claim", func(w http.ResponseWriter, r *http.Request) {
		var body struct{}
		if !decode(w, r, &body) {
			return
		}
		request, err := store.ClaimSMS(r.Context(), identity.Bearer(r.Header.Get("Authorization")))
		result(w, http.StatusOK, map[string]any{"request": request}, err)
	})
	mux.HandleFunc("POST /v1/sms/outgoing/result", func(w http.ResponseWriter, r *http.Request) {
		var body smsout.Result
		if !decode(w, r, &body) {
			return
		}
		err := store.FinishSMS(r.Context(), identity.Bearer(r.Header.Get("Authorization")), body)
		result(w, http.StatusOK, map[string]bool{"updated": err == nil}, err)
	})
}
