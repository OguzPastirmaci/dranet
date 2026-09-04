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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"golang.org/x/sys/unix"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/dranet/pkg/cloudprovider"
)

// fakeSysfs points the sysfs paths at a temporary directory for one test.
func fakeSysfs(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	originalClassNet, originalPCIDevices := sysClassNet, sysfsPCIDevices
	sysClassNet = filepath.Join(dir, "class", "net")
	sysfsPCIDevices = filepath.Join(dir, "bus", "pci", "devices")
	t.Cleanup(func() { sysClassNet, sysfsPCIDevices = originalClassNet, originalPCIDevices })
}

// fakeInterface adds an interface with the given ARPHRD type to the fake sysfs.
// A non-empty pciAddress also links the interface to that PCI device.
func fakeInterface(t *testing.T, ifName, pciAddress string, hardwareType int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(sysClassNet, ifName), 0o755); err != nil {
		t.Fatalf("MkdirAll() returned error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sysClassNet, ifName, "type"), []byte(strconv.Itoa(hardwareType)+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() returned error: %v", err)
	}
	if pciAddress == "" {
		return
	}
	if err := os.MkdirAll(filepath.Join(sysfsPCIDevices, pciAddress, "net", ifName), 0o755); err != nil {
		t.Fatalf("MkdirAll() returned error: %v", err)
	}
}

func TestGetDeviceAttributes(t *testing.T) {
	tests := []struct {
		name     string
		instance *OKEInstance
		id       cloudprovider.DeviceIdentifiers
		want     map[resourceapi.QualifiedName]resourceapi.DeviceAttribute
	}{
		{
			name: "full topology with gpu memory fabric and shape (GB200/GB300 shapes)",
			instance: newOKEInstance(&okeMetadata{
				HPCIslandId:     "fake-island-id",
				NetworkBlockId:  "fake-network-block-id",
				LocalBlockId:    "fake-local-block-id",
				RackId:          "fake-rack-id",
				GpuMemoryFabric: "fake-gpu-memory-fabric-id",
				Shape:           "BM.GPU.GB200.4",
			}, nil),
			id: cloudprovider.DeviceIdentifiers{Name: "dev1"},
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrOKEHPCIslandId:     {StringValue: ptr.To("fake-island-id")},
				AttrOKENetworkBlockId:  {StringValue: ptr.To("fake-network-block-id")},
				AttrOKELocalBlockId:    {StringValue: ptr.To("fake-local-block-id")},
				AttrOKERackId:          {StringValue: ptr.To("fake-rack-id")},
				AttrOKEGpuMemoryFabric: {StringValue: ptr.To("fake-gpu-memory-fabric-id")},
				AttrOKEShape:           {StringValue: ptr.To("BM.GPU.GB200.4")},
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
			name:     "no metadata",
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
			instance: newOKEInstance(&okeMetadata{RDMAFabric: &rdmaFabric{IPv6: false, Planes: 0}}, nil),
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrOKERDMAFabricIPv6:   {BoolValue: ptr.To(false)},
				AttrOKERDMAFabricPlanes: {IntValue: ptr.To[int64](0)},
			},
		},
		{
			name:     "IPv6 fabric with multiple planes",
			instance: newOKEInstance(&okeMetadata{RDMAFabric: &rdmaFabric{IPv6: true, Planes: 8}}, nil),
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrOKERDMAFabricIPv6:   {BoolValue: ptr.To(true)},
				AttrOKERDMAFabricPlanes: {IntValue: ptr.To[int64](8)},
			},
		},
		{
			name:     "unconfirmed fabric data is published",
			instance: newOKEInstance(&okeMetadata{RDMAFabric: &rdmaFabric{IPv6: true, Planes: 2}, FabricConfirmed: false}, nil),
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrOKERDMAFabricIPv6:   {BoolValue: ptr.To(true)},
				AttrOKERDMAFabricPlanes: {IntValue: ptr.To[int64](2)},
			},
		},
		{
			name:     "fabric data is absent",
			instance: newOKEInstance(&okeMetadata{RackId: "fake-rack-id"}, nil),
			want: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				AttrOKERackId: {StringValue: ptr.To("fake-rack-id")},
			},
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
	tests := []struct {
		name            string
		ifName          string
		pciAddress      string
		hardwareType    int
		alreadyRequired bool
		id              cloudprovider.DeviceIdentifiers
		wantRequired    bool
		wantWoken       bool
	}{
		{
			name: "device without a PCI address",
			id:   cloudprovider.DeviceIdentifiers{Name: "dev1"},
		},
		{
			name:         "Ethernet fabric interface requires fabric data",
			ifName:       "rdma0",
			pciAddress:   "0000:0c:00.0",
			hardwareType: unix.ARPHRD_ETHER,
			id:           cloudprovider.DeviceIdentifiers{Name: "rdma0", PCIAddress: "0000:0c:00.0"},
			wantRequired: true,
			wantWoken:    true,
		},
		{
			name:            "fabric data already required",
			ifName:          "rdma0",
			pciAddress:      "0000:0c:00.0",
			hardwareType:    unix.ARPHRD_ETHER,
			alreadyRequired: true,
			id:              cloudprovider.DeviceIdentifiers{Name: "rdma0", PCIAddress: "0000:0c:00.0"},
			wantRequired:    true,
		},
		{
			name:         "InfiniBand fabric interface does not require fabric data",
			ifName:       "rdma0",
			pciAddress:   "0000:0c:00.0",
			hardwareType: unix.ARPHRD_INFINIBAND,
			id:           cloudprovider.DeviceIdentifiers{Name: "rdma0", PCIAddress: "0000:0c:00.0"},
		},
		{
			name:         "Ethernet non-fabric interface does not require fabric data",
			ifName:       "eth0",
			pciAddress:   "0000:0c:00.0",
			hardwareType: unix.ARPHRD_ETHER,
			id:           cloudprovider.DeviceIdentifiers{Name: "eth0", PCIAddress: "0000:0c:00.0"},
		},
		{
			name:         "unknown PCI address",
			ifName:       "rdma0",
			pciAddress:   "0000:0c:00.0",
			hardwareType: unix.ARPHRD_ETHER,
			id:           cloudprovider.DeviceIdentifiers{Name: "rdma1", PCIAddress: "0000:0d:00.0"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeSysfs(t)
			if tt.ifName != "" {
				fakeInterface(t, tt.ifName, tt.pciAddress, tt.hardwareType)
			}
			instance := newOKEInstance(nil, nil)
			instance.requiresFabricData.Store(tt.alreadyRequired)

			if got := instance.GetDeviceConfig(tt.id); got != nil {
				t.Errorf("GetDeviceConfig() = %v, want nil", got)
			}
			if got := instance.requiresFabricData.Load(); got != tt.wantRequired {
				t.Errorf("requiresFabricData = %v, want %v", got, tt.wantRequired)
			}
			if woken := len(instance.refreshMetadataNow) == 1; woken != tt.wantWoken {
				t.Errorf("refresh loop woken = %v, want %v", woken, tt.wantWoken)
			}
		})
	}
}

func TestHasEthernetFabricInterface(t *testing.T) {
	fakeSysfs(t)

	if hasEthernetFabricInterface() {
		t.Fatal("hasEthernetFabricInterface() = true without a sysfs directory")
	}
	fakeInterface(t, "eth0", "", unix.ARPHRD_ETHER)
	if hasEthernetFabricInterface() {
		t.Fatal("hasEthernetFabricInterface() = true without an rdmaN interface")
	}
	fakeInterface(t, "rdma3", "", unix.ARPHRD_INFINIBAND)
	if hasEthernetFabricInterface() {
		t.Fatal("hasEthernetFabricInterface() = true with only native InfiniBand")
	}
	fakeInterface(t, "rdma4", "", unix.ARPHRD_ETHER)
	if !hasEthernetFabricInterface() {
		t.Fatal("hasEthernetFabricInterface() = false with an Ethernet fabric interface")
	}
}

func TestIsFabricInterface(t *testing.T) {
	tests := []struct {
		ifName string
		want   bool
	}{
		{ifName: "rdma0", want: true},
		{ifName: "rdma15", want: true},
		{ifName: "rdma", want: false},
		{ifName: "rdmax", want: false},
		{ifName: "rdma1x", want: false},
		{ifName: "eth0", want: false},
		{ifName: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.ifName, func(t *testing.T) {
			if got := isFabricInterface(tt.ifName); got != tt.want {
				t.Errorf("isFabricInterface(%q) = %v, want %v", tt.ifName, got, tt.want)
			}
		})
	}
}

func TestQueryIMDS(t *testing.T) {
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

	var got imdsRDMAFabricData
	if err := queryIMDS(context.Background(), server.Client(), server.URL+"/opc/v2/host/rdmaFabricData/", &got); err != nil {
		t.Fatalf("queryIMDS() returned error: %v", err)
	}
	if got.IPv6 == nil || !*got.IPv6 || got.Planes == nil || *got.Planes != 8 {
		t.Errorf("queryIMDS() = %#v, want IPv6 with 8 planes", got)
	}
}

func TestQueryIMDSRejectsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	var got imdsRDMAFabricData
	if err := queryIMDS(context.Background(), server.Client(), server.URL, &got); err == nil {
		t.Fatal("queryIMDS() returned no error")
	}
}

func TestParseRDMAFabricDataRequiresAllFields(t *testing.T) {
	tests := []struct {
		name    string
		data    *imdsRDMAFabricData
		want    *rdmaFabric
		wantErr bool
	}{
		{
			name: "absent object",
			data: nil,
			want: nil,
		},
		{
			name: "IPv4 with zero planes",
			data: &imdsRDMAFabricData{IPv6: ptr.To(false), Planes: ptr.To[int64](0)},
			want: &rdmaFabric{IPv6: false, Planes: 0},
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

func TestFetchOKEMetadataFabricFallback(t *testing.T) {
	tests := []struct {
		name               string
		hostResponse       string
		directResponse     string
		directStatus       int
		requiresFabricData bool
		wantFabric         *rdmaFabric
		wantDirectRequest  bool
	}{
		{
			name:               "direct endpoint is preferred",
			hostResponse:       `{"networkBlockId":"network-1","rackId":"rack-1","rdmaFabricData":{"ipv6":true,"planes":8}}`,
			directResponse:     `{"ipv6":false,"planes":0}`,
			directStatus:       http.StatusOK,
			requiresFabricData: true,
			wantFabric:         &rdmaFabric{IPv6: false, Planes: 0},
			wantDirectRequest:  true,
		},
		{
			name:               "embedded data is used when direct endpoint is absent",
			hostResponse:       `{"networkBlockId":"network-1","rackId":"rack-1","rdmaFabricData":{"ipv6":false,"planes":4}}`,
			directStatus:       http.StatusNotFound,
			requiresFabricData: true,
			wantFabric:         &rdmaFabric{IPv6: false, Planes: 4},
			wantDirectRequest:  true,
		},
		{
			name:               "topology is kept when direct and embedded data are absent",
			hostResponse:       `{"networkBlockId":"network-1","rackId":"rack-1"}`,
			directStatus:       http.StatusNotFound,
			requiresFabricData: true,
			wantDirectRequest:  true,
		},
		{
			name:               "embedded data is used when direct data is incomplete",
			hostResponse:       `{"networkBlockId":"network-1","rackId":"rack-1","rdmaFabricData":{"ipv6":true,"planes":8}}`,
			directResponse:     `{"ipv6":false}`,
			directStatus:       http.StatusOK,
			requiresFabricData: true,
			wantFabric:         &rdmaFabric{IPv6: true, Planes: 8},
			wantDirectRequest:  true,
		},
		{
			name:               "topology is kept when both fabric responses are incomplete",
			hostResponse:       `{"networkBlockId":"network-1","rackId":"rack-1","rdmaFabricData":{"ipv6":false}}`,
			directResponse:     `{"planes":8}`,
			directStatus:       http.StatusOK,
			requiresFabricData: true,
			wantDirectRequest:  true,
		},
		{
			name:           "non-fabric node uses embedded data without the direct endpoint",
			hostResponse:   `{"networkBlockId":"network-1","rackId":"rack-1","rdmaFabricData":{"ipv6":true,"planes":2}}`,
			directResponse: `{"ipv6":false,"planes":0}`,
			directStatus:   http.StatusOK,
			wantFabric:     &rdmaFabric{IPv6: true, Planes: 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var directRequested atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer Oracle" {
					http.Error(w, "missing authorization", http.StatusUnauthorized)
					return
				}
				switch r.URL.Path {
				case "/host/":
					_, _ = w.Write([]byte(tt.hostResponse))
				case "/instance/":
					_, _ = w.Write([]byte(`{"shape":"BM.GPU.GB200.4"}`))
				case "/host/rdmaFabricData/":
					directRequested.Store(true)
					w.WriteHeader(tt.directStatus)
					_, _ = w.Write([]byte(tt.directResponse))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			got, err := fetchOKEMetadata(context.Background(), server.Client(), server.URL, tt.requiresFabricData)
			if err != nil {
				t.Fatalf("fetchOKEMetadata() returned error: %v", err)
			}
			want := &okeMetadata{
				NetworkBlockId:  "network-1",
				RackId:          "rack-1",
				Shape:           "BM.GPU.GB200.4",
				RDMAFabric:      tt.wantFabric,
				FabricConfirmed: tt.requiresFabricData && tt.wantFabric != nil,
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("fetchOKEMetadata() mismatch (-want +got):\n%s", diff)
			}
			if directRequested.Load() != tt.wantDirectRequest {
				t.Errorf("direct endpoint requested = %v, want %v", directRequested.Load(), tt.wantDirectRequest)
			}
		})
	}
}

func TestFetchOKEMetadataFullTopology(t *testing.T) {
	const (
		islandOCID  = "ocid1.hpcisland.oc1.test-region-1.aaaaaaaa2mvjha24vj6evyafdqtis6nzqibhrnxxhzt65zkc3upy4xlrz5za"
		networkOCID = "ocid1.networkblock.oc1.test-region-1.bbbbbbbb2mvjha24vj6evyafdqtis6nzqibhrnxxhzt65zkc3upy4xlrz5za"
		localOCID   = "ocid1.localblock.oc1.test-region-1.cccccccc2mvjha24vj6evyafdqtis6nzqibhrnxxhzt65zkc3upy4xlrz5za"
		fabricOCID  = "ocid1.computegpumemoryfabric.oc1.test-region-1.dddddddd2mvjha24vj6evyafdqtis6nzqibhrnxxhzt65zkc3upy4xlrz5za"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/host/":
			_, _ = fmt.Fprintf(w, `{"networkBlockId":"top-level-network","rackId":"rack-1","rdmaTopologyData":{"customerHPCIslandId":%q,"customerNetworkBlock":%q,"customerLocalBlock":%q,"customerGpuMemoryFabric":%q,"customerHostId":"ocid1.host.oc1..host"},"rdmaFabricData":{"ipv6":true,"planes":2}}`, islandOCID, networkOCID, localOCID, fabricOCID)
		case "/host/rdmaFabricData/":
			_, _ = w.Write([]byte(`{"ipv6":true,"planes":2}`))
		case "/instance/":
			_, _ = w.Write([]byte(`{"shape":"BM.GPU.GB300.4"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	metadata, err := fetchOKEMetadata(context.Background(), server.Client(), server.URL, true)
	if err != nil {
		t.Fatalf("fetchOKEMetadata() returned error: %v", err)
	}
	if !metadata.FabricConfirmed {
		t.Error("fetchOKEMetadata() did not confirm fabric data fetched with the gate on")
	}

	suffix := func(ocid string) *string {
		return ptr.To(ocid[strings.LastIndex(ocid, ".")+1:])
	}
	// The topology block overrides the top-level networkBlockId.
	want := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		AttrOKEHPCIslandId:      {StringValue: suffix(islandOCID)},
		AttrOKENetworkBlockId:   {StringValue: suffix(networkOCID)},
		AttrOKELocalBlockId:     {StringValue: suffix(localOCID)},
		AttrOKERackId:           {StringValue: ptr.To("rack-1")},
		AttrOKEGpuMemoryFabric:  {StringValue: suffix(fabricOCID)},
		AttrOKEShape:            {StringValue: ptr.To("BM.GPU.GB300.4")},
		AttrOKERDMAFabricIPv6:   {BoolValue: ptr.To(true)},
		AttrOKERDMAFabricPlanes: {IntValue: ptr.To[int64](2)},
	}
	got := newOKEInstance(metadata, nil).GetDeviceAttributes(cloudprovider.DeviceIdentifiers{Name: "dev1"})
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("GetDeviceAttributes() mismatch (-want +got):\n%s", diff)
	}
}

func TestFetchOKEMetadataKeepsTopologyWithoutInstanceMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/host/":
			_, _ = w.Write([]byte(`{"networkBlockId":"network-1","rackId":"rack-1"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	got, err := fetchOKEMetadata(context.Background(), server.Client(), server.URL, false)
	if err != nil {
		t.Fatalf("fetchOKEMetadata() returned error: %v", err)
	}
	want := &okeMetadata{NetworkBlockId: "network-1", RackId: "rack-1"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("fetchOKEMetadata() mismatch (-want +got):\n%s", diff)
	}
}

func TestFetchOKEMetadataQueriesRequiredFabricBeforeOptionalMetadata(t *testing.T) {
	var fabricRequested atomic.Bool
	var optionalRequestedFirst atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/host/":
			_, _ = w.Write([]byte(`{}`))
		case "/host/rdmaFabricData/":
			fabricRequested.Store(true)
			_, _ = w.Write([]byte(`{"ipv6":true,"planes":8}`))
		case "/instance/":
			if !fabricRequested.Load() {
				optionalRequestedFirst.Store(true)
			}
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	if _, err := fetchOKEMetadata(context.Background(), server.Client(), server.URL, true); err != nil {
		t.Fatalf("fetchOKEMetadata() returned error: %v", err)
	}
	if optionalRequestedFirst.Load() {
		t.Fatal("fetchOKEMetadata() queried optional metadata before required fabric data")
	}
}

func TestRefreshMetadataMergesPartialDataAndPinsFabric(t *testing.T) {
	instance := newOKEInstance(&okeMetadata{
		HPCIslandId:     "island-1",
		NetworkBlockId:  "network-1",
		Shape:           "BM.GPU.GB200.4",
		RDMAFabric:      &rdmaFabric{IPv6: false, Planes: 0},
		FabricConfirmed: true,
	}, func(context.Context) (*okeMetadata, error) {
		return &okeMetadata{
			RackId:          "rack-1",
			RDMAFabric:      &rdmaFabric{IPv6: true, Planes: 8},
			FabricConfirmed: true,
		}, nil
	})

	if err := instance.refreshMetadata(context.Background()); err != nil {
		t.Fatalf("refreshMetadata() returned error: %v", err)
	}
	want := &okeMetadata{
		HPCIslandId:     "island-1",
		NetworkBlockId:  "network-1",
		RackId:          "rack-1",
		Shape:           "BM.GPU.GB200.4",
		RDMAFabric:      &rdmaFabric{IPv6: false, Planes: 0},
		FabricConfirmed: true,
	}
	if diff := cmp.Diff(want, instance.metadata.Load()); diff != "" {
		t.Errorf("refreshMetadata() mismatch (-want +got):\n%s", diff)
	}
	if !instance.fabricDriftLogged.Load() {
		t.Error("refreshMetadata() did not report RDMA fabric drift")
	}
}

func TestRefreshMetadataReplacesPreliminaryFabricData(t *testing.T) {
	instance := newOKEInstance(&okeMetadata{
		RDMAFabric: &rdmaFabric{IPv6: false, Planes: 0},
	}, func(context.Context) (*okeMetadata, error) {
		return &okeMetadata{
			RDMAFabric:      &rdmaFabric{IPv6: true, Planes: 8},
			FabricConfirmed: true,
		}, nil
	})

	if err := instance.refreshMetadata(context.Background()); err != nil {
		t.Fatalf("refreshMetadata() returned error: %v", err)
	}
	got := instance.metadata.Load()
	if got.RDMAFabric == nil || !got.RDMAFabric.IPv6 || !got.FabricConfirmed {
		t.Fatalf("refreshMetadata() stored %#v, want confirmed IPv6 fabric data", got)
	}
	if instance.fabricDriftLogged.Load() {
		t.Error("refreshMetadata() reported expected preliminary fabric replacement as drift")
	}
}

func TestRefreshMetadataConfirmsIdenticalFabricData(t *testing.T) {
	instance := newOKEInstance(&okeMetadata{
		RDMAFabric: &rdmaFabric{IPv6: false, Planes: 0},
	}, func(context.Context) (*okeMetadata, error) {
		return &okeMetadata{
			RDMAFabric:      &rdmaFabric{IPv6: false, Planes: 0},
			FabricConfirmed: true,
		}, nil
	})
	instance.requiresFabricData.Store(true)
	if instance.metadataComplete() {
		t.Fatal("metadataComplete() = true before the fabric data was confirmed")
	}

	if err := instance.refreshMetadata(context.Background()); err != nil {
		t.Fatalf("refreshMetadata() returned error: %v", err)
	}
	want := &okeMetadata{
		RDMAFabric:      &rdmaFabric{IPv6: false, Planes: 0},
		FabricConfirmed: true,
	}
	if diff := cmp.Diff(want, instance.metadata.Load()); diff != "" {
		t.Errorf("refreshMetadata() mismatch (-want +got):\n%s", diff)
	}
	if !instance.metadataComplete() {
		t.Error("metadataComplete() = false after identical fabric data was confirmed")
	}
	if instance.fabricDriftLogged.Load() {
		t.Error("refreshMetadata() reported drift for identical fabric data")
	}
}

func TestRefreshMetadataKeepsFabricDataWhenNextIsEmpty(t *testing.T) {
	instance := newOKEInstance(&okeMetadata{
		RDMAFabric:      &rdmaFabric{IPv6: false, Planes: 0},
		FabricConfirmed: true,
	}, func(context.Context) (*okeMetadata, error) {
		return &okeMetadata{RackId: "rack-1"}, nil
	})

	if err := instance.refreshMetadata(context.Background()); err != nil {
		t.Fatalf("refreshMetadata() returned error: %v", err)
	}
	want := &okeMetadata{
		RackId:          "rack-1",
		RDMAFabric:      &rdmaFabric{IPv6: false, Planes: 0},
		FabricConfirmed: true,
	}
	if diff := cmp.Diff(want, instance.metadata.Load()); diff != "" {
		t.Errorf("refreshMetadata() mismatch (-want +got):\n%s", diff)
	}
	if instance.fabricDriftLogged.Load() {
		t.Error("refreshMetadata() reported drift for missing fabric data")
	}
}

func TestRefreshMetadataAcceptsAttributeChanges(t *testing.T) {
	instance := newOKEInstance(&okeMetadata{
		HPCIslandId:    "island-1",
		NetworkBlockId: "network-1",
		LocalBlockId:   "local-1",
		RackId:         "rack-1",
		Shape:          "BM.GPU.GB200.4",
	}, func(context.Context) (*okeMetadata, error) {
		return &okeMetadata{
			HPCIslandId:    "island-2",
			NetworkBlockId: "network-2",
			LocalBlockId:   "local-2",
			RackId:         "rack-2",
			Shape:          "BM.GPU.GB300.4",
		}, nil
	})

	if err := instance.refreshMetadata(context.Background()); err != nil {
		t.Fatalf("refreshMetadata() returned error: %v", err)
	}
	want := &okeMetadata{
		HPCIslandId:    "island-2",
		NetworkBlockId: "network-2",
		LocalBlockId:   "local-2",
		RackId:         "rack-2",
		Shape:          "BM.GPU.GB300.4",
	}
	if diff := cmp.Diff(want, instance.metadata.Load()); diff != "" {
		t.Errorf("refreshMetadata() mismatch (-want +got):\n%s", diff)
	}
	if !instance.metadataDriftLogged.Load() {
		t.Error("refreshMetadata() did not report attribute drift")
	}
}

func TestRefreshMetadataKeepsLastKnownDataOnFailure(t *testing.T) {
	want := &okeMetadata{
		NetworkBlockId: "network-1",
		RDMAFabric:     &rdmaFabric{IPv6: false, Planes: 0},
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

func TestRefreshLoopRetriesIncompleteFabricData(t *testing.T) {
	attempts := make(chan int, 2)
	attempt := 0
	instance := newOKEInstance(nil, func(context.Context) (*okeMetadata, error) {
		attempt++
		attempts <- attempt
		if attempt == 1 {
			return &okeMetadata{NetworkBlockId: "network-1"}, nil
		}
		return &okeMetadata{RDMAFabric: &rdmaFabric{IPv6: true, Planes: 8}, FabricConfirmed: true}, nil
	})
	instance.requiresFabricData.Store(true)
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
	for !instance.metadataComplete() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	got := instance.metadata.Load()
	if got == nil || got.NetworkBlockId != "network-1" || got.RDMAFabric == nil || !got.RDMAFabric.IPv6 || got.RDMAFabric.Planes != 8 {
		t.Fatalf("refreshLoop() stored %#v, want IPv6 with 8 planes", got)
	}
}

func TestRefreshLoopWakesForLateFabricData(t *testing.T) {
	attempted := make(chan bool, 1)
	var instance *OKEInstance
	instance = newOKEInstance(&okeMetadata{NetworkBlockId: "network-1"}, func(context.Context) (*okeMetadata, error) {
		attempted <- instance.requiresFabricData.Load()
		return &okeMetadata{
			RDMAFabric:      &rdmaFabric{IPv6: false, Planes: 0},
			FabricConfirmed: true,
		}, nil
	})
	instance.refreshInterval = time.Hour
	instance.requestTimeout = time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go instance.refreshLoop(ctx)

	instance.requireFabricData()
	select {
	case required := <-attempted:
		if !required {
			t.Fatal("refreshLoop() fetched metadata without requiring fabric data")
		}
	case <-time.After(time.Second):
		t.Fatal("refreshLoop() did not wake for a late fabric interface")
	}

	deadline := time.Now().Add(time.Second)
	for !instance.metadataComplete() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !instance.metadataComplete() {
		t.Fatal("refreshLoop() did not store complete fabric data")
	}
}

func TestRequireFabricDataRefreshesPreliminaryData(t *testing.T) {
	attempted := make(chan struct{}, 1)
	instance := newOKEInstance(&okeMetadata{
		RDMAFabric: &rdmaFabric{IPv6: false, Planes: 0},
	}, func(context.Context) (*okeMetadata, error) {
		attempted <- struct{}{}
		return &okeMetadata{
			RDMAFabric:      &rdmaFabric{IPv6: true, Planes: 8},
			FabricConfirmed: true,
		}, nil
	})
	instance.refreshInterval = time.Hour
	instance.requestTimeout = time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go instance.refreshLoop(ctx)

	instance.requireFabricData()
	select {
	case <-attempted:
	case <-time.After(time.Second):
		t.Fatal("requireFabricData() did not refresh preliminary fabric data")
	}
}

func TestRefreshLoopStopsOnContextCancel(t *testing.T) {
	instance := newOKEInstance(&okeMetadata{RackId: "rack-1"}, func(context.Context) (*okeMetadata, error) {
		return &okeMetadata{RackId: "rack-1"}, nil
	})
	instance.refreshInterval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		instance.refreshLoop(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("refreshLoop() did not stop after the context was cancelled")
	}
}

func TestNextRetryInterval(t *testing.T) {
	instance := newOKEInstance(nil, nil)
	tests := []struct {
		name      string
		current   time.Duration
		succeeded bool
		want      time.Duration
	}{
		{name: "doubles after a failed or incomplete refresh", current: 10 * time.Second, succeeded: false, want: 20 * time.Second},
		{name: "caps at the maximum", current: 10 * time.Minute, succeeded: false, want: 15 * time.Minute},
		{name: "stays at the maximum", current: 15 * time.Minute, succeeded: false, want: 15 * time.Minute},
		{name: "resets after a successful complete refresh", current: 15 * time.Minute, succeeded: true, want: 10 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := instance.nextRetryInterval(tt.current, tt.succeeded); got != tt.want {
				t.Errorf("nextRetryInterval(%s, %v) = %s, want %s", tt.current, tt.succeeded, got, tt.want)
			}
		})
	}
}

func TestRefreshLoopRetriesFailedRefreshOfCompleteMetadata(t *testing.T) {
	attempts := make(chan int, 3)
	attempt := 0
	instance := newOKEInstance(&okeMetadata{RackId: "rack-1"}, func(context.Context) (*okeMetadata, error) {
		attempt++
		attempts <- attempt
		if attempt == 1 {
			return nil, errors.New("IMDS is not ready")
		}
		return &okeMetadata{RackId: "rack-1"}, nil
	})
	instance.retryInterval = time.Millisecond
	instance.maxRetryInterval = 2 * time.Millisecond
	instance.refreshInterval = time.Hour
	instance.requestTimeout = time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go instance.refreshLoop(ctx)

	// Wake the loop instead of waiting for the hourly refresh. The first
	// refresh fails, so the second must follow after the retry interval.
	instance.refreshMetadataNow <- struct{}{}
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
	// The successful refresh puts the loop back on the hourly interval.
	select {
	case gotAttempt := <-attempts:
		t.Fatalf("refreshLoop() made attempt %d after a successful refresh", gotAttempt)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestStartReturnsCompleteMetadata(t *testing.T) {
	fakeSysfs(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/host/":
			_, _ = w.Write([]byte(`{"networkBlockId":"network-1","rackId":"rack-1","rdmaFabricData":{"ipv6":true,"planes":0}}`))
		case "/instance/":
			_, _ = w.Write([]byte(`{"shape":"BM.GPU.GB300.4"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	instance := newOKEInstance(nil, nil)
	instance.initialRetryInterval = time.Millisecond
	instance.initialWait = 2 * time.Second
	instance.refreshInterval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startedAt := time.Now()
	got, err := instance.start(ctx, server.Client(), server.URL)
	if err != nil {
		t.Fatalf("start() returned error: %v", err)
	}
	if got != instance {
		t.Fatal("start() did not return the instance")
	}
	if elapsed := time.Since(startedAt); elapsed >= instance.initialWait {
		t.Fatalf("start() waited %s for complete metadata", elapsed)
	}
	if instance.requiresFabricData.Load() {
		t.Fatal("start() required fabric data without an Ethernet fabric interface")
	}
	// Without the gate the embedded fabric data is published but not confirmed.
	want := &okeMetadata{
		NetworkBlockId: "network-1",
		RackId:         "rack-1",
		Shape:          "BM.GPU.GB300.4",
		RDMAFabric:     &rdmaFabric{IPv6: true, Planes: 0},
	}
	if diff := cmp.Diff(want, instance.metadata.Load()); diff != "" {
		t.Errorf("start() mismatch (-want +got):\n%s", diff)
	}
}

func TestStartReturnsPartialMetadataAfterStartupWindow(t *testing.T) {
	fakeSysfs(t)
	fakeInterface(t, "rdma0", "", unix.ARPHRD_ETHER)
	var hostRequests atomic.Int64
	var directRequested atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/host/":
			hostRequests.Add(1)
			_, _ = w.Write([]byte(`{"networkBlockId":"network-1","rackId":"rack-1"}`))
		case "/host/rdmaFabricData/":
			directRequested.Store(true)
			http.NotFound(w, r)
		case "/instance/":
			_, _ = w.Write([]byte(`{"shape":"BM.GPU.B4.8"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	instance := newOKEInstance(nil, nil)
	instance.initialRetryInterval = time.Millisecond
	instance.initialWait = 200 * time.Millisecond
	instance.retryInterval = time.Hour
	instance.maxRetryInterval = time.Hour
	instance.refreshInterval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := instance.start(ctx, server.Client(), server.URL); err != nil {
		t.Fatalf("start() returned error: %v", err)
	}
	if !instance.requiresFabricData.Load() {
		t.Fatal("start() did not require fabric data for an Ethernet fabric interface")
	}
	if !directRequested.Load() {
		t.Fatal("start() did not query the direct RDMA fabric endpoint")
	}
	// Incomplete metadata must keep start() polling for the whole window.
	if got := hostRequests.Load(); got < 2 {
		t.Fatalf("start() made %d host requests before giving up, want at least 2", got)
	}
	if instance.metadataComplete() {
		t.Fatal("start() reported complete metadata without fabric data")
	}
	want := &okeMetadata{NetworkBlockId: "network-1", RackId: "rack-1", Shape: "BM.GPU.B4.8"}
	if diff := cmp.Diff(want, instance.metadata.Load()); diff != "" {
		t.Errorf("start() mismatch (-want +got):\n%s", diff)
	}
}

func TestStartReturnsErrorWhenContextIsCancelled(t *testing.T) {
	fakeSysfs(t)
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	instance := newOKEInstance(nil, nil)
	instance.initialRetryInterval = time.Millisecond
	instance.initialWait = time.Second

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := instance.start(ctx, server.Client(), server.URL)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("start() returned error %v, want context.Canceled", err)
	}
	if got != nil {
		t.Fatalf("start() returned %v with an error, want nil", got)
	}
}

func TestStartRecoversFromHostOutage(t *testing.T) {
	fakeSysfs(t)
	var hostAvailable atomic.Bool
	var hostRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/host/" {
			hostRequests.Add(1)
		}
		if !hostAvailable.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/host/":
			_, _ = w.Write([]byte(`{"networkBlockId":"network-1","rackId":"rack-1"}`))
		case "/instance/":
			_, _ = w.Write([]byte(`{"shape":"VM.Standard.E5.Flex"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	instance := newOKEInstance(nil, nil)
	instance.initialRetryInterval = time.Millisecond
	instance.initialWait = 20 * time.Millisecond
	instance.retryInterval = time.Millisecond
	instance.maxRetryInterval = 2 * time.Millisecond
	instance.refreshInterval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := instance.start(ctx, server.Client(), server.URL); err != nil {
		t.Fatalf("start() returned error: %v", err)
	}
	if instance.metadata.Load() != nil {
		t.Fatal("start() stored metadata while the host endpoint was unavailable")
	}
	if got := instance.GetDeviceAttributes(cloudprovider.DeviceIdentifiers{Name: "dev1"}); len(got) != 0 {
		t.Fatalf("GetDeviceAttributes() = %v before recovery, want none", got)
	}
	// A failing endpoint must keep start() polling for the whole window.
	if got := hostRequests.Load(); got < 2 {
		t.Fatalf("start() made %d host requests before giving up, want at least 2", got)
	}

	// The background retries pick the metadata up once the endpoint recovers.
	hostAvailable.Store(true)
	deadline := time.Now().Add(time.Second)
	for instance.metadata.Load() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	want := &okeMetadata{NetworkBlockId: "network-1", RackId: "rack-1", Shape: "VM.Standard.E5.Flex"}
	if diff := cmp.Diff(want, instance.metadata.Load()); diff != "" {
		t.Errorf("metadata after recovery mismatch (-want +got):\n%s", diff)
	}
}

func TestRefreshLoopRefreshesCompleteMetadataPeriodically(t *testing.T) {
	fetched := make(chan struct{}, 1)
	instance := newOKEInstance(&okeMetadata{RackId: "rack-1"}, func(context.Context) (*okeMetadata, error) {
		select {
		case fetched <- struct{}{}:
		default:
		}
		return &okeMetadata{RackId: "rack-1"}, nil
	})
	// A loop stuck on the retry path would never make the second fetch.
	instance.retryInterval = time.Hour
	instance.maxRetryInterval = time.Hour
	instance.refreshInterval = time.Millisecond
	instance.requestTimeout = time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go instance.refreshLoop(ctx)

	for i := 1; i <= 2; i++ {
		select {
		case <-fetched:
		case <-time.After(time.Second):
			t.Fatalf("refreshLoop() did not run periodic refresh %d", i)
		}
	}
}

func TestGetDeviceAttributesDuringRefresh(t *testing.T) {
	var fetches atomic.Int64
	instance := newOKEInstance(nil, func(context.Context) (*okeMetadata, error) {
		n := fetches.Add(1)
		return &okeMetadata{
			RackId:     fmt.Sprintf("rack-%d", n),
			RDMAFabric: &rdmaFabric{IPv6: n%2 == 0, Planes: n},
		}, nil
	})
	instance.retryInterval = time.Millisecond
	instance.maxRetryInterval = time.Millisecond
	instance.refreshInterval = time.Millisecond
	instance.requestTimeout = time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go instance.refreshLoop(ctx)

	// waitForSnapshot returns once the stored snapshot differs from previous.
	waitForSnapshot := func(previous *okeMetadata) *okeMetadata {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if current := instance.metadata.Load(); current != previous {
				return current
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("refreshLoop() did not store a new snapshot")
		return nil
	}
	waitForSnapshot(nil)

	// Read while the loop keeps storing new snapshots. Every read must see a
	// snapshot, and every value in a read must come from the same snapshot.
	stop := make(chan struct{})
	observed := make([]atomic.Int64, 4)
	var wg sync.WaitGroup
	for i := range observed {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				attributes := instance.GetDeviceAttributes(cloudprovider.DeviceIdentifiers{Name: "dev1"})
				rack, ok := attributes[AttrOKERackId]
				if !ok {
					t.Errorf("GetDeviceAttributes() returned no rack after the first snapshot")
					return
				}
				planes := attributes[AttrOKERDMAFabricPlanes]
				if want := fmt.Sprintf("rack-%d", *planes.IntValue); *rack.StringValue != want {
					t.Errorf("GetDeviceAttributes() mixed snapshots: rack %q with planes %d", *rack.StringValue, *planes.IntValue)
					return
				}
				observed[i].Add(1)
			}
		}()
	}
	// Wait until every reader has observed a snapshot, then keep them active
	// across at least one more stored snapshot.
	allObserved := func() bool {
		for i := range observed {
			if observed[i].Load() == 0 {
				return false
			}
		}
		return true
	}
	deadline := time.Now().Add(time.Second)
	for !allObserved() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	waitForSnapshot(instance.metadata.Load())
	close(stop)
	wg.Wait()
	for i := range observed {
		if observed[i].Load() == 0 {
			t.Errorf("reader %d observed no snapshot", i)
		}
	}
}

func TestMetadataComplete(t *testing.T) {
	tests := []struct {
		name     string
		metadata *okeMetadata
		requires bool
		want     bool
	}{
		{name: "no metadata", metadata: nil, requires: false, want: false},
		{name: "fabric data not required", metadata: &okeMetadata{}, requires: false, want: true},
		{name: "missing fabric data", metadata: &okeMetadata{}, requires: true, want: false},
		{name: "preliminary fabric data", metadata: &okeMetadata{RDMAFabric: &rdmaFabric{IPv6: true, Planes: 8}}, requires: true, want: false},
		{name: "confirmed fabric data", metadata: &okeMetadata{RDMAFabric: &rdmaFabric{IPv6: false, Planes: 0}, FabricConfirmed: true}, requires: true, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := newOKEInstance(tt.metadata, nil)
			instance.requiresFabricData.Store(tt.requires)
			if got := instance.metadataComplete(); got != tt.want {
				t.Errorf("metadataComplete() = %v, want %v", got, tt.want)
			}
		})
	}
}
