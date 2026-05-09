package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/llm-pool/gateway/internal/config"
	"github.com/llm-pool/gateway/internal/store"
)

func (s *Server) handleTokenOptimizerSettings(w http.ResponseWriter, r *http.Request) {
	opt, source, err := s.currentTokenOptimizer(r)
	data := map[string]any{
		"Active": "settings",
		"Title":  "系统设置",
		"Opt":    opt,
		"Source": source,
		"Saved":  r.URL.Query().Get("saved") == "1",
	}
	if err != nil {
		data["Error"] = err.Error()
	}
	s.render(w, r, "settings_token_optimizer.html", data)
}

func (s *Server) handleTokenOptimizerSettingsPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderTokenOptimizerError(w, r, config.NormalizeTokenOptimizer(s.deps.Cfg.TokenOptimizer), err)
		return
	}
	mode, ok := config.NormalizeTokenOptimizerMode(r.FormValue("mode"))
	if !ok {
		s.renderTokenOptimizerError(w, r, config.NormalizeTokenOptimizer(s.deps.Cfg.TokenOptimizer), fmt.Errorf("invalid mode"))
		return
	}
	current := config.NormalizeTokenOptimizer(s.deps.Cfg.TokenOptimizer)
	opt := config.TokenOptimizer{Mode: mode}
	var err error
	if opt.MinToolOutputBytes, err = parsePositiveIntForm(r, "min_tool_output_bytes", current.MinToolOutputBytes); err != nil {
		s.renderTokenOptimizerError(w, r, current, err)
		return
	}
	if opt.MaxOptimizedToolOutputBytes, err = parsePositiveIntForm(r, "max_optimized_tool_output_bytes", current.MaxOptimizedToolOutputBytes); err != nil {
		s.renderTokenOptimizerError(w, r, current, err)
		return
	}
	if opt.HeadLines, err = parsePositiveIntForm(r, "head_lines", current.HeadLines); err != nil {
		s.renderTokenOptimizerError(w, r, current, err)
		return
	}
	if opt.TailLines, err = parsePositiveIntForm(r, "tail_lines", current.TailLines); err != nil {
		s.renderTokenOptimizerError(w, r, current, err)
		return
	}
	if opt.ErrorContextLines, err = parsePositiveIntForm(r, "error_context_lines", current.ErrorContextLines); err != nil {
		s.renderTokenOptimizerError(w, r, current, err)
		return
	}
	opt = config.NormalizeTokenOptimizer(opt)

	payload, err := json.Marshal(opt)
	if err != nil {
		s.renderTokenOptimizerError(w, r, current, err)
		return
	}
	if err := s.deps.Store.SetSetting(r.Context(), store.SettingTokenOptimizer, string(payload)); err != nil {
		s.renderTokenOptimizerError(w, r, current, err)
		return
	}
	s.deps.Cfg.TokenOptimizer = opt
	if s.crud.Audit != nil {
		s.crud.Audit.Log("info", "settings", "", "", "token optimizer updated: "+opt.Mode)
	}
	http.Redirect(w, r, "/settings/token-optimizer?saved=1", http.StatusSeeOther)
}

func (s *Server) currentTokenOptimizer(r *http.Request) (config.TokenOptimizer, string, error) {
	opt := config.NormalizeTokenOptimizer(s.deps.Cfg.TokenOptimizer)
	value, ok, err := s.deps.Store.GetSetting(r.Context(), store.SettingTokenOptimizer)
	if err != nil {
		return opt, "memory", err
	}
	if !ok || strings.TrimSpace(value) == "" {
		return opt, "config.yaml", nil
	}
	var stored config.TokenOptimizer
	if err := json.Unmarshal([]byte(value), &stored); err != nil {
		return opt, "memory", err
	}
	opt = config.NormalizeTokenOptimizer(stored)
	s.deps.Cfg.TokenOptimizer = opt
	return opt, "admin", nil
}

func (s *Server) renderTokenOptimizerError(w http.ResponseWriter, r *http.Request, opt config.TokenOptimizer, err error) {
	s.render(w, r, "settings_token_optimizer.html", map[string]any{
		"Active": "settings",
		"Title":  "系统设置",
		"Opt":    config.NormalizeTokenOptimizer(opt),
		"Source": "memory",
		"Error":  err.Error(),
	})
}

func parsePositiveIntForm(r *http.Request, name string, fallback int) (int, error) {
	raw := strings.TrimSpace(r.FormValue(name))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return n, nil
}
