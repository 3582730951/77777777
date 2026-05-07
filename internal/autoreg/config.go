package autoreg

import "time"

type Config struct {
	Enabled                  bool
	PythonPath               string
	WorkDir                  string
	Listen                   string
	SyncInterval             time.Duration
	AutoActivate             bool
	AutoDiscovery            bool
	DiscoveryRefreshInterval time.Duration
	PlatformGroupMapping     map[string]string
}
