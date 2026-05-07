package config

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
)

// LiveConfig wraps an atomically-swappable config root. Readers call Load()
// for a lock-free snapshot; the watcher goroutine calls Store() after
// validating a new config file.
type LiveConfig struct {
	v atomic.Pointer[Root]
}

func NewLiveConfig(initial *Root) *LiveConfig {
	lc := &LiveConfig{}
	lc.v.Store(initial)
	return lc
}

func (lc *LiveConfig) Load() *Root  { return lc.v.Load() }
func (lc *LiveConfig) Store(r *Root) { lc.v.Store(r) }

// ReloadCallback is called after a successful config reload with the new config.
type ReloadCallback func(cfg *Root)

// Watcher monitors a config file for changes and atomically swaps the live
// config on valid updates. Uses fsnotify + 500ms debounce to coalesce rapid
// writes (editors often write temp file then rename).
type Watcher struct {
	path      string
	live      *LiveConfig
	callbacks []ReloadCallback
	mu        sync.Mutex
	stopCh    chan struct{}
}

func NewWatcher(path string, live *LiveConfig) *Watcher {
	return &Watcher{
		path:   path,
		live:   live,
		stopCh: make(chan struct{}),
	}
}

// OnReload registers a callback invoked after each successful reload.
func (w *Watcher) OnReload(cb ReloadCallback) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.callbacks = append(w.callbacks, cb)
}

// Start begins watching in a background goroutine. Returns immediately.
func (w *Watcher) Start() error {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := fw.Add(w.path); err != nil {
		fw.Close()
		return err
	}
	go w.loop(fw)
	return nil
}

func (w *Watcher) Stop() {
	close(w.stopCh)
}

func (w *Watcher) loop(fw *fsnotify.Watcher) {
	defer fw.Close()
	var debounce *time.Timer
	for {
		select {
		case <-w.stopCh:
			return
		case ev, ok := <-fw.Events:
			if !ok {
				return
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}
			if debounce != nil {
				debounce.Stop()
			}
			debounce = time.AfterFunc(500*time.Millisecond, func() {
				w.reload()
			})
		case err, ok := <-fw.Errors:
			if !ok {
				return
			}
			slog.Warn("config watcher error", "err", err)
		}
	}
}

func (w *Watcher) reload() {
	cfg, err := Load(w.path)
	if err != nil {
		slog.Error("config reload failed, keeping old config", "err", err)
		return
	}
	w.live.Store(cfg)
	slog.Info("config reloaded successfully", "path", w.path)

	w.mu.Lock()
	cbs := append([]ReloadCallback{}, w.callbacks...)
	w.mu.Unlock()
	for _, cb := range cbs {
		cb(cfg)
	}
}
