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
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
)

func TestHasEthernetFabricInterface(t *testing.T) {
	originalSysClassNet := sysClassNet
	sysClassNet = t.TempDir()
	t.Cleanup(func() { sysClassNet = originalSysClassNet })

	if err := os.Mkdir(filepath.Join(sysClassNet, "eth0"), 0o755); err != nil {
		t.Fatalf("Mkdir() returned error: %v", err)
	}
	if hasEthernetFabricInterface() {
		t.Fatal("hasEthernetFabricInterface() = true without an rdmaN interface")
	}
	if err := os.Mkdir(filepath.Join(sysClassNet, "rdma3"), 0o755); err != nil {
		t.Fatalf("Mkdir() returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sysClassNet, "rdma3", "type"), []byte("32\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() returned error: %v", err)
	}
	if hasEthernetFabricInterface() {
		t.Fatal("hasEthernetFabricInterface() = true with only native InfiniBand")
	}
	if err := os.Mkdir(filepath.Join(sysClassNet, "rdma4"), 0o755); err != nil {
		t.Fatalf("Mkdir() returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sysClassNet, "rdma4", "type"), []byte("1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() returned error: %v", err)
	}
	if !hasEthernetFabricInterface() {
		t.Fatal("hasEthernetFabricInterface() = false with an Ethernet fabric interface")
	}
}

func TestGetDeviceConfig(t *testing.T) {
	originalSysfs := sysfsPCIDevices
	originalSysClassNet := sysClassNet
	originalProc := procSysNetIPv4Conf
	sysfsPCIDevices = t.TempDir()
	sysClassNet = t.TempDir()
	procSysNetIPv4Conf = t.TempDir()
	t.Cleanup(func() {
		sysfsPCIDevices = originalSysfs
		sysClassNet = originalSysClassNet
		procSysNetIPv4Conf = originalProc
	})

	addDevice := func(pciAddress, ifName, hardwareType string, arpIgnore, arpAnnounce *string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(sysfsPCIDevices, pciAddress, "net", ifName), 0o755); err != nil {
			t.Fatalf("failed to create fake sysfs: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(sysClassNet, ifName), 0o755); err != nil {
			t.Fatalf("failed to create fake network class: %v", err)
		}
		if err := os.WriteFile(filepath.Join(sysClassNet, ifName, "type"), []byte(hardwareType), 0o644); err != nil {
			t.Fatalf("failed to write fake interface type: %v", err)
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
	addDevice("0000:0c:00.0", "rdma0", "1\n", &one, &two)
	addDevice("0000:0d:00.0", "eth0", "1\n", &one, &two)
	addDevice("0000:0e:00.0", "rdma1", "1\n", &four, &two)
	addDevice("0000:12:00.0", "rdma3", "32\n", nil, nil)
	addDevice("0000:13:00.0", "rdma4", "invalid\n", nil, nil)
	addDevice("0000:14:00.0", "rdma5", "772\n", nil, nil)
	ipv4Instance := newOKEInstance(&okeMetadata{RDMAFabric: &RDMAFabric{IPv6: false, Planes: 0}}, nil)

	tests := []struct {
		name                   string
		instance               *OKEInstance
		id                     cloudprovider.DeviceIdentifiers
		want                   *apis.NetworkConfig
		wantRequiresFabricData bool
	}{
		{
			name:     "fabric interface",
			instance: ipv4Instance,
			id:       cloudprovider.DeviceIdentifiers{Name: "pci-0000-0c-00-0", PCIAddress: "0000:0c:00.0"},
			want: &apis.NetworkConfig{
				Profile: okeRDMAIPv4Profile,
				Interface: apis.InterfaceConfig{
					ARPIgnore:   ptr.To[int32](1),
					ARPAnnounce: ptr.To[int32](2),
				},
				SubInterface: &apis.SubInterfaceConfig{Type: apis.SubInterfaceTypeIPVlan},
			},
			wantRequiresFabricData: true,
		},
		{
			name:     "IPv6 fabric uses address-free ipvlan",
			instance: newOKEInstance(&okeMetadata{RDMAFabric: &RDMAFabric{IPv6: true, Planes: 8}}, nil),
			id:       cloudprovider.DeviceIdentifiers{Name: "pci-0000-0c-00-0", PCIAddress: "0000:0c:00.0"},
			want: &apis.NetworkConfig{SubInterface: &apis.SubInterfaceConfig{
				Type:        apis.SubInterfaceTypeIPVlan,
				AddressMode: apis.SubInterfaceAddressModeSLAAC,
			}},
			wantRequiresFabricData: true,
		},
		{
			name:     "missing fabric data keeps a failing profile",
			instance: newOKEInstance(nil, nil),
			id:       cloudprovider.DeviceIdentifiers{Name: "pci-0000-0c-00-0", PCIAddress: "0000:0c:00.0"},
			want: &apis.NetworkConfig{
				Profile:      okeRDMAIPv4Profile,
				SubInterface: &apis.SubInterfaceConfig{Type: apis.SubInterfaceTypeIPVlan},
			},
			wantRequiresFabricData: true,
		},
		{
			name: "non-fabric interface",
			id:   cloudprovider.DeviceIdentifiers{Name: "pci-0000-0d-00-0", PCIAddress: "0000:0d:00.0"},
		},
		{
			name: "native InfiniBand interface uses direct move",
			id:   cloudprovider.DeviceIdentifiers{Name: "pci-0000-12-00-0", PCIAddress: "0000:12:00.0"},
		},
		{
			name: "invalid interface type keeps failing profile",
			id:   cloudprovider.DeviceIdentifiers{Name: "pci-0000-13-00-0", PCIAddress: "0000:13:00.0"},
			want: &apis.NetworkConfig{
				Profile:      okeRDMAIPv4Profile,
				SubInterface: &apis.SubInterfaceConfig{Type: apis.SubInterfaceTypeIPVlan},
			},
		},
		{
			name: "unsupported interface type keeps failing profile",
			id:   cloudprovider.DeviceIdentifiers{Name: "pci-0000-14-00-0", PCIAddress: "0000:14:00.0"},
			want: &apis.NetworkConfig{
				Profile:      okeRDMAIPv4Profile,
				SubInterface: &apis.SubInterfaceConfig{Type: apis.SubInterfaceTypeIPVlan},
			},
		},
		{
			name: "missing PCI address",
			id:   cloudprovider.DeviceIdentifiers{Name: "dev1"},
		},
		{
			name:     "invalid ARP value keeps ipvlan",
			instance: ipv4Instance,
			id:       cloudprovider.DeviceIdentifiers{Name: "pci-0000-0e-00-0", PCIAddress: "0000:0e:00.0"},
			want: &apis.NetworkConfig{
				Profile:      okeRDMAIPv4Profile,
				Interface:    apis.InterfaceConfig{ARPAnnounce: ptr.To[int32](2)},
				SubInterface: &apis.SubInterfaceConfig{Type: apis.SubInterfaceTypeIPVlan},
			},
			wantRequiresFabricData: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := tt.instance
			if instance == nil {
				instance = newOKEInstance(nil, nil)
			}
			got := instance.GetDeviceConfig(tt.id)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("GetDeviceConfig() mismatch (-want +got):\n%s", diff)
			}
			if got := instance.requiresFabricData.Load(); got != tt.wantRequiresFabricData {
				t.Errorf("requiresFabricData = %v, want %v", got, tt.wantRequiresFabricData)
			}
		})
	}
}

func TestGetProfileConfig(t *testing.T) {
	originalSysfs := sysfsPCIDevices
	originalSysClassNet := sysClassNet
	originalInterfaceAddresses := interfaceAddresses
	sysfsPCIDevices = t.TempDir()
	sysClassNet = t.TempDir()
	t.Cleanup(func() {
		sysfsPCIDevices = originalSysfs
		sysClassNet = originalSysClassNet
		interfaceAddresses = originalInterfaceAddresses
	})

	addDevice := func(pciAddress, ifName, hardwareType string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(sysfsPCIDevices, pciAddress, "net", ifName), 0o755); err != nil {
			t.Fatalf("failed to create fake sysfs: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(sysClassNet, ifName), 0o755); err != nil {
			t.Fatalf("failed to create fake network class: %v", err)
		}
		if err := os.WriteFile(filepath.Join(sysClassNet, ifName, "type"), []byte(hardwareType), 0o644); err != nil {
			t.Fatalf("failed to write fake interface type: %v", err)
		}
	}
	addDevice("0000:0c:00.0", "rdma0", "1\n")
	addDevice("0000:0d:00.0", "rdma15", "1\n")
	addDevice("0000:0e:00.0", "rdma16", "1\n")
	addDevice("0000:0f:00.0", "eth0", "1\n")
	addDevice("0000:10:00.0", "rdma1", "1\n")
	addDevice("0000:11:00.0", "rdma2", "1\n")
	addDevice("0000:12:00.0", "rdma3", "32\n")

	interfaceAddresses = func(ifName string) ([]net.Addr, error) {
		addresses := map[string]string{
			"rdma0":  "10.224.6.100/12",
			"rdma15": "10.225.230.100/12",
			"rdma16": "10.225.250.100/12",
			"eth0":   "10.140.70.100/19",
			"rdma1":  "10.226.6.100/12",
			"rdma2":  "10.224.6.101/12",
		}
		cidr, ok := addresses[ifName]
		if !ok {
			return nil, os.ErrNotExist
		}
		ip, network, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, err
		}
		network.IP = ip
		return []net.Addr{network}, nil
	}

	baseConfig := &apis.NetworkConfig{
		Profile:      okeRDMAIPv4Profile,
		SubInterface: &apis.SubInterfaceConfig{Type: apis.SubInterfaceTypeIPVlan},
	}
	ipv4Instance := newOKEInstance(&okeMetadata{RDMAFabric: &RDMAFabric{IPv6: false, Planes: 0}}, nil)
	tests := []struct {
		name            string
		instance        *OKEInstance
		id              cloudprovider.DeviceIdentifiers
		config          *apis.NetworkConfig
		want            *apis.NetworkConfig
		wantErr         bool
		wantErrContains string
	}{
		{
			name:   "first rail",
			id:     cloudprovider.DeviceIdentifiers{Name: "pci-0000-0c-00-0", PCIAddress: "0000:0c:00.0"},
			config: baseConfig,
			want: &apis.NetworkConfig{
				SubInterface: &apis.SubInterfaceConfig{Addresses: []string{"10.240.6.100/15"}},
				Routes: []apis.RouteConfig{{
					Destination: "10.240.0.0/15",
					Source:      "10.240.6.100",
					Scope:       253,
					Table:       100,
				}},
				Rules: []apis.RuleConfig{{Priority: 32000, Source: "10.240.6.100/32", Table: 100}},
			},
		},
		{
			name:   "last rail",
			id:     cloudprovider.DeviceIdentifiers{Name: "pci-0000-0d-00-0", PCIAddress: "0000:0d:00.0"},
			config: baseConfig,
			want: &apis.NetworkConfig{
				SubInterface: &apis.SubInterfaceConfig{Addresses: []string{"10.241.230.100/15"}},
				Routes: []apis.RouteConfig{{
					Destination: "10.240.0.0/15",
					Source:      "10.241.230.100",
					Scope:       253,
					Table:       115,
				}},
				Rules: []apis.RuleConfig{{Priority: 32000, Source: "10.241.230.100/32", Table: 115}},
			},
		},
		{
			name:    "unsupported profile",
			id:      cloudprovider.DeviceIdentifiers{Name: "pci-0000-0c-00-0", PCIAddress: "0000:0c:00.0"},
			config:  &apis.NetworkConfig{Profile: "other", SubInterface: &apis.SubInterfaceConfig{Type: apis.SubInterfaceTypeIPVlan}},
			wantErr: true,
		},
		{
			name:    "profile requires ipvlan",
			id:      cloudprovider.DeviceIdentifiers{Name: "pci-0000-0c-00-0", PCIAddress: "0000:0c:00.0"},
			config:  &apis.NetworkConfig{Profile: okeRDMAIPv4Profile},
			wantErr: true,
		},
		{
			name:     "IPv4 profile rejects IPv6 fabric",
			instance: newOKEInstance(&okeMetadata{RDMAFabric: &RDMAFabric{IPv6: true, Planes: 8}}, nil),
			id:       cloudprovider.DeviceIdentifiers{Name: "pci-0000-0c-00-0", PCIAddress: "0000:0c:00.0"},
			config:   baseConfig,
			wantErr:  true,
		},
		{
			name:     "IPv4 profile rejects missing fabric data",
			instance: newOKEInstance(nil, nil),
			id:       cloudprovider.DeviceIdentifiers{Name: "pci-0000-0c-00-0", PCIAddress: "0000:0c:00.0"},
			config:   baseConfig,
			wantErr:  true,
		},
		{
			name:    "rail index above tested B4 count",
			id:      cloudprovider.DeviceIdentifiers{Name: "pci-0000-0e-00-0", PCIAddress: "0000:0e:00.0"},
			config:  baseConfig,
			wantErr: true,
		},
		{
			name:    "non-fabric interface",
			id:      cloudprovider.DeviceIdentifiers{Name: "pci-0000-0f-00-0", PCIAddress: "0000:0f:00.0"},
			config:  baseConfig,
			wantErr: true,
		},
		{
			name:    "parent address outside source range",
			id:      cloudprovider.DeviceIdentifiers{Name: "pci-0000-10-00-0", PCIAddress: "0000:10:00.0"},
			config:  baseConfig,
			wantErr: true,
		},
		{
			name:    "parent address belongs to another rail",
			id:      cloudprovider.DeviceIdentifiers{Name: "pci-0000-11-00-0", PCIAddress: "0000:11:00.0"},
			config:  baseConfig,
			wantErr: true,
		},
		{
			name:            "native InfiniBand parent rejects ipvlan profile",
			id:              cloudprovider.DeviceIdentifiers{Name: "pci-0000-12-00-0", PCIAddress: "0000:12:00.0"},
			config:          baseConfig,
			wantErr:         true,
			wantErrContains: "rdma3 has ARPHRD type 32",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := tt.instance
			if instance == nil {
				instance = ipv4Instance
			}
			got, err := instance.GetProfileConfig(tt.id, "claim-uid", tt.config)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("GetProfileConfig() returned no error, got %#v", got)
				}
				if tt.wantErrContains != "" && !strings.Contains(err.Error(), tt.wantErrContains) {
					t.Fatalf("GetProfileConfig() error = %q, want it to contain %q", err, tt.wantErrContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetProfileConfig() returned error: %v", err)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("GetProfileConfig() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestValidateClassicRailLayout(t *testing.T) {
	tests := []struct {
		name      string
		address   string
		railIndex int
		wantErr   bool
	}{
		{name: "first rail", address: "10.224.6.100", railIndex: 0},
		{name: "last rail", address: "10.225.230.100", railIndex: 15},
		{name: "wrong rail", address: "10.224.6.100", railIndex: 1, wantErr: true},
		{name: "rail above classic range", address: "10.225.250.100", railIndex: 16, wantErr: true},
		{name: "address outside classic range", address: "10.226.6.100", railIndex: 0, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateClassicRailLayout(netip.MustParseAddr(tt.address), tt.railIndex)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateClassicRailLayout() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestTranslateIPv4Address(t *testing.T) {
	tests := []struct {
		name    string
		address string
		source  string
		target  string
		want    string
		wantErr bool
	}{
		{
			name:    "preserves host bits",
			address: "10.225.230.100",
			source:  "10.224.0.0/15",
			target:  "10.240.0.0/15",
			want:    "10.241.230.100",
		},
		{
			name:    "address outside source",
			address: "10.226.0.1",
			source:  "10.224.0.0/15",
			target:  "10.240.0.0/15",
			wantErr: true,
		},
		{
			name:    "different prefix lengths",
			address: "10.224.0.1",
			source:  "10.224.0.0/15",
			target:  "10.240.0.0/16",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := translateIPv4Address(
				netip.MustParseAddr(tt.address),
				netip.MustParsePrefix(tt.source),
				netip.MustParsePrefix(tt.target),
			)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("translateIPv4Address() = %s, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("translateIPv4Address() returned error: %v", err)
			}
			if got.String() != tt.want {
				t.Errorf("translateIPv4Address() = %s, want %s", got, tt.want)
			}
		})
	}
}
