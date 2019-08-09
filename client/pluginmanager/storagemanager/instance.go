package storagemanager

import (
	"context"
	"sync"
	"time"

	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/storage"
	"github.com/hashicorp/nomad/client/storage/csi"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/plugins/drivers"
)

const (
	csiFingerprintInterval = 30 * time.Second
	csiFingerprintTimeout  = 1 * time.Minute
)

// instanceManagerConfig configures a storage instance manager
type instanceManagerConfig struct {
	// Logger is the logger used by the storage instance manager
	Logger log.Logger

	// Ctx is used to shutdown the storage instance manager
	Ctx context.Context

	// cfg is the configuration required to connect to a CSI Plugin
	Cfg *storage.PluginConfig

	// Loader is the plugin loader
	Loader storage.PluginCatalog

	// updateNodeFromDriver is the callback used to update the node from fingerprinting
	UpdateNodeFromPlugin UpdateNodeStorageInfoFn
}

// instanceManager is used to manage a single driver plugin
type instanceManager struct {
	// name is the name of the plugin in domain name format and should match the
	// name of the plugin in CSI
	name string

	// logger is the logger used by the driver instance manager
	logger log.Logger

	// ctx is used to shutdown the driver manager
	ctx context.Context

	// cancel is used to shutdown management of this storage plugin
	cancel context.CancelFunc

	// cfg is the configuration required to connect to a CSI Plugin
	cfg *storage.PluginConfig

	// sp is the storage plugin being managed
	sp csi.Client

	// pluginLock locks access to the plugin
	pluginLock sync.Mutex

	// shutdownLock is used to serialize attempts to shutdown
	shutdownLock sync.Mutex

	// updateNodeFromPlugin is the callback used to update the node from fingerprinting
	updateNodeFromPlugin UpdateNodeStorageInfoFn

	// firstFingerprintCh is used to trigger that we have successfully
	// fingerprinted once. It also acts as an indicator as to whether the plugin
	// has been detected.
	firstFingerprintCh chan struct{}
	hasFingerprinted   bool

	// lastHealthState is the last known health fingerprinted by the manager
	lastHealthState   drivers.HealthState
	lastHealthStateMu sync.Mutex
}

// newInstanceManager returns a new driver instance manager. It is expected that
// the context passed in the configuration is cancelled in order to shutdown
// launched goroutines.
func newInstanceManager(name string, c *instanceManagerConfig) *instanceManager {
	ctx, cancel := context.WithCancel(c.Ctx)
	i := &instanceManager{
		name:                 name,
		logger:               c.Logger.With("csi_plugin", name),
		ctx:                  ctx,
		cancel:               cancel,
		cfg:                  c.Cfg,
		updateNodeFromPlugin: c.UpdateNodeFromPlugin,
		firstFingerprintCh:   make(chan struct{}),
	}

	go i.run()
	return i
}

// WaitForFirstFingerprint waits until either the plugin fingerprints, the
// passed context is done, or the plugin instance manager is shutdown.
func (i *instanceManager) WaitForFirstFingerprint(ctx context.Context) {
	select {
	case <-i.ctx.Done():
	case <-ctx.Done():
	case <-i.firstFingerprintCh:
	}
}

func (i *instanceManager) run() {
	var wg sync.WaitGroup

	// Start the fingerprinter
	wg.Add(1)
	go func() {
		i.fingerprint()
		wg.Done()
	}()

	// Do a final cleanup
	wg.Wait()
}

func (i *instanceManager) dispenseClient() (csi.Client, error) {
	// TODO: Cache Client
	return csi.NewClient(i.cfg.Address)
}

func (i *instanceManager) fingerprint() {
	t := time.NewTimer(0)
	defer t.Stop()

	for {
		select {
		case <-i.ctx.Done():
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(i.ctx, csiFingerprintTimeout)
			fp, err := i.fingerprintOnce(ctx)
			if err != nil {
				fp = &structs.StoragePluginInfo{
					Healthy:           false,
					HealthDescription: "fingerprint failed",
				}
			}
			cancel()

			i.updateLastHealthState(fp)
			i.updateNodeFromPlugin(i.name, fp)

			t.Reset(csiFingerprintInterval)
		}
	}
}

func (i *instanceManager) updateLastHealthState(fp *structs.StoragePluginInfo) {
	i.lastHealthStateMu.Lock()
	defer i.lastHealthStateMu.Unlock()

	if !fp.Healthy && !i.hasFingerprinted {
		i.lastHealthState = drivers.HealthStateUndetected
		return
	} else if fp.Healthy && !i.hasFingerprinted {
		i.lastHealthState = drivers.HealthStateHealthy
		i.hasFingerprinted = true
		close(i.firstFingerprintCh)

		i.logger.Info("plugin is now available")
		return
	}

	state := drivers.HealthStateUndetected
	if fp.Healthy {
		state = drivers.HealthStateHealthy
	} else {
		state = drivers.HealthStateUnhealthy
	}

	if i.lastHealthState != state {
		i.logger.Info("plugin health state has changed", "previous", i.lastHealthState, "current", state, "description", fp.HealthDescription)
	}

	i.lastHealthState = state
}

func (i *instanceManager) fingerprintOnce(ctx context.Context) (*structs.StoragePluginInfo, error) {
	client, err := i.dispenseClient()
	if err != nil {
		return nil, err
	}

	result := &structs.StoragePluginInfo{}
	result.Attributes = make(map[string]string)

	healthy, err := client.PluginProbe(ctx)
	if err != nil {
		return &structs.StoragePluginInfo{
			Healthy:           false,
			HealthDescription: "probe request returned error",
		}, nil
	}
	result.Healthy = healthy

	pluginName, err := client.PluginGetInfo(ctx)
	if err != nil {
		return &structs.StoragePluginInfo{
			Healthy:           false,
			HealthDescription: "get plugin info request returned error",
		}, nil
	}
	result.Attributes["PluginName"] = pluginName

	nodeInfo, err := client.NodeGetInfo(ctx)
	if err != nil {
		return &structs.StoragePluginInfo{
			Healthy:           false,
			HealthDescription: "get node info request returned error",
		}, nil
	}

	result.NodeID = nodeInfo.NodeID
	result.MaxVolumeCount = nodeInfo.MaxVolumes

	return result, nil
}
