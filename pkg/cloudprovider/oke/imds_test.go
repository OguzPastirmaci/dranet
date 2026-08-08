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
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"k8s.io/utils/ptr"
)

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

func TestFetchOKEMetadataFabricFallback(t *testing.T) {
	tests := []struct {
		name               string
		hostResponse       string
		directResponse     string
		directStatus       int
		requiresFabricData bool
		wantFabric         *RDMAFabric
		wantDirectRequest  bool
	}{
		{
			name:               "direct endpoint is preferred",
			hostResponse:       `{"networkBlockId":"network-1","rackId":"rack-1","rdmaFabricData":{"ipv6":true,"planes":8}}`,
			directResponse:     `{"ipv6":false,"planes":0}`,
			directStatus:       http.StatusOK,
			requiresFabricData: true,
			wantFabric:         &RDMAFabric{IPv6: false, Planes: 0},
			wantDirectRequest:  true,
		},
		{
			name:               "embedded data is used when direct endpoint is absent",
			hostResponse:       `{"networkBlockId":"network-1","rackId":"rack-1","rdmaFabricData":{"ipv6":false,"planes":4}}`,
			directStatus:       http.StatusNotFound,
			requiresFabricData: true,
			wantFabric:         &RDMAFabric{IPv6: false, Planes: 4},
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
			wantFabric:         &RDMAFabric{IPv6: true, Planes: 8},
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
			name:           "non-fabric node uses embedded data",
			hostResponse:   `{"networkBlockId":"network-1","rackId":"rack-1","rdmaFabricData":{"ipv6":true,"planes":2}}`,
			directResponse: `{"ipv6":false,"planes":0}`,
			directStatus:   http.StatusOK,
			wantFabric:     &RDMAFabric{IPv6: true, Planes: 2},
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
				NetworkBlockId: "network-1",
				RackId:         "rack-1",
				RDMAFabric:     tt.wantFabric,
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

func TestRefreshLoopRetriesIncompleteFabricData(t *testing.T) {
	attempts := make(chan int, 2)
	attempt := 0
	instance := newOKEInstance(nil, func(context.Context) (*okeMetadata, error) {
		attempt++
		attempts <- attempt
		if attempt == 1 {
			return &okeMetadata{NetworkBlockId: "network-1"}, nil
		}
		return &okeMetadata{RDMAFabric: &RDMAFabric{IPv6: true, Planes: 8}}, nil
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
	for instance.metadata.Load() == nil && time.Now().Before(deadline) {
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
		return &okeMetadata{RDMAFabric: &RDMAFabric{IPv6: false, Planes: 0}}, nil
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
