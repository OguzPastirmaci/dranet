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

package oke

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/utils/ptr"
)

func TestGetDeviceAttributes(t *testing.T) {
	tests := []struct {
		name     string
		instance *OKEInstance
		id       cloudprovider.DeviceIdentifiers
		want     map[resourceapi.QualifiedName]resourceapi.DeviceAttribute
	}{
		{
			name: "full topology with gpu memory fabric (GB200/GB300 shapes)",
			instance: &OKEInstance{
				HPCIslandId:     "fake-island-id",
				NetworkBlockId:  "fake-network-block-id",
				LocalBlockId:    "fake-local-block-id",
				RackId:          "fake-rack-id",
				GpuMemoryFabric: "fake-gpu-memory-fabric-id",
			},
			id: cloudprovider.DeviceIdentifiers{Name: "dev1"},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrOKEHPCIslandId:     {StringValue: ptr.To("fake-island-id")},
				AttrOKENetworkBlockId:  {StringValue: ptr.To("fake-network-block-id")},
				AttrOKELocalBlockId:    {StringValue: ptr.To("fake-local-block-id")},
				AttrOKERackId:          {StringValue: ptr.To("fake-rack-id")},
				AttrOKEGpuMemoryFabric: {StringValue: ptr.To("fake-gpu-memory-fabric-id")},
			},
		},
		{
			name: "H100 fallback: only networkBlockId and rackId (no rdmaTopologyData)",
			instance: &OKEInstance{
				NetworkBlockId: "fake-network-block-id",
				RackId:         "fake-rack-id",
			},
			id: cloudprovider.DeviceIdentifiers{Name: "dev1"},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrOKENetworkBlockId: {StringValue: ptr.To("fake-network-block-id")},
				AttrOKERackId:         {StringValue: ptr.To("fake-rack-id")},
			},
		},
		{
			name: "partial topology (only hpcIslandId and networkBlockId)",
			instance: &OKEInstance{
				HPCIslandId:    "fake-island-id",
				NetworkBlockId: "fake-network-block-id",
			},
			id: cloudprovider.DeviceIdentifiers{Name: "dev1"},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrOKEHPCIslandId:    {StringValue: ptr.To("fake-island-id")},
				AttrOKENetworkBlockId: {StringValue: ptr.To("fake-network-block-id")},
			},
		},
		{
			name:     "no topology data",
			instance: &OKEInstance{},
			id:       cloudprovider.DeviceIdentifiers{Name: "dev1"},
			want:     map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{},
		},
		{
			name: "attributes are node-level, same for any device identifier",
			instance: &OKEInstance{
				HPCIslandId:    "fake-island-id",
				NetworkBlockId: "fake-network-block-id",
				RackId:         "fake-rack-id",
			},
			id: cloudprovider.DeviceIdentifiers{
				Name:       "pci-0000-0c-00-0",
				MAC:        "a0:88:c2:a7:c5:04",
				PCIAddress: "0000:0c:00.0",
			},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrOKEHPCIslandId:    {StringValue: ptr.To("fake-island-id")},
				AttrOKENetworkBlockId: {StringValue: ptr.To("fake-network-block-id")},
				AttrOKERackId:         {StringValue: ptr.To("fake-rack-id")},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.instance.GetDeviceAttributes(tt.id)
			if diff := cmp.Diff(tt.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("GetDeviceAttributes() returned unexpected diff (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestOCIDSuffix(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "valid OCID extracts 60-char suffix",
			input: "ocid1.hpcisland.oc1.test-region-1.aaaaaaaa2mvjha24vj6evyafdqtis6nzqibhrnxxhzt65zkc3upy4xlrz5za",
			want:  "aaaaaaaa2mvjha24vj6evyafdqtis6nzqibhrnxxhzt65zkc3upy4xlrz5za",
		},
		{
			name:  "OCID with suffix longer than 60 chars is truncated to last 60",
			input: "ocid1.hpcisland.oc1.test-region-1.xaaaaaaaa2mvjha24vj6evyafdqtis6nzqibhrnxxhzt65zkc3upy4xlrz5za",
			want:  "aaaaaaaa2mvjha24vj6evyafdqtis6nzqibhrnxxhzt65zkc3upy4xlrz5za",
		},
		{
			name:  "empty string returns empty (field not present on shape)",
			input: "",
			want:  "",
		},
		{
			name:    "non-OCID string returns error",
			input:   "fakehexhash",
			wantErr: true,
		},
		{
			name:    "non-OCID dotted string returns error",
			input:   "some.dotted.value",
			wantErr: true,
		},
		{
			name:    "OCID without dot separator returns error",
			input:   "ocid1-hpcisland-no-dots",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ocidSuffix(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ocidSuffix(%q) = %q, want error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ocidSuffix(%q) unexpected error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("ocidSuffix(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// fakeSysfs builds a sysfs tree mapping PCI addresses to interface names and
// points the package path at it. A PCI address mapped to "" gets an empty net
// directory, which is what the host sees while the interface is in a Pod.
func fakeSysfs(t *testing.T, devices map[string]string) {
	t.Helper()
	root := t.TempDir()
	for pciAddress, ifName := range devices {
		dir := filepath.Join(root, pciAddress, "net")
		if ifName != "" {
			dir = filepath.Join(dir, ifName)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("failed to build sysfs tree: %v", err)
		}
	}
	orig := sysfsPCIDevices
	sysfsPCIDevices = root
	t.Cleanup(func() { sysfsPCIDevices = orig })
}

func TestGetDeviceConfig(t *testing.T) {
	// Mirrors a real BM.GPU.B4.8: the fabric PFs, the primary VNIC, and a VNIC
	// VLAN interface. Every one of these reports rdma=true on the node, which is
	// why the interface name is the discriminator.
	fakeSysfs(t, map[string]string{
		"0000:0c:00.0": "rdma0",
		"0000:d1:00.1": "rdma15",
		"0000:aa:00.0": "eth1",
		"0000:aa:00.1": "eth0.2679",
	})

	fabric := &apis.NetworkConfig{
		Interface: apis.InterfaceConfig{
			ARPIgnore:   ptr.To[int32](1),
			ARPAnnounce: ptr.To[int32](2),
		},
	}

	tests := []struct {
		name string
		id   cloudprovider.DeviceIdentifiers
		want *apis.NetworkConfig
	}{
		{
			name: "fabric interface rdma0",
			id:   cloudprovider.DeviceIdentifiers{Name: "pci-0000-0c-00-0", PCIAddress: "0000:0c:00.0"},
			want: fabric,
		},
		{
			name: "fabric interface rdma15",
			id:   cloudprovider.DeviceIdentifiers{Name: "pci-0000-d1-00-1", PCIAddress: "0000:d1:00.1"},
			want: fabric,
		},
		{
			name: "primary VNIC is skipped even though it is RDMA capable",
			id:   cloudprovider.DeviceIdentifiers{Name: "pci-0000-aa-00-0", PCIAddress: "0000:aa:00.0"},
			want: nil,
		},
		{
			name: "VNIC VLAN interface is skipped",
			id:   cloudprovider.DeviceIdentifiers{Name: "net-mv2gqmbogi3dooi", PCIAddress: "0000:aa:00.1"},
			want: nil,
		},
		{
			name: "device with no PCI address",
			id:   cloudprovider.DeviceIdentifiers{Name: "net-abc"},
			want: nil,
		},
		{
			name: "unknown PCI address",
			id:   cloudprovider.DeviceIdentifiers{Name: "pci-x", PCIAddress: "0000:ff:00.0"},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := (&OKEInstance{}).GetDeviceConfig(tt.id)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("GetDeviceConfig() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// While a fabric interface is attached to a Pod it is not visible in the host
// sysfs tree, so no configuration is produced until it comes back.
func TestGetDeviceConfigWhileInterfaceIsInAPod(t *testing.T) {
	fakeSysfs(t, map[string]string{"0000:0c:00.0": ""})

	got := (&OKEInstance{}).GetDeviceConfig(
		cloudprovider.DeviceIdentifiers{Name: "pci-0000-0c-00-0", PCIAddress: "0000:0c:00.0"},
	)
	if got != nil {
		t.Errorf("GetDeviceConfig() = %#v, want nil", got)
	}
}

func TestIsFabricInterface(t *testing.T) {
	tests := []struct {
		ifName string
		want   bool
	}{
		{"rdma0", true},
		{"rdma9", true},
		{"rdma15", true},
		{"eth0", false},
		{"eth1", false},
		{"eth0.2679", false},
		{"eth0v2679", false},
		{"lo", false},
		{"rdma", false},
		{"rdmabond0", false},
		{"rdma0.100", false},
		{"myrdma0", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.ifName, func(t *testing.T) {
			if got := isFabricInterface(tt.ifName); got != tt.want {
				t.Errorf("isFabricInterface(%q) = %v, want %v", tt.ifName, got, tt.want)
			}
		})
	}
}

// The provider only supplies a default; a user value must win.
func TestGetDeviceConfigIsOverridableByUser(t *testing.T) {
	fakeSysfs(t, map[string]string{"0000:0c:00.0": "rdma0"})

	cloudConf := (&OKEInstance{}).GetDeviceConfig(
		cloudprovider.DeviceIdentifiers{Name: "pci-0000-0c-00-0", PCIAddress: "0000:0c:00.0"},
	)
	userConf := &apis.NetworkConfig{
		Interface: apis.InterfaceConfig{ARPIgnore: ptr.To[int32](2)},
	}

	merged := apis.MergeNetworkConfig(userConf, cloudConf)
	if got := *merged.Interface.ARPIgnore; got != 2 {
		t.Errorf("merged arpIgnore = %d, want the user value 2", got)
	}
	if got := *merged.Interface.ARPAnnounce; got != 2 {
		t.Errorf("merged arpAnnounce = %d, want the provider value 2", got)
	}
}

// Documents a MergeNetworkConfig limitation rather than desired behavior:
// mergo compares pointed-to values, so a user's explicit 0 looks unset and the
// provider default survives. The same applies to any pointer field, which is
// why a user cannot currently override a cloud `dhcp: true` with `dhcp: false`.
func TestGetDeviceConfigCannotBeOverriddenToZero(t *testing.T) {
	fakeSysfs(t, map[string]string{"0000:0c:00.0": "rdma0"})

	cloudConf := (&OKEInstance{}).GetDeviceConfig(
		cloudprovider.DeviceIdentifiers{Name: "pci-0000-0c-00-0", PCIAddress: "0000:0c:00.0"},
	)
	userConf := &apis.NetworkConfig{
		Interface: apis.InterfaceConfig{ARPIgnore: ptr.To[int32](0)},
	}

	merged := apis.MergeNetworkConfig(userConf, cloudConf)
	if got := *merged.Interface.ARPIgnore; got != 1 {
		t.Errorf("merged arpIgnore = %d, want the provider default 1 to survive", got)
	}
}
