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
	"os/exec"
	"path"
	"runtime"
	"strings"
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
		ARPIgnore:   ptr.To(1),
		ARPAnnounce: ptr.To(2),
	}
	if err := applyInterfaceIPv4Sysctls(ifaceName, hostIPv4Sysctls); err != nil {
		t.Fatalf("failed to configure host IPv4 sysctls: %v", err)
	}

	deviceData, err := nsAttachNetdev(ifaceName, path.Join("/run/netns", nsName), config, hostIPv4Sysctls)
	if err != nil {
		t.Fatalf("fail to attach netdev to namespace: %v", err)
	}

	// check against  ip lin
	func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		err := netns.Set(testNS)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("ip", "-d", "link", "show", config.Name)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("not able to use ethtool from namespace: %v", err)
		}
		outputStr := string(output)

		if !strings.Contains(outputStr, fmt.Sprintf("mtu %d", *config.MTU)) {
			t.Errorf("mtu not changed %s", outputStr)
		}
		if !strings.Contains(outputStr, fmt.Sprintf("gso_max_size %d", *config.GSOMaxSize)) {
			t.Errorf("GSOMaxSize not changed wanted %s got %s", fmt.Sprintf("gso_max_size %d", *config.GSOMaxSize), outputStr)
		}
		if !strings.Contains(outputStr, fmt.Sprintf("gro_max_size %d", *config.GROMaxSize)) {
			t.Errorf("GROMaxSize not changed %s", outputStr)
		}
		// require iproute 6.3.0+
		// TODO: validate the ip version to check it
		// https://github.com/iproute2/iproute2/commit/1dafe448c7a2f2be5dfddd8da250980708a48c41
		/*
			if !strings.Contains(outputStr, fmt.Sprintf("gso_ipv4_max_size %d", *config.GSOIPv4MaxSize)) {
				t.Errorf("GSOIPv4MaxSize not changed %s", outputStr)
			}
			if !strings.Contains(outputStr, fmt.Sprintf("gro_ipv4_max_size %d", *config.GROIPv4MaxSize)) {
				t.Errorf("GROIPv4MaxSize not changed %s", outputStr)
			}
		*/
		if !strings.Contains(outputStr, fmt.Sprintf("link/ether %s", *config.HardwareAddr)) {
			t.Errorf("HardwareAddr not changed %s", outputStr)
		}
		if *config.HardwareAddr != deviceData.HardwareAddress {
			t.Errorf("HardwareAddr not reported")
		}

		gotIPv4Sysctls, err := readInterfaceIPv4Sysctls(config.Name)
		if err != nil {
			t.Fatalf("failed to read pod IPv4 sysctls: %v", err)
		}
		if diff := cmp.Diff(hostIPv4Sysctls, gotIPv4Sysctls); diff != "" {
			t.Errorf("pod IPv4 sysctls mismatch (-want +got):\n%s", diff)
		}

		podIPv4Sysctls := &InterfaceIPv4Sysctls{
			ARPIgnore:   ptr.To(0),
			ARPAnnounce: ptr.To(0),
		}
		if err := applyInterfaceIPv4Sysctls(config.Name, podIPv4Sysctls); err != nil {
			t.Fatalf("failed to change pod IPv4 sysctls before detach: %v", err)
		}

		cmd = exec.Command("ip", "addr", "show", config.Name)
		output, err = cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("not able to use ethtool from namespace: %v", err)
		}
		outputStr = string(output)
		// TODO check reported state
		for _, addr := range config.Addresses {
			if !strings.Contains(outputStr, addr) {
				t.Errorf("address %s not found", addr)
			}
		}

		// Switch back to the original namespace
		err = netns.Set(origns)
		if err != nil {
			t.Fatal(err)
		}
	}()

	moved, err := nsDetachNetdev(path.Join("/run/netns", nsName), config.Name, ifaceName, hostIPv4Sysctls)
	if err != nil {
		t.Fatalf("fail to attach netdev to namespace: %v", err)
	}
	if !moved {
		t.Fatal("network device was not moved back to the host namespace")
	}

	gotIPv4Sysctls, err := readInterfaceIPv4Sysctls(ifaceName)
	if err != nil {
		t.Fatalf("failed to read restored host IPv4 sysctls: %v", err)
	}
	if diff := cmp.Diff(hostIPv4Sysctls, gotIPv4Sysctls); diff != "" {
		t.Errorf("restored host IPv4 sysctls mismatch (-want +got):\n%s", diff)
	}

	errorConfig := config
	errorConfig.Addresses = nil
	if _, err := nsAttachNetdev(ifaceName, path.Join("/run/netns", nsName), errorConfig, hostIPv4Sysctls); err != nil {
		t.Fatalf("failed to reattach netdev for restore error test: %v", err)
	}

	invalidHostIPv4Sysctls := *hostIPv4Sysctls
	invalidHostIPv4Sysctls.ARPIgnore = ptr.To(2_147_483_648)
	moved, err = nsDetachNetdev(path.Join("/run/netns", nsName), errorConfig.Name, ifaceName, &invalidHostIPv4Sysctls)
	if !moved {
		t.Fatal("network device was not moved back after sysctl restore error")
	}
	if err == nil || !strings.Contains(err.Error(), "arp_ignore") {
		t.Fatalf("nsDetachNetdev() error = %v, want arp_ignore restore error", err)
	}
	if err := applyInterfaceIPv4Sysctls(ifaceName, hostIPv4Sysctls); err != nil {
		t.Fatalf("failed to recover host IPv4 sysctls: %v", err)
	}

	invalidAttachIPv4Sysctls := *hostIPv4Sysctls
	invalidAttachIPv4Sysctls.ARPIgnore = ptr.To(2_147_483_648)
	if _, err := nsAttachNetdev(
		ifaceName,
		path.Join("/run/netns", nsName),
		errorConfig,
		&invalidAttachIPv4Sysctls,
	); err == nil || !strings.Contains(err.Error(), "arp_ignore") {
		t.Fatalf("nsAttachNetdev() error = %v, want arp_ignore apply error", err)
	}
	returnedDev, err := nlwrap.LinkByName(ifaceName)
	if err != nil {
		t.Fatalf("network device was not returned after sysctl apply error: %v", err)
	}
	if returnedDev.Attrs().Flags&net.FlagUp == 0 {
		t.Fatal("network device was not brought up after sysctl apply error")
	}
	if err := applyInterfaceIPv4Sysctls(ifaceName, hostIPv4Sysctls); err != nil {
		t.Fatalf("failed to recover host IPv4 sysctls after attach rollback: %v", err)
	}
}
