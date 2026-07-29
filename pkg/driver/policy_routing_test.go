/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package driver

import (
	"net"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"sigs.k8s.io/dranet/pkg/apis"
)

func TestRuleToConfigPreservesPolicySelectors(t *testing.T) {
	_, source, err := net.ParseCIDR("10.224.6.100/32")
	if err != nil {
		t.Fatal(err)
	}
	_, destination, err := net.ParseCIDR("10.224.27.74/32")
	if err != nil {
		t.Fatal(err)
	}

	rule := netlink.NewRule()
	rule.Priority = 11
	rule.Family = unix.AF_INET
	rule.Protocol = unix.RTPROT_STATIC
	rule.Table = 10
	rule.Src = source
	rule.Dst = destination
	rule.IifName = "rdma0"
	rule.OifName = "rdma0"
	want := apis.RuleConfig{
		Priority:    11,
		Family:      unix.AF_INET,
		Protocol:    unix.RTPROT_STATIC,
		Source:      "10.224.6.100/32",
		Destination: "10.224.27.74/32",
		IifName:     "rdma0",
		OifName:     "rdma0",
		Table:       10,
	}

	if diff := cmp.Diff(want, ruleToConfig(*rule)); diff != "" {
		t.Fatalf("ruleToConfig() mismatch (-want +got):\n%s", diff)
	}

	roundTrip, err := ruleFromConfig(want)
	if err != nil {
		t.Fatalf("ruleFromConfig() error: %v", err)
	}
	if diff := cmp.Diff(want, ruleToConfig(*roundTrip)); diff != "" {
		t.Fatalf("rule conversion round trip mismatch (-want +got):\n%s", diff)
	}
}

func TestCanPreserveRuleRejectsUnsupportedSelectors(t *testing.T) {
	rule := netlink.NewRule()
	rule.Priority = 10
	rule.Family = unix.AF_INET
	rule.Table = 10
	rule.OifName = "rdma0"
	if !canPreserveRule(*rule) {
		t.Fatal("canPreserveRule() rejected a supported output-interface rule")
	}

	rule.Mark = 1
	if canPreserveRule(*rule) {
		t.Fatal("canPreserveRule() accepted a rule with an unsupported mark selector")
	}
}

func TestTranslateRuleInterfaceNames(t *testing.T) {
	rules := []apis.RuleConfig{
		{Priority: 10, Family: unix.AF_INET, OifName: "rdma0", Table: 10},
		{Priority: 11, Family: unix.AF_INET, IifName: "other0", Table: 10},
	}

	got := translateRuleInterfaceNames(rules, "rdma0", "net1")
	want := []apis.RuleConfig{
		{Priority: 10, Family: unix.AF_INET, OifName: "net1", Table: 10},
		{Priority: 11, Family: unix.AF_INET, IifName: "other0", Table: 10},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("translateRuleInterfaceNames() mismatch (-want +got):\n%s", diff)
	}
	if rules[0].OifName != "rdma0" {
		t.Fatalf("translateRuleInterfaceNames() changed host rule: %q", rules[0].OifName)
	}
}

func TestAppendDiscoveredRoutesKeepsConfiguredRoute(t *testing.T) {
	configured := []apis.RouteConfig{
		{Destination: "0.0.0.0/0", Gateway: "192.0.2.1", Table: 10},
	}
	discovered := []apis.RouteConfig{
		{Destination: "0.0.0.0/0", Gateway: "198.51.100.1", Table: 10},
		{Destination: "10.224.0.0/12", Scope: uint8(netlink.SCOPE_LINK), Table: 10},
	}
	want := []apis.RouteConfig{
		{Destination: "0.0.0.0/0", Gateway: "192.0.2.1", Table: 10},
		{Destination: "10.224.0.0/12", Scope: uint8(netlink.SCOPE_LINK), Table: 10},
	}

	if diff := cmp.Diff(want, appendDiscoveredRoutes(configured, discovered)); diff != "" {
		t.Fatalf("appendDiscoveredRoutes() mismatch (-want +got):\n%s", diff)
	}
}

func TestRouteToConfigDefaultRoutes(t *testing.T) {
	tests := []struct {
		name  string
		route netlink.Route
		want  apis.RouteConfig
	}{
		{
			name: "IPv4",
			route: netlink.Route{
				Family:   unix.AF_INET,
				Gw:       net.ParseIP("10.0.0.1"),
				Scope:    netlink.SCOPE_UNIVERSE,
				Priority: 100,
				Table:    10,
			},
			want: apis.RouteConfig{
				Destination: "0.0.0.0/0",
				Gateway:     "10.0.0.1",
				Metric:      100,
				Table:       10,
			},
		},
		{
			name: "IPv6",
			route: netlink.Route{
				Family: unix.AF_INET6,
				Gw:     net.ParseIP("2001:db8::1"),
				Scope:  netlink.SCOPE_UNIVERSE,
				Table:  11,
			},
			want: apis.RouteConfig{
				Destination: "::/0",
				Gateway:     "2001:db8::1",
				Table:       11,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := routeToConfig(tt.route)
			if !ok {
				t.Fatal("routeToConfig() rejected a known address family")
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Fatalf("routeToConfig() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRouteToConfigRejectsUnknownDefaultRouteFamily(t *testing.T) {
	if _, ok := routeToConfig(netlink.Route{}); ok {
		t.Fatal("routeToConfig() accepted a default route with unknown address family")
	}
}
