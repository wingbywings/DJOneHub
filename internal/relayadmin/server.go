// Package relayadmin serves the tenant-scoped browser management plane. Native
// signaling retains its Origin ban, including when mounted on the same listener.
package relayadmin

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/iniwex5/vohive/internal/relay/identity"
	"github.com/iniwex5/vohive/internal/relaystatus"
	"github.com/iniwex5/vohive/internal/webadmin"
)

//go:embed web/*
var assets embed.FS

type Options struct {
	Auth          *webadmin.Auth
	Identity      *identity.Store
	Directory     string
	RelayOrigin   string
	Build         string
	RuntimeStatus func() relaystatus.Observation
	BasePath      string // default /admin; native integrated service uses /relay-admin
}
type Server struct {
	http.Handler
	audit *auditStore
}

var basePathPattern = regexp.MustCompile(`^/[a-z][a-z0-9-]{0,31}$`)

func NormalizeBasePath(value string) (string, error) {
	if value == "" {
		return "/admin", nil
	}
	value = strings.TrimSuffix(value, "/")
	if !basePathPattern.MatchString(value) || value == "/v1" || value == "/healthz" {
		return "", errors.New("invalid administration base path")
	}
	return value, nil
}

func New(options Options) (*Server, error) {
	base, err := NormalizeBasePath(options.BasePath)
	if err != nil {
		return nil, err
	}
	if options.Auth == nil || options.Identity == nil || !identity.HTTPSOrigin(options.RelayOrigin) {
		return nil, errors.New("invalid Relay administration options")
	}
	audit, err := openAudit(options.Directory)
	if err != nil {
		return nil, err
	}
	s := &Server{audit: audit}
	mux := http.NewServeMux()
	options.Auth.Register(mux, base+"/api/session")
	require := func(method, path string, handler http.HandlerFunc) {
		mux.Handle(method+" "+path, options.Auth.Require(handler))
	}
	require("GET", base+"/api/overview", func(w http.ResponseWriter, r *http.Request) {
		runtime := runtimeObservation(options)
		if runtime.Snapshot != nil {
			copy := *runtime.Snapshot
			copy.OnlineCount = 0
			items, err := options.Identity.AdminIdentities(r.Context(), options.Auth.Tenant())
			if err != nil {
				output(w, nil, err)
				return
			}
			runtime.Apply(items)
			for _, item := range items {
				if item.SignalState == "online" {
					copy.OnlineCount++
				}
			}
			copy.OnlineIdentities = nil
			runtime.Snapshot = &copy
		}
		reply(w, 200, map[string]any{"relay_origin": options.RelayOrigin, "tenant": options.Auth.Tenant(), "build": options.Build, "identity_schema": 3, "runtime": runtime})
	})
	require("GET", base+"/api/requests", func(w http.ResponseWriter, r *http.Request) {
		items, err := options.Identity.AdminEnrollmentRequests(r.Context(), options.Auth.Tenant())
		output(w, map[string]any{"requests": items}, err)
	})
	require("POST", base+"/api/requests/import", func(w http.ResponseWriter, r *http.Request) {
		var body identity.EnrollmentRequest
		if !readLimit(w, r, &body, 16384) {
			return
		}
		if strings.TrimSuffix(body.RelayURL, "/") != strings.TrimSuffix(options.RelayOrigin, "/") {
			reply(w, 400, map[string]string{"error": "enrollment_origin_mismatch"})
			return
		}
		s.operation(w, r, options, "import_request", body.ID, func() (any, error) { return options.Identity.SubmitEnrollmentRequest(r.Context(), body) }, false)
	})
	require("POST", base+"/api/requests/{id}/approve", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Modules []string `json:"modules"`
		}
		if !read(w, r, &body) {
			return
		}
		s.operation(w, r, options, "approve_request", r.PathValue("id"), func() (any, error) {
			err := options.Identity.AdminApproveEnrollment(r.Context(), options.Auth.Tenant(), r.PathValue("id"), body.Modules)
			return map[string]bool{"approved": err == nil}, err
		}, false)
	})
	require("POST", base+"/api/requests/{id}/reject", func(w http.ResponseWriter, r *http.Request) {
		var body struct{}
		if !read(w, r, &body) {
			return
		}
		s.operation(w, r, options, "reject_request", r.PathValue("id"), func() (any, error) {
			err := options.Identity.AdminRejectEnrollment(r.Context(), options.Auth.Tenant(), r.PathValue("id"))
			return map[string]bool{"rejected": err == nil}, err
		}, false)
	})
	require("POST", base+"/api/requests/{id}/authorization", func(w http.ResponseWriter, r *http.Request) {
		var body struct{}
		if !read(w, r, &body) {
			return
		}
		s.operation(w, r, options, "issue_authorization", r.PathValue("id"), func() (any, error) {
			return options.Identity.AdminIssueEnrollment(r.Context(), options.Auth.Tenant(), r.PathValue("id"), options.RelayOrigin)
		}, true)
	})
	require("POST", base+"/api/requests/{id}/delete", func(w http.ResponseWriter, r *http.Request) {
		var body struct{}
		if !read(w, r, &body) {
			return
		}
		s.operation(w, r, options, "delete_enrollment_request", r.PathValue("id"), func() (any, error) {
			count, err := options.Identity.AdminDeleteEnrollment(r.Context(), options.Auth.Tenant(), r.PathValue("id"))
			return map[string]any{"deleted": err == nil, "deleted_authorizations": count}, err
		}, false)
	})
	require("POST", base+"/api/requests/{id}/authorizations/{grant}/delete", func(w http.ResponseWriter, r *http.Request) {
		var body struct{}
		if !read(w, r, &body) {
			return
		}
		s.operation(w, r, options, "delete_enrollment_authorization", r.PathValue("grant"), func() (any, error) {
			err := options.Identity.AdminDeleteEnrollmentAuthorization(r.Context(), options.Auth.Tenant(), r.PathValue("id"), r.PathValue("grant"))
			return map[string]bool{"deleted": err == nil}, err
		}, false)
	})
	require("GET", base+"/api/identities", func(w http.ResponseWriter, r *http.Request) {
		items, err := options.Identity.AdminIdentities(r.Context(), options.Auth.Tenant())
		if err == nil {
			runtimeObservation(options).Apply(items)
		}
		output(w, map[string]any{"identities": items}, err)
	})
	require("POST", base+"/api/identities/{id}/revoke", func(w http.ResponseWriter, r *http.Request) {
		var body struct{}
		if !read(w, r, &body) {
			return
		}
		s.operation(w, r, options, "revoke_identity", r.PathValue("id"), func() (any, error) {
			err := options.Identity.AdminRevokeTenant(r.Context(), options.Auth.Tenant(), r.PathValue("id"))
			return map[string]bool{"revoked": err == nil}, err
		}, false)
	})
	require("POST", base+"/api/identities/{id}/delete", func(w http.ResponseWriter, r *http.Request) {
		var body struct{}
		if !read(w, r, &body) {
			return
		}
		s.operation(w, r, options, "delete_revoked_identity", r.PathValue("id"), func() (any, error) {
			count, err := options.Identity.AdminDeleteRevokedTenant(r.Context(), options.Auth.Tenant(), r.PathValue("id"))
			return map[string]any{"deleted": err == nil, "deleted_count": count}, err
		}, false)
	})
	require("GET", base+"/api/audit", func(w http.ResponseWriter, r *http.Request) {
		items, err := audit.list(r.Context(), options.Auth.Tenant())
		output(w, map[string]any{"events": items}, err)
	})
	require("POST", base+"/api/audit/clear", func(w http.ResponseWriter, r *http.Request) {
		var body struct{}
		if !read(w, r, &body) {
			return
		}
		s.operation(w, r, options, "clear_audit", "audit", func() (any, error) {
			count, err := audit.clearCompleted(r.Context(), options.Auth.Tenant())
			return map[string]any{"cleared_count": count}, err
		}, false)
	})
	content, _ := fs.Sub(assets, "web")
	index, err := fs.ReadFile(content, "index.html")
	if err != nil {
		audit.db.Close()
		return nil, err
	}
	page := strings.Replace(string(index), `content="/admin/"`, `content="`+base+`/"`, 1)
	mux.HandleFunc("GET "+base+"/{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, page)
	})
	mux.Handle("GET "+base+"/", http.StripPrefix(base+"/", http.FileServer(http.FS(content))))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, base+"/", http.StatusSeeOther)
	})
	s.Handler = options.Auth.Guard(mux)
	return s, nil
}
func runtimeObservation(options Options) relaystatus.Observation {
	if options.RuntimeStatus == nil {
		return relaystatus.Observation{State: "unavailable"}
	}
	o := options.RuntimeStatus()
	if o.State == "fresh" && o.Snapshot != nil {
		return relaystatus.Observe(*o.Snapshot, time.Now())
	}
	return o
}
func (s *Server) Close() error { return s.audit.db.Close() }
func (s *Server) operation(w http.ResponseWriter, r *http.Request, options Options, action, id string, run func() (any, error), download bool) {
	auditID, err := s.audit.begin(r.Context(), options.Auth.Username(), options.Auth.Tenant(), action, id)
	if err != nil {
		reply(w, 503, map[string]string{"error": "audit_unavailable_operation_not_started"})
		return
	}
	value, err := run()
	result := "success"
	if err != nil {
		result = "failed"
	}
	// Intent was durable before the operation. If final audit persistence fails,
	// leave it 'pending'; never record success or expose the grant without it.
	if s.audit.finish(context.WithoutCancel(r.Context()), auditID, result) != nil {
		reply(w, 503, map[string]string{"error": "operation_result_uncertain_refresh_required"})
		return
	}
	if err != nil {
		output(w, nil, err)
		return
	}
	if download {
		w.Header().Set("Content-Disposition", `attachment; filename="djonehub-enrollment.json"`)
	}
	reply(w, 200, value)
}
func read(w http.ResponseWriter, r *http.Request, value any) bool {
	return readLimit(w, r, value, 8192)
}
func readLimit(w http.ResponseWriter, r *http.Request, value any, limit int64) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	d.DisallowUnknownFields()
	if d.Decode(value) != nil || d.Decode(new(any)) != io.EOF {
		reply(w, 400, map[string]string{"error": "invalid_admin_request"})
		return false
	}
	return true
}
func output(w http.ResponseWriter, value any, err error) {
	if err == nil {
		reply(w, 200, value)
		return
	}
	status, code := 500, "management_storage_unavailable"
	switch {
	case errors.Is(err, identity.ErrUnauthorized):
		status, code = 403, "management_scope_denied"
	case errors.Is(err, identity.ErrInvalid):
		status, code = 400, "management_state_invalid"
	case errors.Is(err, identity.ErrCapacity):
		status, code = 429, "management_capacity"
	}
	reply(w, status, map[string]string{"error": code})
}
func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
