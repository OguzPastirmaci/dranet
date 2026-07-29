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

package driver

import (
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"path"
	"runtime"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/dranet/internal/nlwrap"
	"sigs.k8s.io/dranet/pkg/apis"
)

func Test_nhNetdev(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges.")
	}

	origns, err := netns.Get()
	if err != nil {
		t.Fatalf("unexpected error trying to get namespace: %v", err)
	}
	defer origns.Close()

	rndString := make([]byte, 4)
	_, err = rand.Read(rndString)
	if err != nil {
		t.Errorf("fail to generate random name: %v", err)
	}
	nsName := fmt.Sprintf("ns%x", rndString)
	testNS, err := netns.NewNamed(nsName)
	if err != nil {
		t.Fatalf("Failed to create network namespace: %v", err)
	}
	defer netns.DeleteNamed(nsName)
	defer testNS.Close()

	// Switch back to the original namespace
	netns.Set(origns)

	// Create a dummy interface in the test namespace
	nhNs, err := nlwrap.NewHandleAt(testNS)
	if err != nil {
		t.Fatalf("fail to open netlink handle: %v", err)
	}
	defer nhNs.Close()

	loLink, err := nhNs.LinkByName("lo")
	if err != nil {
		t.Fatalf("Failed to get loopback interface: %v", err)
	}
	if err := nhNs.LinkSetUp(loLink); err != nil {
		t.Fatalf("Failed to set up loopback interface: %v", err)
	}

	ifaceName := "testdummy-0"
	// Create a veth pair
	la := netlink.NewLinkAttrs()
	la.Name = ifaceName
	link := &netlink.Dummy{
		LinkAttrs: la,
	}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatalf("Failed to add dummy link %s in ns %s: %v", ifaceName, nsName, err)
	}

	t.Cleanup(func() {
		link, err := nlwrap.LinkByName(ifaceName)
		if err == nil {
			_ = netlink.LinkDel(link)
		}
	})
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("Failed to add veth link %s in ns %s: %v", ifaceName, nsName, err)
	}
	config := apis.InterfaceConfig{
		Name:           "dranet0",
		Addresses:      []string{"192.168.7.7/32"},
		MTU:            ptr.To[int32](1234),
		HardwareAddr:   ptr.To("00:11:22:33:44:55"),
		GSOMaxSize:     ptr.To[int32](1024),
		GROMaxSize:     ptr.To[int32](1025),
		GSOIPv4MaxSize: ptr.To[int32](1026),
		GROIPv4MaxSize: ptr.To[int32](1027),
	}
	hostIPv4Sysctls := &InterfaceIPv4Sysctls{
		RPFilter:    ptr.To(0),
		ARPIgnore:   ptr.To(1),
		ARPAnnounce: ptr.To(2),
		AcceptLocal: ptr.To(1),
		ARPFilter:   ptr.To(1),
	}
	tableID := 10000 + int(rndString[0])
	rulePriority := 20000 + int(rndString[1])
	expectedHostNetworkConfig := apis.NetworkConfig{
		Interface: apis.InterfaceConfig{Name: ifaceName},
		Routes: []apis.RouteConfig{
			{
				Destination: "198.18.0.0/15",
				Scope:       uint8(netlink.SCOPE_LINK),
				Metric:      100,
				Table:       tableID,
			},
		},
		Rules: []apis.RuleConfig{
			{Priority: rulePriority, Family: netlink.FAMILY_V4, OifName: ifaceName, Table: tableID},
			{Priority: rulePriority + 1, Family: netlink.FAMILY_V4, Source: "198.18.0.1/32", Table: tableID},
			{Priority: rulePriority + 2, Family: netlink.FAMILY_V4, Destination: "198.18.0.1/32", Table: tableID},
		},
	}
	if err := applyInterfaceIPv4Sysctls(ifaceName, hostIPv4Sysctls); err != nil {
		t.Fatalf("failed to set host IPv4 sysctls: %v", err)
	}

	_, policyDestination, err := net.ParseCIDR(expectedHostNetworkConfig.Routes[0].Destination)
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.RouteAdd(&netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       policyDestination,
		Scope:     netlink.SCOPE_LINK,
		Priority:  expectedHostNetworkConfig.Routes[0].Metric,
		Table:     tableID,
	}); err != nil {
		t.Fatalf("failed to add host policy route: %v", err)
	}
	for _, ruleConfig := range expectedHostNetworkConfig.Rules {
		rule, err := ruleFromConfig(ruleConfig)
		if err != nil {
			t.Fatalf("failed to build host policy rule: %v", err)
		}
		if err := netlink.RuleAdd(rule); err != nil {
			t.Fatalf("failed to add host policy rule: %v", err)
		}
	}
	t.Cleanup(func() {
		for _, ruleConfig := range expectedHostNetworkConfig.Rules {
			rule, err := ruleFromConfig(ruleConfig)
			if err == nil {
				_ = netlink.RuleDel(rule)
			}
		}
	})

	hostHandle, err := nlwrap.NewHandle()
	if err != nil {
		t.Fatalf("failed to get host netlink handle: %v", err)
	}
	defer hostHandle.Close()

	capturedRoutes, tables, err := getRouteInfo(hostHandle, ifaceName, link)
	if err != nil {
		t.Fatalf("failed to capture host policy routes: %v", err)
	}
	if !tables.Has(tableID) {
		t.Fatalf("captured route tables do not include %d: %v", tableID, tables)
	}
	rulesByTable, err := getRuleInfo(hostHandle)
	if err != nil {
		t.Fatalf("failed to capture host policy rules: %v", err)
	}
	hostNetworkConfig := apis.NetworkConfig{
		Interface: apis.InterfaceConfig{Name: ifaceName},
		Routes:    capturedRoutes,
		Rules:     rulesByTable[tableID],
	}
	if diff := cmp.Diff(expectedHostNetworkConfig, hostNetworkConfig); diff != "" {
		t.Fatalf("captured host policy routing mismatch (-want +got):\n%s", diff)
	}

	deviceData, err := nsAttachNetdev(
		ifaceName,
		path.Join("/run/netns", nsName),
		config,
		hostNetworkConfig,
		hostIPv4Sysctls,
	)
	if err != nil {
		t.Fatalf("fail to attach netdev to namespace: %v", err)
	}
	if err := applyRoutingConfig(path.Join("/run/netns", nsName), config.Name, hostNetworkConfig.Routes, 0); err != nil {
		t.Fatalf("failed to apply pod policy route: %v", err)
	}
	podRules := translateRuleInterfaceNames(hostNetworkConfig.Rules, ifaceName, config.Name)
	if err := applyRulesConfig(path.Join("/run/netns", nsName), podRules); err != nil {
		t.Fatalf("failed to apply pod policy rules: %v", err)
	}

	func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		err := netns.Set(testNS)
		if err != nil {
			t.Fatal(err)
		}

		nsLink, err := nhNs.LinkByName(config.Name)
		if err != nil {
			t.Fatalf("failed to get pod interface: %v", err)
		}
		attrs := nsLink.Attrs()
		if attrs.MTU != int(*config.MTU) {
			t.Errorf("MTU mismatch: want %d, got %d", *config.MTU, attrs.MTU)
		}
		if attrs.GSOMaxSize != uint32(*config.GSOMaxSize) {
			t.Errorf("GSOMaxSize mismatch: want %d, got %d", *config.GSOMaxSize, attrs.GSOMaxSize)
		}
		if attrs.GROMaxSize != uint32(*config.GROMaxSize) {
			t.Errorf("GROMaxSize mismatch: want %d, got %d", *config.GROMaxSize, attrs.GROMaxSize)
		}
		if attrs.HardwareAddr.String() != *config.HardwareAddr {
			t.Errorf("hardware address mismatch: want %s, got %s", *config.HardwareAddr, attrs.HardwareAddr)
		}
		if *config.HardwareAddr != deviceData.HardwareAddress {
			t.Errorf("reported hardware address mismatch: want %s, got %s", *config.HardwareAddr, deviceData.HardwareAddress)
		}

		addresses, err := nhNs.AddrList(nsLink, netlink.FAMILY_ALL)
		if err != nil {
			t.Fatalf("failed to list pod interface addresses: %v", err)
		}
		for _, wantAddress := range config.Addresses {
			found := false
			for _, address := range addresses {
				if address.IPNet.String() == wantAddress {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("address %s not found in %#v", wantAddress, addresses)
			}
		}

		gotIPv4Sysctls, err := readInterfaceIPv4Sysctls(config.Name)
		if err != nil {
			t.Fatalf("failed to read IPv4 sysctls in test namespace: %v", err)
		}
		if diff := cmp.Diff(hostIPv4Sysctls, gotIPv4Sysctls); diff != "" {
			t.Errorf("IPv4 sysctls in test namespace mismatch (-want +got):\n%s", diff)
		}

		routes, err := nhNs.RouteListFiltered(
			netlink.FAMILY_V4,
			&netlink.Route{Table: tableID},
			netlink.RT_FILTER_TABLE,
		)
		if err != nil {
			t.Fatalf("failed to list pod policy routes: %v", err)
		}
		if len(routes) != 1 ||
			routes[0].Dst == nil ||
			routes[0].Dst.String() != hostNetworkConfig.Routes[0].Destination ||
			routes[0].Priority != hostNetworkConfig.Routes[0].Metric {
			t.Fatalf("pod policy route mismatch: %#v", routes)
		}

		rules, err := nhNs.RuleList(netlink.FAMILY_V4)
		if err != nil {
			t.Fatalf("failed to list pod policy rules: %v", err)
		}
		var gotPodRules []apis.RuleConfig
		for _, rule := range rules {
			if rule.Table == tableID {
				gotPodRules = append(gotPodRules, ruleToConfig(rule))
			}
		}
		if diff := cmp.Diff(podRules, gotPodRules); diff != "" {
			t.Fatalf("pod policy rules mismatch (-want +got):\n%s", diff)
		}

		// Switch back to the original namespace
		err = netns.Set(origns)
		if err != nil {
			t.Fatal(err)
		}
	}()

	_, err = nsDetachNetdev(path.Join("/run/netns", nsName), config.Name, hostNetworkConfig, hostIPv4Sysctls)
	if err != nil {
		t.Fatalf("fail to attach netdev to namespace: %v", err)
	}
	if err := restoreHostInterfaceConfig(ifaceName, hostNetworkConfig, hostIPv4Sysctls); err != nil {
		t.Fatalf("failed to retry host interface restoration: %v", err)
	}

	gotIPv4Sysctls, err := readInterfaceIPv4Sysctls(ifaceName)
	if err != nil {
		t.Fatalf("failed to read restored host IPv4 sysctls: %v", err)
	}
	if diff := cmp.Diff(hostIPv4Sysctls, gotIPv4Sysctls); diff != "" {
		t.Errorf("restored host IPv4 sysctls mismatch (-want +got):\n%s", diff)
	}

	routes, err := hostHandle.RouteListFiltered(
		netlink.FAMILY_V4,
		&netlink.Route{Table: tableID},
		netlink.RT_FILTER_TABLE,
	)
	if err != nil {
		t.Fatalf("failed to list restored host policy routes: %v", err)
	}
	if len(routes) != 1 ||
		routes[0].Dst == nil ||
		routes[0].Dst.String() != hostNetworkConfig.Routes[0].Destination ||
		routes[0].Priority != hostNetworkConfig.Routes[0].Metric {
		t.Fatalf("restored host policy route mismatch: %#v", routes)
	}

	rules, err := hostHandle.RuleList(netlink.FAMILY_V4)
	if err != nil {
		t.Fatalf("failed to list restored host policy rules: %v", err)
	}
	var gotHostRules []apis.RuleConfig
	for _, rule := range rules {
		if rule.Table == tableID {
			gotHostRules = append(gotHostRules, ruleToConfig(rule))
		}
	}
	if diff := cmp.Diff(hostNetworkConfig.Rules, gotHostRules); diff != "" {
		t.Fatalf("restored host policy rules mismatch (-want +got):\n%s", diff)
	}
}
