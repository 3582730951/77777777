package admin

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ----- Cache hit metrics -----

func parseWindow(r *http.Request, def time.Duration) time.Duration {
	w := r.URL.Query().Get("window")
	if w == "" {
		return def
	}
	if d, err := time.ParseDuration(w); err == nil && d > 0 && d <= 30*24*time.Hour {
		return d
	}
	return def
}

func (s *Server) handleCacheHitOverall(w http.ResponseWriter, r *http.Request) {
	window := parseWindow(r, 24*time.Hour)
	tenant := r.URL.Query().Get("tenant")
	var stat any
	var err error
	if tenant != "" {
		stat, err = s.deps.Store.CacheHitByTenant(r.Context(), tenant, window)
	} else {
		stat, err = s.deps.Store.CacheHitOverall(r.Context(), window)
	}
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSONStatus(w, 200, stat)
}

func (s *Server) handleCacheHitSeries(w http.ResponseWriter, r *http.Request) {
	window := parseWindow(r, 24*time.Hour)
	tenant := r.URL.Query().Get("tenant")
	buckets := 60
	if b := r.URL.Query().Get("buckets"); b != "" {
		fmt.Sscanf(b, "%d", &buckets)
	}
	pts, err := s.deps.Store.CacheHitSeries(r.Context(), tenant, window, buckets)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSONStatus(w, 200, pts)
}

func (s *Server) handleCacheHitByKey(w http.ResponseWriter, r *http.Request) {
	window := parseWindow(r, 24*time.Hour)
	tenant := r.URL.Query().Get("tenant")
	rows, err := s.deps.Store.CacheHitByAPIKey(r.Context(), tenant, window)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSONStatus(w, 200, rows)
}

// ----- Provider breakdown -----

func (s *Server) handleProviderBreakdown(w http.ResponseWriter, r *http.Request) {
	window := parseWindow(r, 24*time.Hour)
	from := time.Now().Add(-window).Unix()
	rows, err := s.deps.Store.QueryDB().QueryContext(r.Context(),
		`SELECT provider, COUNT(*), COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0)
		 FROM request_samples WHERE at >= ? AND status='ok' GROUP BY provider`, from)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer rows.Close()
	type entry struct {
		Provider     string `json:"provider"`
		Count        int64  `json:"count"`
		InputTokens  int64  `json:"input_tokens"`
		OutputTokens int64  `json:"output_tokens"`
	}
	var out []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.Provider, &e.Count, &e.InputTokens, &e.OutputTokens); err != nil {
			continue
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSONStatus(w, 200, out)
}

// ----- Token trend -----

func (s *Server) handleTokenTrend(w http.ResponseWriter, r *http.Request) {
	window := parseWindow(r, 24*time.Hour)
	buckets := 48
	from := time.Now().Add(-window)
	bucketSec := int64(window.Seconds()) / int64(buckets)
	if bucketSec < 1 {
		bucketSec = 1
	}
	fromUnix := from.Unix()
	rows, err := s.deps.Store.QueryDB().QueryContext(r.Context(),
		`SELECT (at - ?) / ? AS bucket_idx,
		        COALESCE(SUM(input_tokens), 0),
		        COALESCE(SUM(output_tokens), 0)
		 FROM request_samples WHERE at >= ? AND status='ok'
		 GROUP BY bucket_idx ORDER BY bucket_idx`,
		fromUnix, bucketSec, fromUnix)
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	defer rows.Close()
	type point struct {
		Bucket       string `json:"bucket"`
		InputTokens  int64  `json:"input_tokens"`
		OutputTokens int64  `json:"output_tokens"`
	}
	out := make([]point, buckets)
	for i := range out {
		t := from.Add(time.Duration(int64(i)*bucketSec) * time.Second)
		out[i].Bucket = t.Format(time.RFC3339)
	}
	for rows.Next() {
		var idx int
		var inp, outp int64
		if err := rows.Scan(&idx, &inp, &outp); err != nil {
			continue
		}
		if idx < 0 {
			continue
		}
		if idx >= buckets {
			idx = buckets - 1
		}
		out[idx].InputTokens += inp
		out[idx].OutputTokens += outp
	}
	if err := rows.Err(); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSONStatus(w, 200, out)
}

// ----- Backup (encrypted dump) -----

func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	tenants, _ := s.deps.Store.ListDynTenants(r.Context())
	groups, _ := s.deps.Store.ListDynGroups(r.Context(), "")
	accounts, _ := s.deps.Store.ListAccounts(r.Context(), "")
	keys, _ := s.deps.Store.ListAPIKeys(r.Context(), "", "")
	type dump struct {
		ExportedAt time.Time `json:"exported_at"`
		Tenants    any       `json:"tenants"`
		Groups     any       `json:"groups"`
		Accounts   any       `json:"accounts"`
		APIKeys    any       `json:"api_keys"`
		Note       string    `json:"note"`
	}
	body, _ := json.MarshalIndent(dump{
		ExportedAt: time.Now(),
		Tenants:    tenants,
		Groups:     groups,
		Accounts:   accounts,
		APIKeys:    keys,
		Note:       "Account credentials are stored as ciphertext in 'credential_blob'; restoration requires the original POOL_MASTER_KEY.",
	}, "", "  ")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write(body)
	_ = gz.Close()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="llm-pool-backup-`+time.Now().Format("20060102-150405")+`.json.gz"`)
	_, _ = io.Copy(w, &buf)
}

// ----- Cluster config push (master → peer) -----

type clusterPushReq struct {
	PeerName string `json:"peer_name"`
	Items    struct {
		Tenants  bool `json:"tenants"`
		Groups   bool `json:"groups"`
		Accounts bool `json:"accounts"`
		Keys     bool `json:"keys"`
	} `json:"items"`
}

func (s *Server) handleClusterPush(w http.ResponseWriter, r *http.Request) {
	var req clusterPushReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	var peerURL, peerToken string
	for _, p := range s.deps.Cfg.Cluster.Peers {
		if p.Name == req.PeerName {
			peerURL = p.URL
			peerToken = p.Token
			break
		}
	}
	if peerURL == "" {
		errJSON(w, 404, "peer not found")
		return
	}
	payload := map[string]any{}
	if req.Items.Tenants {
		t, _ := s.deps.Store.ListDynTenants(r.Context())
		payload["tenants"] = t
	}
	if req.Items.Groups {
		g, _ := s.deps.Store.ListDynGroups(r.Context(), "")
		payload["groups"] = g
	}
	if req.Items.Accounts {
		a, _ := s.deps.Store.ListAccounts(r.Context(), "")
		payload["accounts"] = a // credentials are NOT included; remote will need separate enrollment
	}
	if req.Items.Keys {
		k, _ := s.deps.Store.ListAPIKeys(r.Context(), "", "")
		payload["keys"] = k
	}
	body, _ := json.Marshal(payload)
	pr, _ := http.NewRequestWithContext(r.Context(), "POST",
		strings.TrimRight(peerURL, "/")+"/api/cluster/config/accept", bytes.NewReader(body))
	pr.Header.Set("X-Pool-Token", peerToken)
	pr.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(pr)
	if err != nil {
		errJSON(w, 502, "peer call failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if s.crud.Audit != nil {
		s.crud.Audit.Log("info", "cluster", "", "", "config pushed to "+req.PeerName+" status="+fmt.Sprint(resp.StatusCode))
	}
	writeJSONStatus(w, 200, map[string]any{
		"peer":   req.PeerName,
		"status": resp.StatusCode,
		"body":   string(respBody),
	})
}
