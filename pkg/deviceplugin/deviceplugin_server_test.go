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
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

func TestDP(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "DP Suite")
}

var _ = Describe("socketPath", func() {
	It("produces paths under the device plugin directory", func() {
		path := socketPath("example.com/my-resource")
		Expect(path).To(HavePrefix(pluginapi.DevicePluginPath))
		Expect(path).To(HaveSuffix(".sock"))
	})

	It("extracts the suffix after the last slash", func() {
		path := socketPath("ovsdpdk.k8snetworkplumbingwg.io/topology-br0")
		Expect(path).To(Equal(pluginapi.DevicePluginPath + "topology-br0.sock"))
	})

	It("preserves dots and dashes in the suffix without collision", func() {
		path1 := socketPath("example.com/foo.bar")
		path2 := socketPath("example.com/foo-bar")
		Expect(path1).To(HaveSuffix("foo.bar.sock"))
		Expect(path2).To(HaveSuffix("foo-bar.sock"))
		Expect(path1).NotTo(Equal(path2))
	})

	It("keeps paths under Unix socket limit for maximum-length suffix", func() {
		// TopologyResource is validated to max 63 chars by the CRD.
		// Path = DevicePluginPath (32) + suffix (63) + ".sock" (5) = 100 bytes max.
		maxSuffix := strings.Repeat("a", 63)
		path := socketPath("ovsdpdk.k8snetworkplumbingwg.io/" + maxSuffix)
		Expect(len(path)).To(BeNumerically("<", 108))
	})

	It("produces consistent paths for the same resource name", func() {
		path1 := socketPath("example.com/resource")
		path2 := socketPath("example.com/resource")
		Expect(path1).To(Equal(path2))
	})
})

var _ = Describe("Server", func() {
	Describe("devices", func() {
		It("returns the correct number of devices", func() {
			srv := newServer("example.com/res", 0, 5)
			Expect(srv.devices()).To(HaveLen(5))
		})

		It("marks all devices as Healthy", func() {
			srv := newServer("example.com/res", 0, 3)
			for _, d := range srv.devices() {
				Expect(d.Health).To(Equal(pluginapi.Healthy))
			}
		})

		It("sets the correct NUMA node on all devices", func() {
			srv := newServer("example.com/res", 2, 4)
			for _, d := range srv.devices() {
				Expect(d.Topology).NotTo(BeNil())
				Expect(d.Topology.Nodes).To(HaveLen(1))
				Expect(d.Topology.Nodes[0].ID).To(Equal(int64(2)))
			}
		})

		It("generates unique device IDs", func() {
			srv := newServer("example.com/res", 0, 10)
			ids := make(map[string]struct{})
			for _, d := range srv.devices() {
				ids[d.ID] = struct{}{}
			}
			Expect(ids).To(HaveLen(10))
		})
	})

	Describe("Allocate", func() {
		It("returns one empty ContainerAllocateResponse per container request", func() {
			srv := newServer("example.com/res", 0, 10)
			req := &pluginapi.AllocateRequest{
				ContainerRequests: []*pluginapi.ContainerAllocateRequest{
					{DevicesIds: []string{"device-0"}},
					{DevicesIds: []string{"device-1"}},
					{DevicesIds: []string{"device-2"}},
				},
			}
			resp, err := srv.Allocate(context.Background(), req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.ContainerResponses).To(HaveLen(3))
			for _, cr := range resp.ContainerResponses {
				Expect(cr.Envs).To(BeEmpty())
				Expect(cr.Mounts).To(BeEmpty())
				Expect(cr.Devices).To(BeEmpty())
				Expect(cr.Annotations).To(BeEmpty())
			}
		})

		It("returns an empty response for zero container requests", func() {
			srv := newServer("example.com/res", 0, 10)
			resp, err := srv.Allocate(context.Background(), &pluginapi.AllocateRequest{})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.ContainerResponses).To(BeEmpty())
		})
	})

	Describe("GetDevicePluginOptions", func() {
		It("returns empty options", func() {
			srv := newServer("example.com/res", 0, 1)
			opts, err := srv.GetDevicePluginOptions(context.Background(), &pluginapi.Empty{})
			Expect(err).NotTo(HaveOccurred())
			Expect(opts.PreStartRequired).To(BeFalse())
			Expect(opts.GetPreferredAllocationAvailable).To(BeFalse())
		})
	})

	Describe("PreStartContainer", func() {
		It("returns an empty response", func() {
			srv := newServer("example.com/res", 0, 1)
			resp, err := srv.PreStartContainer(context.Background(), &pluginapi.PreStartContainerRequest{})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
		})
	})
})
