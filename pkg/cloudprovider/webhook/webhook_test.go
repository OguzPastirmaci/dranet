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

package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
)

func TestWebhookCapabilitiesAndPost(t *testing.T) {
	ctx := context.Background()
	id := cloudprovider.DeviceIdentifiers{}

	tests := []struct {
		name                   string
		caps                   Capabilities
		expectCloudSuccess     bool
		expectProfileSuccess   bool
		expectLifecycleSuccess bool
	}{
		{
			name:                   "All capabilities enabled",
			caps:                   Capabilities{CloudProvider: true, ProfileProvider: true, DeviceLifecycleProvider: true},
			expectCloudSuccess:     true,
			expectProfileSuccess:   true,
			expectLifecycleSuccess: true,
		},
		{
			name:                 "Only CloudProvider enabled",
			caps:                 Capabilities{CloudProvider: true, ProfileProvider: false},
			expectCloudSuccess:   true,
			expectProfileSuccess: false,
		},
		{
			name:                 "Only ProfileProvider enabled",
			caps:                 Capabilities{CloudProvider: false, ProfileProvider: true},
			expectCloudSuccess:   false,
			expectProfileSuccess: true,
		},
		{
			name:                   "Only DeviceLifecycleProvider enabled",
			caps:                   Capabilities{DeviceLifecycleProvider: true},
			expectCloudSuccess:     false,
			expectProfileSuccess:   false,
			expectLifecycleSuccess: true,
		},
		{
			name:                 "All capabilities disabled",
			caps:                 Capabilities{},
			expectCloudSuccess:   false,
			expectProfileSuccess: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == PathHealth {
					json.NewEncoder(w).Encode(tt.caps)
					return
				}
				if r.URL.Path == PathGetDeviceAttributes {
					w.WriteHeader(http.StatusOK)
					w.Write([]byte(`{}`))
					return
				}
				if r.URL.Path == PathGetProfileConfig {
					w.WriteHeader(http.StatusOK)
					w.Write([]byte(`{}`))
					return
				}
				if r.URL.Path == PathPostAttachDevice || r.URL.Path == PathPreDetachDevice {
					w.WriteHeader(http.StatusOK)
					return
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			defer srv.Close()

			if !OnWebhook(ctx, srv.URL) {
				t.Errorf("OnWebhook failed to discover a healthy webhook")
			}

			provider, err := NewWebhookProvider(ctx, srv.URL)
			if err != nil {
				t.Fatalf("NewWebhookProvider failed: %v", err)
			}

			if provider.caps.CloudProvider != tt.caps.CloudProvider {
				t.Errorf("Expected CloudProvider capability to be %v", tt.caps.CloudProvider)
			}
			if provider.caps.ProfileProvider != tt.caps.ProfileProvider {
				t.Errorf("Expected ProfileProvider capability to be %v", tt.caps.ProfileProvider)
			}
			if provider.HasDeviceLifecycleProvider() != tt.caps.DeviceLifecycleProvider {
				t.Errorf("Expected DeviceLifecycleProvider capability to be %v", tt.caps.DeviceLifecycleProvider)
			}

			attrs := provider.GetDeviceAttributes(id)
			if tt.expectCloudSuccess && attrs == nil {
				t.Errorf("Expected GetDeviceAttributes to succeed, got nil")
			} else if !tt.expectCloudSuccess && attrs != nil {
				t.Errorf("Expected GetDeviceAttributes to fail and return nil due to lack of capability")
			}

			_, err = provider.GetProfileConfig(id, "claim-123", nil)
			if tt.expectProfileSuccess && err != nil {
				t.Errorf("Expected GetProfileConfig to succeed, got error: %v", err)
			} else if !tt.expectProfileSuccess && err == nil {
				t.Errorf("Expected GetProfileConfig to fail due to lack of capability")
			}

			err = provider.PostAttachDevice(ctx, cloudprovider.DeviceLifecycleRequest{})
			if tt.expectLifecycleSuccess && err != nil {
				t.Errorf("Expected PostAttachDevice to succeed, got error: %v", err)
			} else if !tt.expectLifecycleSuccess && err == nil {
				t.Errorf("Expected PostAttachDevice to fail due to lack of capability")
			}

			err = provider.PreDetachDevice(ctx, cloudprovider.DeviceLifecycleRequest{})
			if tt.expectLifecycleSuccess && err != nil {
				t.Errorf("Expected PreDetachDevice to succeed, got error: %v", err)
			} else if !tt.expectLifecycleSuccess && err == nil {
				t.Errorf("Expected PreDetachDevice to fail due to lack of capability")
			}
		})
	}
}

func TestWebhookDeviceLifecycleRequests(t *testing.T) {
	ctx := context.Background()
	received := make(chan struct {
		path    string
		request cloudprovider.DeviceLifecycleRequest
	}, 2)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == PathHealth {
			json.NewEncoder(w).Encode(Capabilities{DeviceLifecycleProvider: true})
			return
		}

		var request cloudprovider.DeviceLifecycleRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		received <- struct {
			path    string
			request cloudprovider.DeviceLifecycleRequest
		}{path: r.URL.Path, request: request}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	provider, err := NewWebhookProvider(ctx, srv.URL)
	if err != nil {
		t.Fatalf("NewWebhookProvider failed: %v", err)
	}

	request := cloudprovider.DeviceLifecycleRequest{
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
		Config: &apis.NetworkConfig{
			Interface: apis.InterfaceConfig{Name: "eth0"},
		},
	}

	if err := provider.PostAttachDevice(ctx, request); err != nil {
		t.Fatalf("PostAttachDevice failed: %v", err)
	}
	if err := provider.PreDetachDevice(ctx, request); err != nil {
		t.Fatalf("PreDetachDevice failed: %v", err)
	}

	for _, expectedPath := range []string{PathPostAttachDevice, PathPreDetachDevice} {
		got := <-received
		if got.path != expectedPath {
			t.Errorf("request path = %q, want %q", got.path, expectedPath)
		}
		if !reflect.DeepEqual(got.request, request) {
			t.Errorf("request = %#v, want %#v", got.request, request)
		}
	}
}

func TestWebhookDeviceLifecycleContextCancellation(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == PathHealth {
			json.NewEncoder(w).Encode(Capabilities{DeviceLifecycleProvider: true})
			return
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()

	provider, err := NewWebhookProvider(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("NewWebhookProvider failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err = provider.PostAttachDevice(ctx, cloudprovider.DeviceLifecycleRequest{})
	close(release)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("PostAttachDevice error = %v, want context deadline exceeded", err)
	}
}

func TestWebhookUnixSocket(t *testing.T) {
	// Create a temporary file for the unix socket
	f, err := os.CreateTemp("", "webhook-test-*.sock")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	sockPath := f.Name()
	f.Close()
	os.Remove(sockPath)

	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("failed to listen on unix socket: %v", err)
	}
	defer l.Close()
	defer os.Remove(sockPath)

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == PathHealth {
				caps := Capabilities{CloudProvider: true, ProfileProvider: true, DeviceLifecycleProvider: true}
				json.NewEncoder(w).Encode(caps)
				return
			}
			w.WriteHeader(http.StatusOK)
		}),
	}
	go srv.Serve(l)
	defer srv.Close()

	// Give the server a moment to start
	time.Sleep(100 * time.Millisecond)

	ctx := context.Background()
	socketURL := "unix://" + sockPath

	if !OnWebhook(ctx, socketURL) {
		t.Errorf("OnWebhook failed over unix socket")
	}

	provider, err := NewWebhookProvider(ctx, socketURL)
	if err != nil {
		t.Fatalf("NewWebhookProvider failed over unix socket: %v", err)
	}

	if !provider.caps.CloudProvider || !provider.caps.ProfileProvider {
		t.Errorf("Failed to retrieve capabilities over unix socket")
	}
	if err := provider.PostAttachDevice(ctx, cloudprovider.DeviceLifecycleRequest{}); err != nil {
		t.Errorf("PostAttachDevice failed over unix socket: %v", err)
	}
}

func TestNewWebhookProviderInit(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name          string
		handler       http.HandlerFunc
		endpoint      string
		closedServer  bool
		expectError   bool
		checkProvider func(*testing.T, *WebhookProvider)
	}{
		{
			name:        "Invalid URL format",
			endpoint:    "://invalid-url",
			expectError: true,
		},
		{
			name:         "Unreachable endpoint (server closed)",
			closedServer: true,
			expectError:  true,
		},
		{
			name: "Server returns non-200 status code",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			expectError: true,
		},
		{
			name: "Server returns invalid JSON",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{invalid-json`))
			},
			expectError: true,
		},
		{
			name: "Server returns valid capabilities",
			handler: func(w http.ResponseWriter, r *http.Request) {
				caps := Capabilities{CloudProvider: false, ProfileProvider: true}
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(caps)
			},
			expectError: false,
			checkProvider: func(t *testing.T, p *WebhookProvider) {
				if p.caps.CloudProvider {
					t.Errorf("Expected CloudProvider=false")
				}
				if !p.caps.ProfileProvider {
					t.Errorf("Expected ProfileProvider=true")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			endpoint := tt.endpoint
			if tt.handler != nil {
				srv := httptest.NewServer(tt.handler)
				defer srv.Close()
				endpoint = srv.URL
			} else if tt.closedServer {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
				endpoint = srv.URL
				srv.Close()
			}

			provider, err := NewWebhookProvider(ctx, endpoint)
			if (err != nil) != tt.expectError {
				t.Errorf("NewWebhookProvider() error = %v, expectError %v", err, tt.expectError)
			}
			if err == nil && tt.checkProvider != nil {
				tt.checkProvider(t, provider)
			}
		})
	}
}

func TestWebhookGetDeviceAttributesSerialization(t *testing.T) {
	ctx := context.Background()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == PathHealth {
			json.NewEncoder(w).Encode(Capabilities{CloudProvider: true, ProfileProvider: true})
			return
		}
		if r.URL.Path == PathGetDeviceAttributes {
			w.WriteHeader(http.StatusOK)
			// Return a JSON with the exact "string" key format expected by k8s.io/api/resource/v1
			w.Write([]byte(`{"dra.net/webhook_attr": {"string": "python"}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	provider, err := NewWebhookProvider(ctx, srv.URL)
	if err != nil {
		t.Fatalf("NewWebhookProvider failed: %v", err)
	}

	attrs := provider.GetDeviceAttributes(cloudprovider.DeviceIdentifiers{})
	if attrs == nil {
		t.Fatalf("Expected attributes to be returned, got nil")
	}

	attr, ok := attrs["dra.net/webhook_attr"]
	if !ok {
		t.Fatalf("Expected key 'dra.net/webhook_attr' not found in attributes")
	}

	if attr.StringValue == nil || *attr.StringValue != "python" {
		t.Errorf("Expected string value 'python', got %v", attr.StringValue)
	}
}

func TestWebhookGetProfileConfigPassesConfig(t *testing.T) {
	ctx := context.Background()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == PathHealth {
			json.NewEncoder(w).Encode(Capabilities{CloudProvider: true, ProfileProvider: true})
			return
		}
		if r.URL.Path == PathGetProfileConfig {
			var req ProfileRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			// Validate that the config is passed and contains the expected profile name
			if req.Config == nil || req.Config.Profile != "test-profile" {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"error": "expected config with profile 'test-profile'"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"interface": {"mtu": 1450}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	provider, err := NewWebhookProvider(ctx, srv.URL)
	if err != nil {
		t.Fatalf("NewWebhookProvider failed: %v", err)
	}

	testConfig := &apis.NetworkConfig{
		Profile: "test-profile",
	}

	conf, err := provider.GetProfileConfig(cloudprovider.DeviceIdentifiers{}, "claim-123", testConfig)
	if err != nil {
		t.Fatalf("Expected GetProfileConfig to succeed, got error: %v", err)
	}

	if conf == nil || conf.Interface.MTU == nil || *conf.Interface.MTU != 1450 {
		t.Errorf("Expected GetProfileConfig to return MTU 1450, got %v", conf)
	}
}
