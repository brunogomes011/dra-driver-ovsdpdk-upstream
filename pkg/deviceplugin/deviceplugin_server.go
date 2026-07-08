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
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/klog/v2"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// TopologyDPServer represents a Topology Device Plugin.
type TopologyDPServer interface {
	Start(ctx context.Context) error
	Stop()
	GetNUMA() int
	// Register re-registers the server with kubelet. Used after kubelet restart.
	Register(ctx context.Context) error
}

// Server is a Device Plugin gRPC server that exposes a fixed set of fake
// devices carrying NUMA topology information for a single OVS bridge.
// The NUMA node is immutable after start; to change it, stop and recreate.
type Server struct {
	pluginapi.UnimplementedDevicePluginServer

	resourceName string
	numaNode     int
	deviceCount  int

	socketPath string
	grpcServer *grpc.Server
	log        klog.Logger
}

// newServer creates a Server for the given resource name, NUMA node, and
// device count. The resource name must already be fully qualified.
func newServer(resourceName string, numaNode, deviceCount int) *Server {
	return &Server{
		resourceName: resourceName,
		numaNode:     numaNode,
		deviceCount:  deviceCount,
		socketPath:   socketPath(resourceName),
		log:          klog.Background().WithName("dp.Server").WithValues("resource", resourceName),
	}
}

// socketPath returns the unix socket path for the given resource name.
// It extracts the suffix after the last "/" and uses it directly as the
// filename. The suffix (TopologyResource) is validated by the CRD to be
// max 63 chars, keeping the full path well under the Unix socket limit
// of 108 bytes.
func socketPath(resourceName string) string {
	suffix := resourceName
	if i := strings.LastIndex(resourceName, "/"); i >= 0 {
		suffix = resourceName[i+1:]
	}
	return filepath.Join(pluginapi.DevicePluginPath, suffix+".sock")
}

// Start starts the gRPC server and registers with the kubelet.
func (s *Server) Start(ctx context.Context) error {
	if err := os.Remove(s.socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale socket %q: %w", s.socketPath, err)
	}

	lis, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("listen on %q: %w", s.socketPath, err)
	}

	s.grpcServer = grpc.NewServer()
	pluginapi.RegisterDevicePluginServer(s.grpcServer, s)

	go func() {
		go func() {
			<-ctx.Done()
			s.grpcServer.GracefulStop()
		}()
		if err := s.grpcServer.Serve(lis); err != nil {
			s.log.Error(err, "gRPC server exited")
		}
	}()

	if err := s.Register(ctx); err != nil {
		s.grpcServer.Stop()
		_ = os.Remove(s.socketPath)
		return fmt.Errorf("register with kubelet: %w", err)
	}

	s.log.Info("Device Plugin started", "socket", s.socketPath, "numaNode", s.numaNode)
	return nil
}

// registrationTimeout is the maximum time to wait for kubelet registration.
// This prevents blocking the manager mutex indefinitely if kubelet is unresponsive.
const registrationTimeout = 10 * time.Second

// Register dials the kubelet registration socket and calls Register.
// This is called during Start and can be called again after kubelet restart.
func (s *Server) Register(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, registrationTimeout)
	defer cancel()

	conn, err := grpc.NewClient(
		"unix://"+pluginapi.KubeletSocket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("dial kubelet socket: %w", err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			s.log.V(2).Info("Failed to close kubelet registration connection", "err", err)
		}
	}()

	client := pluginapi.NewRegistrationClient(conn)
	_, err = client.Register(ctx, &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     filepath.Base(s.socketPath),
		ResourceName: s.resourceName,
	})
	return err
}

// Stop stops the gRPC server and removes the socket. It attempts a graceful
// shutdown but forces termination after a timeout to avoid blocking on
// long-lived RPCs like ListAndWatch.
func (s *Server) Stop() {
	if s.grpcServer != nil {
		done := make(chan struct{})
		go func() {
			s.grpcServer.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			s.log.Info("Graceful shutdown timed out, forcing stop")
			s.grpcServer.Stop()
		}
	}
	if err := os.Remove(s.socketPath); err != nil && !os.IsNotExist(err) {
		s.log.Error(err, "Failed to remove socket", "path", s.socketPath)
	}
	s.log.Info("Device Plugin stopped")
}

// GetNUMA returns the NUMA node this server is advertising.
func (s *Server) GetNUMA() int {
	return s.numaNode
}

// devices builds the device list. Each device gets its own TopologyInfo.
func (s *Server) devices() []*pluginapi.Device {
	devs := make([]*pluginapi.Device, s.deviceCount)
	for i := range devs {
		devs[i] = &pluginapi.Device{
			ID:     fmt.Sprintf("device-%d", i),
			Health: pluginapi.Healthy,
			Topology: &pluginapi.TopologyInfo{
				Nodes: []*pluginapi.NUMANode{{ID: int64(s.GetNUMA())}},
			},
		}
	}
	return devs
}

// GetDevicePluginOptions implements DevicePluginServer.
func (s *Server) GetDevicePluginOptions(_ context.Context, _ *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{}, nil
}

// ListAndWatch implements DevicePluginServer. It sends the device list once
// and then blocks until the stream context is done. The device list is static
// for the lifetime of the server; NUMA changes are handled by stop+recreate.
func (s *Server) ListAndWatch(_ *pluginapi.Empty, stream pluginapi.DevicePlugin_ListAndWatchServer) error {
	if err := stream.Send(&pluginapi.ListAndWatchResponse{Devices: s.devices()}); err != nil {
		return err
	}
	<-stream.Context().Done()
	return nil
}

// Allocate implements DevicePluginServer. Returns empty responses — this
// Device Plugin exists only to carry topology information.
func (s *Server) Allocate(_ context.Context, req *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	resp := &pluginapi.AllocateResponse{
		ContainerResponses: make([]*pluginapi.ContainerAllocateResponse, len(req.ContainerRequests)),
	}
	for i := range resp.ContainerResponses {
		resp.ContainerResponses[i] = &pluginapi.ContainerAllocateResponse{}
	}
	return resp, nil
}

// GetPreferredAllocation implements DevicePluginServer.
func (s *Server) GetPreferredAllocation(_ context.Context, _ *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	return &pluginapi.PreferredAllocationResponse{}, nil
}

// PreStartContainer implements DevicePluginServer.
func (s *Server) PreStartContainer(_ context.Context, _ *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return &pluginapi.PreStartContainerResponse{}, nil
}
