package autoreg

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

type Manager struct {
	cfg     Config
	cmd     *exec.Cmd
	healthy atomic.Bool
	mu      sync.Mutex
	logger  *slog.Logger
}

func NewManager(cfg Config, logger *slog.Logger) *Manager {
	return &Manager{cfg: cfg, logger: logger}
}

func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	pythonPath := m.cfg.PythonPath
	if pythonPath == "" {
		pythonPath = "python3"
	}
	workDir := m.cfg.WorkDir
	if workDir == "" {
		workDir = "services/autoreg"
	}
	listen := m.cfg.Listen
	if listen == "" {
		listen = "127.0.0.1:9900"
	}

	_, port, _ := splitHostPort(listen)

	m.cmd = exec.CommandContext(ctx, pythonPath, "-m", "uvicorn", "main:app",
		"--host", "127.0.0.1", "--port", port)
	m.cmd.Dir = workDir
	m.cmd.Env = append(m.cmd.Environ(), fmt.Sprintf("AUTOREG_PORT=%s", port))

	if err := m.cmd.Start(); err != nil {
		return fmt.Errorf("start python autoreg: %w", err)
	}

	go m.waitAndRestart(ctx)
	go m.healthLoop(ctx)

	return nil
}

func (m *Manager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd != nil && m.cmd.Process != nil {
		return m.cmd.Process.Kill()
	}
	return nil
}

func (m *Manager) IsHealthy() bool {
	return m.healthy.Load()
}

func (m *Manager) healthLoop(ctx context.Context) {
	listen := m.cfg.Listen
	if listen == "" {
		listen = "127.0.0.1:9900"
	}
	url := fmt.Sprintf("http://%s/api/health", listen)

	// Wait for initial startup
	for i := 0; i < 60; i++ {
		if ctx.Err() != nil {
			return
		}
		if m.checkHealth(url) {
			m.healthy.Store(true)
			m.logger.Info("autoreg python healthy")
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.healthy.Store(m.checkHealth(url))
		}
	}
}

func (m *Manager) checkHealth(url string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

func (m *Manager) waitAndRestart(ctx context.Context) {
	if m.cmd == nil {
		return
	}
	_ = m.cmd.Wait()
	if ctx.Err() != nil {
		return
	}
	m.healthy.Store(false)
	m.logger.Warn("autoreg python exited, restarting in 5s")
	time.Sleep(5 * time.Second)
	if ctx.Err() != nil {
		return
	}
	if err := m.Start(ctx); err != nil {
		m.logger.Error("autoreg restart failed", "err", err)
	}
}

func splitHostPort(addr string) (string, string, error) {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i], addr[i+1:], nil
		}
	}
	return addr, "", fmt.Errorf("no port in %q", addr)
}
