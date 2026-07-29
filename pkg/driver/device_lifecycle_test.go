/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package driver

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/containerd/nri/pkg/api"
	"github.com/google/go-cmp/cmp"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
)

type fakeDeviceLifecycleProvider struct {
	mu sync.Mutex

	postRequests []cloudprovider.DeviceLifecycleRequest
	preRequests  []cloudprovider.DeviceLifecycleRequest
	postErr      error
	preErr       error
	postFunc     func(context.Context, cloudprovider.DeviceLifecycleRequest) error
	preFunc      func(context.Context, cloudprovider.DeviceLifecycleRequest) error
}

func (p *fakeDeviceLifecycleProvider) PostAttachDevice(ctx context.Context, req cloudprovider.DeviceLifecycleRequest) error {
	p.mu.Lock()
	p.postRequests = append(p.postRequests, req)
	postFunc := p.postFunc
	postErr := p.postErr
	p.mu.Unlock()

	if postFunc != nil {
		return postFunc(ctx, req)
	}
	return postErr
}

func (p *fakeDeviceLifecycleProvider) PreDetachDevice(ctx context.Context, req cloudprovider.DeviceLifecycleRequest) error {
	p.mu.Lock()
	p.preRequests = append(p.preRequests, req)
	preFunc := p.preFunc
	preErr := p.preErr
	p.mu.Unlock()

	if preFunc != nil {
		return preFunc(ctx, req)
	}
	return preErr
}

func (p *fakeDeviceLifecycleProvider) posts() []cloudprovider.DeviceLifecycleRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]cloudprovider.DeviceLifecycleRequest(nil), p.postRequests...)
}

func (p *fakeDeviceLifecycleProvider) detaches() []cloudprovider.DeviceLifecycleRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]cloudprovider.DeviceLifecycleRequest(nil), p.preRequests...)
}

func TestDeviceLifecycleRequest(t *testing.T) {
	networkConfig := apis.NetworkConfig{
		Profile: "supplicant",
		Interface: apis.InterfaceConfig{
			Name:      "eth0",
			Addresses: []string{"192.0.2.10/24"},
		},
	}
	config := DeviceConfig{
		Claim:           types.NamespacedName{Namespace: "default", Name: "rdma-claim"},
		ClaimUID:        types.UID("claim-uid"),
		RuntimeAttached: true,
		NetworkInterfaceConfigInHost: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{Name: "rdma0"},
		},
		NetworkInterfaceConfigInPod: networkConfig,
		RDMADevice:                  RDMAConfig{LinkDev: "mlx5_0"},
		DeviceSnapshot: &resourceapi.Device{
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				apis.AttrMac:        {StringValue: ptr.To("00:11:22:33:44:55")},
				apis.AttrPCIAddress: {StringValue: ptr.To("0000:0c:00.0")},
			},
		},
	}
	pod := &api.PodSandbox{
		Uid:       "pod-uid",
		Name:      "worker-0",
		Namespace: "default",
	}
	np := &NetworkDriver{nodeName: "worker-node-1"}

	got := np.deviceLifecycleRequest(pod, "rdma0", config, "/var/run/netns/test")
	want := cloudprovider.DeviceLifecycleRequest{
		Device: cloudprovider.DeviceIdentifiers{
			Name:       "rdma0",
			MAC:        "00:11:22:33:44:55",
			PCIAddress: "0000:0c:00.0",
		},
		Claim: cloudprovider.ObjectReference{
			Namespace: "default",
			Name:      "rdma-claim",
			UID:       types.UID("claim-uid"),
		},
		Pod: cloudprovider.ObjectReference{
			Namespace: "default",
			Name:      "worker-0",
			UID:       types.UID("pod-uid"),
		},
		NodeName:          "worker-node-1",
		NetworkNamespace:  "/var/run/netns/test",
		HostInterfaceName: "rdma0",
		PodInterfaceName:  "eth0",
		RDMADeviceName:    "mlx5_0",
		Config:            &networkConfig,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("deviceLifecycleRequest mismatch (-want +got):\n%s", diff)
	}
}

func TestLifecycleDevicesIncludesRuntimeAttachments(t *testing.T) {
	podConfig := PodConfig{DeviceConfigs: map[string]DeviceConfig{
		"netdev": {
			NetworkInterfaceConfigInHost: apis.NetworkConfig{
				Interface: apis.InterfaceConfig{Name: "eth0"},
			},
		},
		"rdma-only": {
			RDMADevice: RDMAConfig{LinkDev: "mlx5_0"},
		},
		"empty": {},
	}}

	np := &NetworkDriver{}
	if got := len(np.lifecycleDevices(podConfig)); got != 2 {
		t.Fatalf("exclusive mode lifecycle device count = %d, want 2", got)
	}

	np.rdmaSharedMode = true
	if got := len(np.lifecycleDevices(podConfig)); got != 1 {
		t.Fatalf("shared mode lifecycle device count = %d, want 1", got)
	}
}

func TestPostAttachDevicesUsesOneConcurrentDeadline(t *testing.T) {
	provider := &fakeDeviceLifecycleProvider{
		postFunc: func(ctx context.Context, _ cloudprovider.DeviceLifecycleRequest) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	np := &NetworkDriver{
		nodeName:                "worker-node-1",
		deviceLifecycleProvider: provider,
		deviceLifecycleTimeout:  30 * time.Millisecond,
	}
	pod := &api.PodSandbox{Uid: "pod-uid", Name: "worker-0", Namespace: "default"}
	devices := []lifecycleDevice{
		{name: "device-0"},
		{name: "device-1"},
		{name: "device-2"},
	}

	start := time.Now()
	err := np.postAttachDevices(context.Background(), pod, devices, "/var/run/netns/test")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("postAttachDevices returned nil, want deadline error")
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("postAttachDevices took %v, want one shared deadline", elapsed)
	}
	if got := len(provider.posts()); got != len(devices) {
		t.Fatalf("PostAttachDevice call count = %d, want %d", got, len(devices))
	}
}

func TestSynchronizeReplaysPostAttach(t *testing.T) {
	store := mustNewPodConfigStore()
	config := DeviceConfig{
		Claim:           types.NamespacedName{Namespace: "default", Name: "rdma-claim"},
		ClaimUID:        types.UID("claim-uid"),
		RuntimeAttached: true,
		NetworkInterfaceConfigInHost: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{Name: "rdma0"},
		},
		NetworkInterfaceConfigInPod: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{Name: "eth0"},
		},
	}
	if err := store.SetDeviceConfig("live-pod", "rdma0", config); err != nil {
		t.Fatalf("SetDeviceConfig live pod: %v", err)
	}
	if err := store.SetDeviceConfig("stale-pod", "rdma1", config); err != nil {
		t.Fatalf("SetDeviceConfig stale pod: %v", err)
	}
	preparedConfig := config
	preparedConfig.RuntimeAttached = false
	if err := store.SetDeviceConfig("prepared-pod", "rdma2", preparedConfig); err != nil {
		t.Fatalf("SetDeviceConfig prepared pod: %v", err)
	}

	provider := &fakeDeviceLifecycleProvider{postErr: errors.New("replay failed")}
	np := &NetworkDriver{
		nodeName:                "worker-node-1",
		podConfigStore:          store,
		deviceLifecycleProvider: provider,
		deviceLifecycleTimeout:  time.Second,
	}
	pods := []*api.PodSandbox{
		{
			Uid:       "live-pod",
			Name:      "worker-0",
			Namespace: "default",
			Linux: &api.LinuxPodSandbox{
				Namespaces: []*api.LinuxNamespace{{
					Type: "network",
					Path: "/var/run/netns/live",
				}},
			},
		},
		{
			Uid:       "prepared-pod",
			Name:      "worker-1",
			Namespace: "default",
			Linux: &api.LinuxPodSandbox{
				Namespaces: []*api.LinuxNamespace{{
					Type: "network",
					Path: "/var/run/netns/prepared",
				}},
			},
		},
	}

	if _, err := np.Synchronize(context.Background(), pods, nil); err != nil {
		t.Fatalf("Synchronize returned replay error: %v", err)
	}
	requests := provider.posts()
	if len(requests) != 1 {
		t.Fatalf("PostAttachDevice replay count = %d, want 1", len(requests))
	}
	if requests[0].Pod.UID != types.UID("live-pod") {
		t.Errorf("replayed pod UID = %q, want live-pod", requests[0].Pod.UID)
	}
	if requests[0].NetworkNamespace != "/var/run/netns/live" {
		t.Errorf("replayed network namespace = %q, want /var/run/netns/live", requests[0].NetworkNamespace)
	}
}

func TestStopPodSandboxPreDetachIsBestEffortWithoutNamespace(t *testing.T) {
	store := mustNewPodConfigStore()
	config := DeviceConfig{
		RuntimeAttached: true,
		NetworkInterfaceConfigInHost: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{Name: "rdma0"},
		},
		NetworkInterfaceConfigInPod: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{Name: "eth0"},
		},
	}
	if err := store.SetDeviceConfig("pod-uid", "rdma0", config); err != nil {
		t.Fatalf("SetDeviceConfig: %v", err)
	}

	provider := &fakeDeviceLifecycleProvider{preErr: errors.New("release failed")}
	np := &NetworkDriver{
		podConfigStore:          store,
		eventRecorder:           record.NewFakeRecorder(10),
		deviceLifecycleProvider: provider,
		deviceLifecycleTimeout:  time.Second,
	}
	pod := &api.PodSandbox{Uid: "pod-uid", Name: "worker-0", Namespace: "default"}

	if err := np.StopPodSandbox(context.Background(), pod); err != nil {
		t.Fatalf("StopPodSandbox returned PreDetachDevice error: %v", err)
	}
	requests := provider.detaches()
	if len(requests) != 1 {
		t.Fatalf("PreDetachDevice call count = %d, want 1", len(requests))
	}
	if requests[0].NetworkNamespace != "" {
		t.Errorf("network namespace = %q, want empty", requests[0].NetworkNamespace)
	}
}

func TestRemovePodSandboxReplaysPreDetach(t *testing.T) {
	store := mustNewPodConfigStore()
	config := DeviceConfig{
		RuntimeAttached: true,
		NetworkInterfaceConfigInHost: apis.NetworkConfig{
			Interface: apis.InterfaceConfig{Name: "rdma0"},
		},
	}
	if err := store.SetDeviceConfig("pod-uid", "rdma0", config); err != nil {
		t.Fatalf("SetDeviceConfig: %v", err)
	}
	store.SetPodNetNs("pod-uid", "/var/run/netns/stored")

	provider := &fakeDeviceLifecycleProvider{}
	np := &NetworkDriver{
		podConfigStore:          store,
		deviceLifecycleProvider: provider,
		deviceLifecycleTimeout:  time.Second,
	}
	pod := &api.PodSandbox{Uid: "pod-uid", Name: "worker-0", Namespace: "default"}

	if err := np.RemovePodSandbox(context.Background(), pod); err != nil {
		t.Fatalf("RemovePodSandbox: %v", err)
	}
	requests := provider.detaches()
	if len(requests) != 1 {
		t.Fatalf("PreDetachDevice fallback count = %d, want 1", len(requests))
	}
	if requests[0].NetworkNamespace != "/var/run/netns/stored" {
		t.Errorf("network namespace = %q, want /var/run/netns/stored", requests[0].NetworkNamespace)
	}
}
