package admin

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// Portal-scoped JSON endpoints. Auth is the tenant session cookie (handled by
// the wrapping route group); we read the tenant id from request context and
// hard-pin every query to it so a tenant can never see another tenant's data.

func (s *Server) handlePortalCacheHit(w http.ResponseWriter, r *http.Request) {
	tid := tenantFromCtx(r)
	if tid == "" {
		errJSON(w, 401, "no tenant session")
		return
	}
	window := parseWindow(r, 24*time.Hour)
	stat, err := s.deps.Store.CacheHitByTenant(r.Context(), tid, window)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(stat)
}

func (s *Server) handlePortalCacheHitSeries(w http.ResponseWriter, r *http.Request) {
	tid := tenantFromCtx(r)
	if tid == "" {
		errJSON(w, 401, "no tenant session")
		return
	}
	window := parseWindow(r, 24*time.Hour)
	buckets := 60
	if b := r.URL.Query().Get("buckets"); b != "" {
		if n, err := strconv.Atoi(b); err == nil && n > 0 && n <= 500 {
			buckets = n
		}
	}
	pts, err := s.deps.Store.CacheHitSeries(r.Context(), tid, window, buckets)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(pts)
}

func (s *Server) handlePortalCacheHitByKey(w http.ResponseWriter, r *http.Request) {
	tid := tenantFromCtx(r)
	if tid == "" {
		errJSON(w, 401, "no tenant session")
		return
	}
	window := parseWindow(r, 24*time.Hour)
	rows, err := s.deps.Store.CacheHitByAPIKey(r.Context(), tid, window)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rows)
}

func (s *Server) handlePortalChartRequests(w http.ResponseWriter, r *http.Request) {
	tid := tenantFromCtx(r)
	if tid == "" {
		errJSON(w, 401, "no tenant session")
		return
	}
	window := parseWindow(r, 24*time.Hour)
	buckets := 96
	if b := r.URL.Query().Get("buckets"); b != "" {
		if n, err := strconv.Atoi(b); err == nil && n > 0 && n <= 500 {
			buckets = n
		}
	}
	rollups, err := s.deps.Store.QueryRequestSeries(r.Context(), tid, window, buckets)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rollups)
}
