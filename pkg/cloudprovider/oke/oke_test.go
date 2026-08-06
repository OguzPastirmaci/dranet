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

func TestGetDeviceConfig(t *testing.T) {
	originalSysfs := sysfsPCIDevices
	originalProc := procSysNetIPv4Conf
	sysfsPCIDevices = t.TempDir()
	procSysNetIPv4Conf = t.TempDir()
	t.Cleanup(func() {
		sysfsPCIDevices = originalSysfs
		procSysNetIPv4Conf = originalProc
	})

	addDevice := func(pciAddress, ifName string, arpIgnore, arpAnnounce *string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(sysfsPCIDevices, pciAddress, "net", ifName), 0o755); err != nil {
			t.Fatalf("failed to create fake sysfs: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(procSysNetIPv4Conf, ifName), 0o755); err != nil {
			t.Fatalf("failed to create fake proc sys: %v", err)
		}
		for setting, value := range map[string]*string{"arp_ignore": arpIgnore, "arp_announce": arpAnnounce} {
			if value == nil {
				continue
			}
			if err := os.WriteFile(filepath.Join(procSysNetIPv4Conf, ifName, setting), []byte(*value), 0o644); err != nil {
				t.Fatalf("failed to write fake %s: %v", setting, err)
			}
		}
	}

	one := "1"
	two := "2"
	four := "4"
	addDevice("0000:0c:00.0", "rdma0", &one, &two)
	addDevice("0000:0d:00.0", "eth0", &one, &two)
	addDevice("0000:0e:00.0", "rdma1", &four, &two)

	tests := []struct {
		name string
		id   cloudprovider.DeviceIdentifiers
		want *apis.NetworkConfig
	}{
		{
			name: "fabric interface",
			id:   cloudprovider.DeviceIdentifiers{Name: "pci-0000-0c-00-0", PCIAddress: "0000:0c:00.0"},
			want: &apis.NetworkConfig{
				Interface: apis.InterfaceConfig{
					ARPIgnore:   ptr.To[int32](1),
					ARPAnnounce: ptr.To[int32](2),
				},
				SubInterface: &apis.SubInterfaceConfig{Type: apis.SubInterfaceTypeIPVlan},
			},
		},
		{
			name: "non-fabric interface",
			id:   cloudprovider.DeviceIdentifiers{Name: "pci-0000-0d-00-0", PCIAddress: "0000:0d:00.0"},
		},
		{
			name: "missing PCI address",
			id:   cloudprovider.DeviceIdentifiers{Name: "dev1"},
		},
		{
			name: "invalid ARP value keeps ipvlan",
			id:   cloudprovider.DeviceIdentifiers{Name: "pci-0000-0e-00-0", PCIAddress: "0000:0e:00.0"},
			want: &apis.NetworkConfig{
				Interface:    apis.InterfaceConfig{ARPAnnounce: ptr.To[int32](2)},
				SubInterface: &apis.SubInterfaceConfig{Type: apis.SubInterfaceTypeIPVlan},
			},
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
