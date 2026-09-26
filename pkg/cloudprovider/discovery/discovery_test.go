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

package discovery

import (
	"context"
	"net/netip"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
	"sigs.k8s.io/dranet/pkg/cloudprovider/coreweave"
	"sigs.k8s.io/dranet/pkg/cloudprovider/oke"
)

func TestCloudProviderProbeOrder(t *testing.T) {
	probes := cloudProviderProbes(context.Background(), "", Dependencies{})
	want := []CloudProviderHint{
		CloudProviderHintGCE,
		CloudProviderHintAWS,
		CloudProviderHintAzure,
		CloudProviderHintOKE,
		CloudProviderHintAlibaba,
		CloudProviderHintCKS,
		CloudProviderHintWebhook,
	}

	if len(probes) != len(want) {
		t.Fatalf("cloudProviderProbes() returned %d probes, want %d", len(probes), len(want))
	}
	for i := range want {
		if probes[i].hint != want[i] {
			t.Errorf("cloudProviderProbes()[%d].hint = %q, want %q", i, probes[i].hint, want[i])
		}
	}
}

func TestDetectCloudProviderReturnsFirstMatch(t *testing.T) {
	probes := []cloudProviderProbe{
		{hint: CloudProviderHintAzure, match: func() bool { return true }},
		{hint: CloudProviderHintCKS, match: func() bool { return true }},
	}

	if got := detectCloudProvider(probes); got != CloudProviderHintAzure {
		t.Fatalf("detectCloudProvider() = %q, want %q", got, CloudProviderHintAzure)
	}
}

func TestGetInstancePropertiesCKS(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "cks-node",
		Labels: map[string]string{
			coreweave.LabelCKSCluster:   "use15",
			coreweave.LabelFabricFlavor: "infiniband",
			coreweave.LabelFabric:       "US-EAST-15A-FAB66",
		},
	}}
	dependencies := Dependencies{
		NodeClient: fake.NewSimpleClientset(node).CoreV1().Nodes(),
		NodeName:   node.Name,
	}

	instance, err := GetInstanceProperties(context.Background(), CloudProviderHintCKS, "", dependencies)
	if err != nil {
		t.Fatalf("GetInstanceProperties() error = %v", err)
	}
	if _, ok := instance.(*coreweave.Instance); !ok {
		t.Fatalf("GetInstanceProperties() = %T, want *coreweave.Instance", instance)
	}
}

func TestGetInstancePropertiesCKSRequiresDependencies(t *testing.T) {
	if _, err := GetInstanceProperties(context.Background(), CloudProviderHintCKS, "", Dependencies{}); err == nil {
		t.Fatal("GetInstanceProperties() error = nil, want missing Kubernetes dependency error")
	}
}

// The parsed --oke-rdma-child-cidr reaches the OKE provider only when it is set.
func TestGetInstancePropertiesPassesOKEChildRange(t *testing.T) {
	tests := []struct {
		name       string
		childRange netip.Prefix
		want       int
	}{
		{name: "no child range"},
		{name: "child range", childRange: netip.MustParsePrefix("10.192.0.0/14"), want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := -1
			original := okeGetInstance
			okeGetInstance = func(_ context.Context, opts ...oke.Option) (cloudprovider.CloudInstance, error) {
				got = len(opts)
				return nil, nil
			}
			t.Cleanup(func() { okeGetInstance = original })

			if _, err := GetInstanceProperties(context.Background(), CloudProviderHintOKE, "", Dependencies{OKEChildIPv4Range: tt.childRange}); err != nil {
				t.Fatalf("GetInstanceProperties() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("the OKE provider got %d options, want %d", got, tt.want)
			}
		})
	}
}
