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
	"strings"
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
	instance := &OKEInstance{
		HPCIslandId:    "fake-island-id",
		NetworkBlockId: "fake-network-block-id",
		RackId:         "fake-rack-id",
	}
	got := instance.GetDeviceConfig(cloudprovider.DeviceIdentifiers{Name: "dev1"})
	if got != nil {
		t.Errorf("GetDeviceConfig() = %v, want nil", got)
	}
}

// fakeHost builds a sysfs and procfs tree for one PCI device and points the
// package paths at it for the duration of the test.
func fakeHost(t *testing.T, pciAddress, ifName string, settings map[string]string) {
	t.Helper()
	root := t.TempDir()

	netDir := filepath.Join(root, "sys", pciAddress, "net", ifName)
	if err := os.MkdirAll(netDir, 0o755); err != nil {
		t.Fatalf("failed to build sysfs tree: %v", err)
	}
	confDir := filepath.Join(root, "proc", ifName)
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatalf("failed to build procfs tree: %v", err)
	}
	for setting, value := range settings {
		if err := os.WriteFile(filepath.Join(confDir, setting), []byte(value), 0o644); err != nil {
			t.Fatalf("failed to write %s: %v", setting, err)
		}
	}

	origSysfs, origProc := sysfsPCIDevices, procSysNetIPv4Conf
	sysfsPCIDevices = filepath.Join(root, "sys")
	procSysNetIPv4Conf = filepath.Join(root, "proc")
	t.Cleanup(func() {
		sysfsPCIDevices, procSysNetIPv4Conf = origSysfs, origProc
	})
}

func TestGetProfileConfig(t *testing.T) {
	fakeHost(t, "0000:0c:00.0", "rdma0", map[string]string{
		"arp_ignore":   "1\n",
		"arp_announce": "2\n",
	})

	instance := &OKEInstance{}
	got, err := instance.GetProfileConfig(
		cloudprovider.DeviceIdentifiers{Name: "pci-0000-0c-00-0", PCIAddress: "0000:0c:00.0"},
		"claim-uid",
		&apis.NetworkConfig{Profile: RDMAProfile},
	)
	if err != nil {
		t.Fatalf("GetProfileConfig() error: %v", err)
	}
	want := &apis.NetworkConfig{
		Interface: apis.InterfaceConfig{
			ARPIgnore:   ptr.To[int32](1),
			ARPAnnounce: ptr.To[int32](2),
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("GetProfileConfig() mismatch (-want +got):\n%s", diff)
	}
}

// The point of reading at prepare time is fidelity to whatever the host has,
// not a hardcoded 1 and 2.
func TestGetProfileConfigReturnsTheActualHostValues(t *testing.T) {
	fakeHost(t, "0000:0c:00.0", "rdma0", map[string]string{
		"arp_ignore":   "8",
		"arp_announce": "1",
	})

	instance := &OKEInstance{}
	got, err := instance.GetProfileConfig(
		cloudprovider.DeviceIdentifiers{Name: "pci-0000-0c-00-0", PCIAddress: "0000:0c:00.0"},
		"claim-uid",
		&apis.NetworkConfig{Profile: RDMAProfile},
	)
	if err != nil {
		t.Fatalf("GetProfileConfig() error: %v", err)
	}
	if *got.Interface.ARPIgnore != 8 || *got.Interface.ARPAnnounce != 1 {
		t.Errorf("GetProfileConfig() = %d,%d, want the host values 8,1",
			*got.Interface.ARPIgnore, *got.Interface.ARPAnnounce)
	}
}

func TestGetProfileConfigSkipsMissingSettings(t *testing.T) {
	fakeHost(t, "0000:0c:00.0", "rdma0", map[string]string{"arp_ignore": "1"})

	instance := &OKEInstance{}
	got, err := instance.GetProfileConfig(
		cloudprovider.DeviceIdentifiers{Name: "pci-0000-0c-00-0", PCIAddress: "0000:0c:00.0"},
		"claim-uid",
		&apis.NetworkConfig{Profile: RDMAProfile},
	)
	if err != nil {
		t.Fatalf("GetProfileConfig() error: %v", err)
	}
	if got.Interface.ARPIgnore == nil || *got.Interface.ARPIgnore != 1 {
		t.Errorf("GetProfileConfig() arpIgnore = %v, want 1", got.Interface.ARPIgnore)
	}
	if got.Interface.ARPAnnounce != nil {
		t.Errorf("GetProfileConfig() arpAnnounce = %v, want nil", *got.Interface.ARPAnnounce)
	}
}

func TestGetProfileConfigErrors(t *testing.T) {
	fakeHost(t, "0000:0c:00.0", "rdma0", map[string]string{"arp_ignore": "1"})
	instance := &OKEInstance{}
	id := cloudprovider.DeviceIdentifiers{Name: "pci-0000-0c-00-0", PCIAddress: "0000:0c:00.0"}

	tests := []struct {
		name    string
		id      cloudprovider.DeviceIdentifiers
		config  *apis.NetworkConfig
		wantErr string
	}{
		{
			name:    "nil config",
			id:      id,
			config:  nil,
			wantErr: "no configuration provided",
		},
		{
			name:    "unknown profile",
			id:      id,
			config:  &apis.NetworkConfig{Profile: "something-else"},
			wantErr: "unsupported profile",
		},
		{
			name:    "no PCI address",
			id:      cloudprovider.DeviceIdentifiers{Name: "net-abc"},
			config:  &apis.NetworkConfig{Profile: RDMAProfile},
			wantErr: "no PCI address",
		},
		{
			name:    "unknown PCI address",
			id:      cloudprovider.DeviceIdentifiers{Name: "pci-x", PCIAddress: "0000:ff:00.0"},
			config:  &apis.NetworkConfig{Profile: RDMAProfile},
			wantErr: "failed to list",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := instance.GetProfileConfig(tt.id, "claim-uid", tt.config)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("GetProfileConfig() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestReleaseProfileConfigIsANoOp(t *testing.T) {
	instance := &OKEInstance{}
	if err := instance.ReleaseProfileConfig(
		cloudprovider.DeviceIdentifiers{Name: "pci-0000-0c-00-0"},
		"claim-uid",
		&apis.NetworkConfig{Profile: RDMAProfile},
	); err != nil {
		t.Errorf("ReleaseProfileConfig() error: %v", err)
	}
}

// A claim that sets its own value must win over the host value.
func TestProfileConfigIsOverridableByUser(t *testing.T) {
	fakeHost(t, "0000:0c:00.0", "rdma0", map[string]string{
		"arp_ignore":   "1",
		"arp_announce": "2",
	})

	instance := &OKEInstance{}
	profileConf, err := instance.GetProfileConfig(
		cloudprovider.DeviceIdentifiers{Name: "pci-0000-0c-00-0", PCIAddress: "0000:0c:00.0"},
		"claim-uid",
		&apis.NetworkConfig{Profile: RDMAProfile},
	)
	if err != nil {
		t.Fatalf("GetProfileConfig() error: %v", err)
	}

	// Same order the driver uses: the already merged user config wins.
	userConf := &apis.NetworkConfig{
		Profile:   RDMAProfile,
		Interface: apis.InterfaceConfig{ARPIgnore: ptr.To[int32](3)},
	}
	merged := apis.MergeNetworkConfig(userConf, profileConf)
	if got := *merged.Interface.ARPIgnore; got != 3 {
		t.Errorf("merged arpIgnore = %d, want the user value 3", got)
	}
	if got := *merged.Interface.ARPAnnounce; got != 2 {
		t.Errorf("merged arpAnnounce = %d, want the host value 2", got)
	}
}
