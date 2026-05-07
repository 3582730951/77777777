package windsurf

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"golang.org/x/net/http2"
)

// LSManager manages the lifecycle of the Windsurf Language Server binary.
type LSManager struct {
	binaryPath string
	dataDir    string
	port       int
	csrfToken  string

	mu      sync.Mutex
	cmd     *exec.Cmd
	running bool
	h2c     *http.Client
}

func NewLSManager(binaryPath, dataDir string, port int) *LSManager {
	if port == 0 {
		port = 42100
	}
	if binaryPath == "" {
		binaryPath = "/opt/windsurf/language_server_linux_x64"
	}
	if dataDir == "" {
		dataDir = "/opt/windsurf/data"
	}
	m := &LSManager{
		binaryPath: binaryPath,
		dataDir:    dataDir,
		port:       port,
		csrfToken:  "windsurf-api-csrf-fixed-token",
	}
	// h2c client for plaintext HTTP/2 to localhost LS
	m.h2c = &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
			},
		},
		Timeout: 120 * time.Second,
	}
	return m
}

func (m *LSManager) Addr() string {
	return fmt.Sprintf("http://127.0.0.1:%d", m.port)
}

func (m *LSManager) CSRFToken() string { return m.csrfToken }
func (m *LSManager) Client() *http.Client { return m.h2c }

func (m *LSManager) EnsureRunning(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running && m.cmd != nil && m.cmd.Process != nil {
		if m.healthCheck(ctx) == nil {
			return nil
		}
		m.running = false
	}
	return m.startLocked(ctx)
}

func (m *LSManager) startLocked(ctx context.Context) error {
	_ = os.MkdirAll(m.dataDir+"/db", 0o755)
	args := []string{
		"--api_server_url=https://server.codeium.com",
		fmt.Sprintf("--server_port=%d", m.port),
		fmt.Sprintf("--csrf_token=%s", m.csrfToken),
		"--register_user_url=https://api.codeium.com/register_user/",
		fmt.Sprintf("--codeium_dir=%s", m.dataDir),
		fmt.Sprintf("--database_dir=%s/db", m.dataDir),
		"--detect_proxy=false",
	}
	m.cmd = exec.Command(m.binaryPath, args...)
	m.cmd.Stdout = nil
	m.cmd.Stderr = nil
	m.cmd.Env = filterEnv()
	if err := m.cmd.Start(); err != nil {
		return fmt.Errorf("start LS: %w", err)
	}
	// Wait for readiness
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if m.healthCheck(ctx) == nil {
			m.running = true
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	_ = m.cmd.Process.Kill()
	return fmt.Errorf("LS failed to become ready within 15s")
}

func (m *LSManager) healthCheck(ctx context.Context) error {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", m.port), 1*time.Second)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

func (m *LSManager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd != nil && m.cmd.Process != nil {
		_ = m.cmd.Process.Kill()
		_ = m.cmd.Wait()
	}
	m.running = false
}

func filterEnv() []string {
	allow := map[string]bool{
		"HOME": true, "PATH": true, "LANG": true, "LC_ALL": true,
		"TMPDIR": true, "TMP": true, "TEMP": true,
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true,
		"http_proxy": true, "https_proxy": true, "no_proxy": true,
		"SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
	}
	var out []string
	for _, e := range os.Environ() {
		for i := 0; i < len(e); i++ {
			if e[i] == '=' {
				if allow[e[:i]] {
					out = append(out, e)
				}
				break
			}
		}
	}
	return out
}
