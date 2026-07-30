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
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	sysctltesting "k8s.io/component-helpers/node/util/sysctl/testing"
	"k8s.io/utils/ptr"
)

type failingSetSysctl struct {
	*sysctltesting.Fake
	setting string
}

func (f *failingSetSysctl) SetSysctl(setting string, value int) error {
	if setting == f.setting {
		return errors.New("test set failure")
	}
	return f.Fake.SetSysctl(setting, value)
}

func TestReadInterfaceIPv4Sysctls(t *testing.T) {
	sysctls := sysctltesting.NewFake()
	sysctls.Settings = map[string]int{
		"net/ipv4/conf/rdma0/arp_ignore":   1,
		"net/ipv4/conf/rdma0/arp_announce": 2,
	}

	got, err := readInterfaceIPv4SysctlsWithSysctl(sysctls, "rdma0")
	if err != nil {
		t.Fatalf("readInterfaceIPv4SysctlsWithSysctl() error: %v", err)
	}
	want := &InterfaceIPv4Sysctls{
		ARPIgnore:   ptr.To(1),
		ARPAnnounce: ptr.To(2),
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("readInterfaceIPv4SysctlsWithSysctl() mismatch (-want +got):\n%s", diff)
	}
}

func TestReadInterfaceIPv4SysctlsSkipsMissingSettings(t *testing.T) {
	sysctls := sysctltesting.NewFake()
	sysctls.Settings["net/ipv4/conf/rdma0/arp_ignore"] = 1

	got, err := readInterfaceIPv4SysctlsWithSysctl(sysctls, "rdma0")
	if err != nil {
		t.Fatalf("readInterfaceIPv4SysctlsWithSysctl() error: %v", err)
	}
	want := &InterfaceIPv4Sysctls{ARPIgnore: ptr.To(1)}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("readInterfaceIPv4SysctlsWithSysctl() mismatch (-want +got):\n%s", diff)
	}
}

func TestReadInterfaceIPv4SysctlsReturnsNilWhenUnsupported(t *testing.T) {
	got, err := readInterfaceIPv4SysctlsWithSysctl(sysctltesting.NewFake(), "rdma0")
	if err != nil {
		t.Fatalf("readInterfaceIPv4SysctlsWithSysctl() error: %v", err)
	}
	if got != nil {
		t.Errorf("readInterfaceIPv4SysctlsWithSysctl() = %#v, want nil", got)
	}
}

func TestApplyInterfaceIPv4Sysctls(t *testing.T) {
	sysctls := sysctltesting.NewFake()
	config := &InterfaceIPv4Sysctls{
		ARPIgnore:   ptr.To(1),
		ARPAnnounce: ptr.To(2),
	}

	if err := applyInterfaceIPv4SysctlsWithSysctl(sysctls, "rdma0", config); err != nil {
		t.Fatalf("applyInterfaceIPv4SysctlsWithSysctl() error: %v", err)
	}
	want := map[string]int{
		"net/ipv4/conf/rdma0/arp_ignore":   1,
		"net/ipv4/conf/rdma0/arp_announce": 2,
	}
	if diff := cmp.Diff(want, sysctls.Settings); diff != "" {
		t.Errorf("applyInterfaceIPv4SysctlsWithSysctl() mismatch (-want +got):\n%s", diff)
	}
}

func TestApplyInterfaceIPv4SysctlsNilConfig(t *testing.T) {
	sysctls := sysctltesting.NewFake()
	if err := applyInterfaceIPv4SysctlsWithSysctl(sysctls, "rdma0", nil); err != nil {
		t.Fatalf("applyInterfaceIPv4SysctlsWithSysctl() error: %v", err)
	}
	if len(sysctls.Settings) != 0 {
		t.Errorf("applyInterfaceIPv4SysctlsWithSysctl() changed settings: %#v", sysctls.Settings)
	}
}

func TestApplyInterfaceIPv4SysctlsReturnsSetErrors(t *testing.T) {
	sysctls := &failingSetSysctl{
		Fake:    sysctltesting.NewFake(),
		setting: "net/ipv4/conf/rdma0/arp_ignore",
	}
	config := &InterfaceIPv4Sysctls{
		ARPIgnore:   ptr.To(1),
		ARPAnnounce: ptr.To(2),
	}

	err := applyInterfaceIPv4SysctlsWithSysctl(sysctls, "rdma0", config)
	if err == nil || !strings.Contains(err.Error(), sysctls.setting) {
		t.Fatalf("applyInterfaceIPv4SysctlsWithSysctl() error = %v, want setting name", err)
	}
	if len(sysctls.Settings) != 1 {
		t.Errorf("applyInterfaceIPv4SysctlsWithSysctl() applied %d settings, want 1", len(sysctls.Settings))
	}
}
