/*
 * Copyright 2026 Red Hat, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package deviceplugin

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/fsnotify/fsnotify"
	"k8s.io/klog/v2"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	ovsdpdkdrav1alpha1 "github.com/k8snetworkplumbingwg/dra-driver-ovsdpdk/pkg/api/ovsdpdkdra/v1alpha1"
	"github.com/k8snetworkplumbingwg/dra-driver-ovsdpdk/pkg/consts"
	"github.com/k8snetworkplumbingwg/dra-driver-ovsdpdk/pkg/ovs"
)

// ResourceUpdater is the interface used by the controller to update the
// Device Plugin manager with bridge configuration changes.
type ResourceUpdater interface {
	UpdateResources(ctx context.Context, bridges []ovsdpdkdrav1alpha1.BridgeSpec) error
}

// newServerFunc is the factory used by the Manager to create topology DP servers.
var newServerFunc = func(resourceName string, numaNode, deviceCount int) TopologyDPServer {
	return newServer(resourceName, numaNode, deviceCount)
}

// startKubeletWatcherFunc is the function used to start the kubelet watcher.
// Overridden in tests to avoid filesystem dependencies.
var startKubeletWatcherFunc = (*Manager).startKubeletWatcher

// bridgeName is the name of an OVS bridge.
type bridgeName string

// resourceName is the fully qualified Device Plugin resource name.
type resourceName string

// Manager manages the lifecycle of topology Device Plugin servers.
// Manager reacts to two events:
// - Calls to UpdateResources(), called when bridge information is changed in the API.
// - Updates from OVS informing DPDK interfaces have been created or deleted from existing
// bridges
//
// Multiple bridges can share the same topologyResource. In this case, they must
// all reside on the same NUMA node; otherwise the resource is not advertised.
type Manager struct {
	mutex     sync.Mutex
	topology  map[bridgeName]resourceName
	servers   map[resourceName]TopologyDPServer
	ovsClient ovs.Client
	ctx       context.Context
	log       klog.Logger
}

// NewManager creates a Manager.
func NewManager(ctx context.Context, ovsClient ovs.Client) (*Manager, error) {
	m := &Manager{
		topology:  make(map[bridgeName]resourceName),
		servers:   make(map[resourceName]TopologyDPServer),
		ovsClient: ovsClient,
		ctx:       ctx,
		log:       klog.Background().WithName("dp.Manager"),
	}

	ovsClient.SetInterfaceNotifier(m.OnInterfaceChange)

	if err := startKubeletWatcherFunc(m); err != nil {
		ovsClient.SetInterfaceNotifier(nil)
		return nil, fmt.Errorf("start kubelet watcher: %w", err)
	}
	return m, nil
}

// UpdateResources reconciles the set of running Device Plugin servers against
// the provided bridge list.
func (m *Manager) UpdateResources(ctx context.Context, bridges []ovsdpdkdrav1alpha1.BridgeSpec) error {
	logger := klog.FromContext(ctx).WithName("UpdateResources")

	// Build topology map (bridgeName → resourceName) and group bridges by resource.
	// The user-provided TopologyResource is a suffix; we prepend the driver prefix.
	newTopology := make(map[bridgeName]resourceName)
	resourceBridges := make(map[resourceName][]bridgeName)
	for _, bridge := range bridges {
		if bridge.TopologyResource == "" {
			continue
		}
		resName := resourceName(consts.TopologyResourcePrefix + bridge.TopologyResource)
		brName := bridgeName(bridge.Name)
		if existing, ok := newTopology[brName]; ok {
			if existing != resName {
				return fmt.Errorf("bridge %q has conflicting topology resources: %q and %q",
					bridge.Name, existing, resName)
			}
			// Duplicate with same resource — skip.
			continue
		}
		newTopology[brName] = resName
		resourceBridges[resName] = append(resourceBridges[resName], brName)
	}

	m.mutex.Lock()
	defer m.mutex.Unlock()

	m.topology = newTopology

	// Stop servers for resources no longer needed.
	for resName, srv := range m.servers {
		if _, ok := resourceBridges[resName]; !ok {
			logger.Info("Stopping topology Device Plugin", "resource", resName)
			srv.Stop()
			delete(m.servers, resName)
		}
	}

	// Ensure correct server state for each resource.
	var errs []error
	for resName, brNames := range resourceBridges {
		if err := m.ensureServer(resName, brNames); err != nil {
			errs = append(errs, fmt.Errorf("resource %s: %w", resName, err))
		}
	}
	return errors.Join(errs...)
}

// OnInterfaceChange re-evaluates the Device Plugin server when a DPDK interface
// is added or removed from the bridge.
func (m *Manager) OnInterfaceChange(brName string) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	resName, wanted := m.topology[bridgeName(brName)]
	if !wanted {
		// Bridge not configured for topology; ignore interface changes.
		return
	}

	// Rebuild the list of bridges for this resource.
	var brNames []bridgeName
	for br, res := range m.topology {
		if res == resName {
			brNames = append(brNames, br)
		}
	}

	if err := m.ensureServer(resName, brNames); err != nil {
		m.log.Error(err, "Failed to ensure server on interface change", "resource", resName)
	}
}

// ensureServer ensures the Device Plugin server for the given resource is in the
// correct state based on the NUMA topology of all its bridges.
// All bridges must share the same valid NUMA node; otherwise no server is started.
func (m *Manager) ensureServer(resName resourceName, brNames []bridgeName) error {
	logger := m.log.WithName("ensureServer").WithValues("resource", resName)

	numaNode := m.getConsistentNUMA(logger, brNames)

	// Check if existing server needs to be stopped or updated
	if srv, exists := m.servers[resName]; exists {
		// Keep server if NUMA unchanged
		if srv.GetNUMA() == numaNode {
			return nil
		}
		// Stop server: NUMA invalid or changed
		if numaNode < 0 {
			logger.Info("Stopping topology Device Plugin (NUMA no longer valid)")
		} else {
			logger.Info("NUMA node changed, recreating topology Device Plugin", "numaNode", numaNode)
		}
		srv.Stop()
		delete(m.servers, resName)
	}

	// Don't start new server if NUMA is invalid
	if numaNode < 0 {
		return nil
	}

	// Start a new server
	logger.Info("Starting topology Device Plugin", "numaNode", numaNode, "bridges", brNames)
	srv := newServerFunc(string(resName), numaNode, consts.DefaultTopologyDeviceCount)
	if err := srv.Start(m.ctx); err != nil {
		logger.Error(err, "Failed to start topology Device Plugin")
		return err
	}
	m.servers[resName] = srv
	return nil
}

// getConsistentNUMA retrieves the NUMA node for all bridges and validates they
// all share the same valid NUMA node. Returns -1 if any bridge has invalid NUMA
// or if bridges are on different NUMA nodes.
func (m *Manager) getConsistentNUMA(logger klog.Logger, brNames []bridgeName) int {
	result := -1
	for _, brName := range brNames {
		nodes := m.ovsClient.BridgeNUMANodes(string(brName))

		var numaNode int
		switch {
		case len(nodes) == 0:
			logger.Info("Bridge has no DPDK interfaces yet", "bridge", brName)
			return -1
		case len(nodes) > 1:
			logger.Error(nil, "Bridge uplinks span multiple NUMA nodes", "bridge", brName, "numaNodes", nodes)
			return -1
		case nodes[0] < 0:
			logger.Info("Bridge NUMA affinity unknown", "bridge", brName)
			return -1
		default:
			numaNode = nodes[0]
		}

		if result < 0 {
			result = numaNode
		} else if result != numaNode {
			logger.Error(nil, "Bridges sharing resource are on different NUMA nodes",
				"bridges", brNames, "numaNodes", []int{result, numaNode})
			return -1
		}
	}
	return result
}

// StopAll stops all running Device Plugin servers.
func (m *Manager) StopAll() {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	for resourceName, srv := range m.servers {
		m.log.Info("Stopping topology Device Plugin", "resource", resourceName)
		srv.Stop()
		delete(m.servers, resourceName)
	}
}

// startKubeletWatcher starts a goroutine that watches for kubelet restarts
// and re-registers all running Device Plugin servers when the kubelet socket
// is recreated.
func (m *Manager) startKubeletWatcher() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create fsnotify watcher: %w", err)
	}

	// Watch the directory containing the kubelet socket, not the socket itself.
	// The socket is recreated on kubelet restart, so we need to watch for CREATE events.
	socketDir := filepath.Dir(pluginapi.KubeletSocket)
	socketName := filepath.Base(pluginapi.KubeletSocket)

	if err := watcher.Add(socketDir); err != nil {
		_ = watcher.Close()
		return fmt.Errorf("watch kubelet socket directory %q: %w", socketDir, err)
	}

	m.log.Info("Started watching kubelet socket for restarts", "path", pluginapi.KubeletSocket)

	go func() {
		defer func() {
			if err := watcher.Close(); err != nil {
				m.log.Error(err, "Failed to close kubelet socket watcher")
			}
		}()
		for {
			select {
			case <-m.ctx.Done():
				m.log.Info("Stopping kubelet socket watcher")
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				// Re-register when the kubelet socket is created (kubelet restart)
				if event.Op&fsnotify.Create == fsnotify.Create && filepath.Base(event.Name) == socketName {
					m.log.Info("Kubelet socket recreated, re-registering Device Plugins")
					m.reregisterAll()
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				m.log.Error(err, "Kubelet socket watcher error")
			}
		}
	}()

	return nil
}

// reregisterAll re-registers all running Device Plugin servers with kubelet.
func (m *Manager) reregisterAll() {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	for resourceName, srv := range m.servers {
		if err := srv.Register(m.ctx); err != nil {
			m.log.Error(err, "Failed to re-register Device Plugin after kubelet restart", "resource", resourceName)
		} else {
			m.log.Info("Re-registered Device Plugin after kubelet restart", "resource", resourceName)
		}
	}
}
