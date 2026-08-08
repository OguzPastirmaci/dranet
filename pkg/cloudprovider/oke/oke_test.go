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
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

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
			instance: newOKEInstance(&okeMetadata{
				HPCIslandId:     "fake-island-id",
				NetworkBlockId:  "fake-network-block-id",
				LocalBlockId:    "fake-local-block-id",
				RackId:          "fake-rack-id",
				GpuMemoryFabric: "fake-gpu-memory-fabric-id",
			}, nil),
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
			instance: newOKEInstance(&okeMetadata{
				NetworkBlockId: "fake-network-block-id",
				RackId:         "fake-rack-id",
			}, nil),
			id: cloudprovider.DeviceIdentifiers{Name: "dev1"},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrOKENetworkBlockId: {StringValue: ptr.To("fake-network-block-id")},
				AttrOKERackId:         {StringValue: ptr.To("fake-rack-id")},
			},
		},
		{
			name: "partial topology (only hpcIslandId and networkBlockId)",
			instance: newOKEInstance(&okeMetadata{
				HPCIslandId:    "fake-island-id",
				NetworkBlockId: "fake-network-block-id",
			}, nil),
			id: cloudprovider.DeviceIdentifiers{Name: "dev1"},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrOKEHPCIslandId:    {StringValue: ptr.To("fake-island-id")},
				AttrOKENetworkBlockId: {StringValue: ptr.To("fake-network-block-id")},
			},
		},
		{
			name:     "no topology data",
			instance: newOKEInstance(nil, nil),
			id:       cloudprovider.DeviceIdentifiers{Name: "dev1"},
			want:     map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{},
		},
		{
			name: "attributes are node-level, same for any device identifier",
			instance: newOKEInstance(&okeMetadata{
				HPCIslandId:    "fake-island-id",
				NetworkBlockId: "fake-network-block-id",
				RackId:         "fake-rack-id",
			}, nil),
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

func TestGetDeviceAttributesRDMAFabric(t *testing.T) {
	tests := []struct {
		name     string
		instance *OKEInstance
		want     map[resourceapi.QualifiedName]resourceapi.DeviceAttribute
	}{
		{
			name:     "IPv4 fabric with zero planes",
			instance: newOKEInstance(&okeMetadata{RDMAFabric: &RDMAFabric{IPv6: false, Planes: 0}}, nil),
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrOKERDMAFabricIPv6:   {BoolValue: ptr.To(false)},
				AttrOKERDMAFabricPlanes: {IntValue: ptr.To[int64](0)},
			},
		},
		{
			name:     "IPv6 fabric with multiple planes",
			instance: newOKEInstance(&okeMetadata{RDMAFabric: &RDMAFabric{IPv6: true, Planes: 8}}, nil),
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrOKERDMAFabricIPv6:   {BoolValue: ptr.To(true)},
				AttrOKERDMAFabricPlanes: {IntValue: ptr.To[int64](8)},
			},
		},
		{
			name:     "fabric data is absent",
			instance: newOKEInstance(nil, nil),
			want:     map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.instance.GetDeviceAttributes(cloudprovider.DeviceIdentifiers{Name: "dev1"})
			if diff := cmp.Diff(tt.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("GetDeviceAttributes() returned unexpected diff (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestParseHostMetadataRDMAFabricData(t *testing.T) {
	const body = `{
		"rdmaFabricData": {
			"ipv6": false,
			"planes": 0
		}
	}`

	var metadata imdsHostMetadata
	if err := json.Unmarshal([]byte(body), &metadata); err != nil {
		t.Fatalf("Unmarshal() returned error: %v", err)
	}
	if metadata.RDMAFabricData == nil {
		t.Fatal("RDMAFabricData is nil")
	}
	if metadata.RDMAFabricData.IPv6 == nil || *metadata.RDMAFabricData.IPv6 {
		t.Error("IPv6 is true, want false")
	}
	if metadata.RDMAFabricData.Planes == nil || *metadata.RDMAFabricData.Planes != 0 {
		t.Errorf("Planes = %v, want 0", metadata.RDMAFabricData.Planes)
	}
}

func TestQueryRDMAFabricData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/opc/v2/host/rdmaFabricData/" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer Oracle" {
			http.Error(w, "missing authorization", http.StatusUnauthorized)
			return
		}
		if _, err := w.Write([]byte(`{"ipv6":true,"planes":8}`)); err != nil {
			t.Errorf("Write() returned error: %v", err)
		}
	}))
	defer server.Close()

	got, err := queryRDMAFabricData(context.Background(), server.Client(), server.URL+"/opc/v2/host/rdmaFabricData/")
	if err != nil {
		t.Fatalf("queryRDMAFabricData() returned error: %v", err)
	}
	if got.IPv6 == nil || !*got.IPv6 || got.Planes == nil || *got.Planes != 8 {
		t.Errorf("queryRDMAFabricData() = %#v, want IPv6 with 8 planes", got)
	}
}

func TestQueryRDMAFabricDataRejectsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	if _, err := queryRDMAFabricData(context.Background(), server.Client(), server.URL); err == nil {
		t.Fatal("queryRDMAFabricData() returned no error")
	}
}

func TestParseRDMAFabricDataRequiresAllFields(t *testing.T) {
	tests := []struct {
		name    string
		data    *imdsRDMAFabricData
		want    *RDMAFabric
		wantErr bool
	}{
		{
			name: "IPv4 with zero planes",
			data: &imdsRDMAFabricData{IPv6: ptr.To(false), Planes: ptr.To[int64](0)},
			want: &RDMAFabric{IPv6: false, Planes: 0},
		},
		{
			name:    "missing IPv6",
			data:    &imdsRDMAFabricData{Planes: ptr.To[int64](8)},
			wantErr: true,
		},
		{
			name:    "missing planes",
			data:    &imdsRDMAFabricData{IPv6: ptr.To(true)},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRDMAFabricData(tt.data)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseRDMAFabricData() returned no error, got %#v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRDMAFabricData() returned error: %v", err)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("parseRDMAFabricData() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestFetchOKEMetadataUsesDirectFabricEndpoint(t *testing.T) {
	originalSysClassNet := sysClassNet
	sysClassNet = t.TempDir()
	t.Cleanup(func() { sysClassNet = originalSysClassNet })
	if err := os.Mkdir(filepath.Join(sysClassNet, "rdma0"), 0o755); err != nil {
		t.Fatalf("Mkdir() returned error: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer Oracle" {
			http.Error(w, "missing authorization", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/host/":
			_, _ = w.Write([]byte(`{"networkBlockId":"network-1","rackId":"rack-1"}`))
		case "/host/rdmaFabricData/":
			_, _ = w.Write([]byte(`{"ipv6":false,"planes":0}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	got, err := fetchOKEMetadata(context.Background(), server.Client(), server.URL)
	if err != nil {
		t.Fatalf("fetchOKEMetadata() returned error: %v", err)
	}
	want := &okeMetadata{
		NetworkBlockId: "network-1",
		RackId:         "rack-1",
		RDMAFabric:     &RDMAFabric{IPv6: false, Planes: 0},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("fetchOKEMetadata() mismatch (-want +got):\n%s", diff)
	}
}

func TestFetchOKEMetadataRejectsPartialFabricData(t *testing.T) {
	originalSysClassNet := sysClassNet
	sysClassNet = t.TempDir()
	t.Cleanup(func() { sysClassNet = originalSysClassNet })
	if err := os.Mkdir(filepath.Join(sysClassNet, "rdma0"), 0o755); err != nil {
		t.Fatalf("Mkdir() returned error: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/host/":
			_, _ = w.Write([]byte(`{"networkBlockId":"network-1"}`))
		case "/host/rdmaFabricData/":
			_, _ = w.Write([]byte(`{"ipv6":false}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	if _, err := fetchOKEMetadata(context.Background(), server.Client(), server.URL); err == nil {
		t.Fatal("fetchOKEMetadata() returned no error for partial RDMA fabric data")
	}
}

func TestRefreshMetadataMergesPartialData(t *testing.T) {
	instance := newOKEInstance(&okeMetadata{
		HPCIslandId:    "island-1",
		NetworkBlockId: "network-1",
		RDMAFabric:     &RDMAFabric{IPv6: false, Planes: 0},
	}, func(context.Context) (*okeMetadata, error) {
		return &okeMetadata{
			RackId:     "rack-1",
			RDMAFabric: &RDMAFabric{IPv6: true, Planes: 8},
		}, nil
	})

	if err := instance.refreshMetadata(context.Background()); err != nil {
		t.Fatalf("refreshMetadata() returned error: %v", err)
	}
	want := &okeMetadata{
		HPCIslandId:    "island-1",
		NetworkBlockId: "network-1",
		RackId:         "rack-1",
		RDMAFabric:     &RDMAFabric{IPv6: false, Planes: 8},
	}
	if diff := cmp.Diff(want, instance.metadata.Load()); diff != "" {
		t.Errorf("refreshMetadata() mismatch (-want +got):\n%s", diff)
	}
}

func TestRefreshMetadataKeepsLastKnownDataOnFailure(t *testing.T) {
	want := &okeMetadata{
		NetworkBlockId: "network-1",
		RDMAFabric:     &RDMAFabric{IPv6: false, Planes: 0},
	}
	instance := newOKEInstance(want, func(context.Context) (*okeMetadata, error) {
		return nil, errors.New("IMDS is not ready")
	})

	if err := instance.refreshMetadata(context.Background()); err == nil {
		t.Fatal("refreshMetadata() returned no error")
	}
	if instance.metadata.Load() != want {
		t.Fatal("refreshMetadata() replaced the last known metadata after a failure")
	}
}

func TestRefreshLoopRecoversAfterStartupFailure(t *testing.T) {
	attempts := make(chan int, 2)
	attempt := 0
	instance := newOKEInstance(nil, func(context.Context) (*okeMetadata, error) {
		attempt++
		attempts <- attempt
		if attempt == 1 {
			return nil, errors.New("IMDS is not ready")
		}
		return &okeMetadata{RDMAFabric: &RDMAFabric{IPv6: true, Planes: 8}}, nil
	})
	instance.retryInterval = time.Millisecond
	instance.maxRetryInterval = 2 * time.Millisecond
	instance.refreshInterval = time.Hour
	instance.requestTimeout = time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go instance.refreshLoop(ctx)

	for wantAttempt := 1; wantAttempt <= 2; wantAttempt++ {
		select {
		case gotAttempt := <-attempts:
			if gotAttempt != wantAttempt {
				t.Fatalf("refreshLoop() attempt = %d, want %d", gotAttempt, wantAttempt)
			}
		case <-time.After(time.Second):
			t.Fatalf("refreshLoop() did not make attempt %d", wantAttempt)
		}
	}
	deadline := time.Now().Add(time.Second)
	for instance.metadata.Load() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	got := instance.metadata.Load()
	if got == nil || got.RDMAFabric == nil || !got.RDMAFabric.IPv6 || got.RDMAFabric.Planes != 8 {
		t.Fatalf("refreshLoop() stored %#v, want IPv6 with 8 planes", got)
	}
}

func TestHasFabricInterface(t *testing.T) {
	originalSysClassNet := sysClassNet
	sysClassNet = t.TempDir()
	t.Cleanup(func() { sysClassNet = originalSysClassNet })

	if err := os.Mkdir(filepath.Join(sysClassNet, "eth0"), 0o755); err != nil {
		t.Fatalf("Mkdir() returned error: %v", err)
	}
	if hasFabricInterface() {
		t.Fatal("hasFabricInterface() = true without an rdmaN interface")
	}
	if err := os.Mkdir(filepath.Join(sysClassNet, "rdma3"), 0o755); err != nil {
		t.Fatalf("Mkdir() returned error: %v", err)
	}
	if !hasFabricInterface() {
		t.Fatal("hasFabricInterface() = false with rdma3 present")
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
	ipv4Instance := newOKEInstance(&okeMetadata{RDMAFabric: &RDMAFabric{IPv6: false, Planes: 0}}, nil)

	tests := []struct {
		name     string
		instance *OKEInstance
		id       cloudprovider.DeviceIdentifiers
		want     *apis.NetworkConfig
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
		},
		{
			name:     "IPv6 fabric uses address-free ipvlan",
			instance: newOKEInstance(&okeMetadata{RDMAFabric: &RDMAFabric{IPv6: true, Planes: 8}}, nil),
			id:       cloudprovider.DeviceIdentifiers{Name: "pci-0000-0c-00-0", PCIAddress: "0000:0c:00.0"},
			want:     &apis.NetworkConfig{SubInterface: &apis.SubInterfaceConfig{Type: apis.SubInterfaceTypeIPVlan}},
		},
		{
			name:     "missing fabric data keeps a failing profile",
			instance: newOKEInstance(nil, nil),
			id:       cloudprovider.DeviceIdentifiers{Name: "pci-0000-0c-00-0", PCIAddress: "0000:0c:00.0"},
			want: &apis.NetworkConfig{
				Profile:      okeRDMAIPv4Profile,
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
			name:     "invalid ARP value keeps ipvlan",
			instance: ipv4Instance,
			id:       cloudprovider.DeviceIdentifiers{Name: "pci-0000-0e-00-0", PCIAddress: "0000:0e:00.0"},
			want: &apis.NetworkConfig{
				Profile:      okeRDMAIPv4Profile,
				Interface:    apis.InterfaceConfig{ARPAnnounce: ptr.To[int32](2)},
				SubInterface: &apis.SubInterfaceConfig{Type: apis.SubInterfaceTypeIPVlan},
			},
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
		})
	}
}

func TestGetProfileConfig(t *testing.T) {
	originalSysfs := sysfsPCIDevices
	originalInterfaceAddresses := interfaceAddresses
	sysfsPCIDevices = t.TempDir()
	t.Cleanup(func() {
		sysfsPCIDevices = originalSysfs
		interfaceAddresses = originalInterfaceAddresses
	})

	addDevice := func(pciAddress, ifName string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(sysfsPCIDevices, pciAddress, "net", ifName), 0o755); err != nil {
			t.Fatalf("failed to create fake sysfs: %v", err)
		}
	}
	addDevice("0000:0c:00.0", "rdma0")
	addDevice("0000:0d:00.0", "rdma15")
	addDevice("0000:0e:00.0", "rdma16")
	addDevice("0000:0f:00.0", "eth0")
	addDevice("0000:10:00.0", "rdma1")

	interfaceAddresses = func(ifName string) ([]net.Addr, error) {
		addresses := map[string]string{
			"rdma0":  "10.224.6.100/12",
			"rdma15": "10.225.230.100/12",
			"rdma16": "10.225.250.100/12",
			"eth0":   "10.140.70.100/19",
			"rdma1":  "10.226.6.100/12",
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
		name     string
		instance *OKEInstance
		id       cloudprovider.DeviceIdentifiers
		config   *apis.NetworkConfig
		want     *apis.NetworkConfig
		wantErr  bool
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
			name:   "rail index above tested B4 count",
			id:     cloudprovider.DeviceIdentifiers{Name: "pci-0000-0e-00-0", PCIAddress: "0000:0e:00.0"},
			config: baseConfig,
			want: &apis.NetworkConfig{
				SubInterface: &apis.SubInterfaceConfig{Addresses: []string{"10.241.250.100/15"}},
				Routes: []apis.RouteConfig{{
					Destination: "10.240.0.0/15",
					Source:      "10.241.250.100",
					Scope:       253,
					Table:       116,
				}},
				Rules: []apis.RuleConfig{{Priority: 32000, Source: "10.241.250.100/32", Table: 116}},
			},
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
