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
	"fmt"
	"os"
	"runtime"

	"github.com/vishvananda/netns"
	"k8s.io/component-helpers/node/util/sysctl"
)

// InterfaceIPv4Sysctls holds the host values that must follow an interface
// when it moves between network namespaces.
type InterfaceIPv4Sysctls struct {
	RPFilter    *int `json:"rpFilter,omitempty"`
	ARPIgnore   *int `json:"arpIgnore,omitempty"`
	ARPAnnounce *int `json:"arpAnnounce,omitempty"`
	AcceptLocal *int `json:"acceptLocal,omitempty"`
	ARPFilter   *int `json:"arpFilter,omitempty"`
}

func interfaceIPv4Sysctl(ifName, setting string) string {
	return fmt.Sprintf("net/ipv4/conf/%s/%s", ifName, setting)
}

func readOptionalInterfaceIPv4Sysctl(sysctlInterface sysctl.Interface, ifName, setting string) (*int, error) {
	name := interfaceIPv4Sysctl(ifName, setting)
	value, err := sysctlInterface.GetSysctl(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", name, err)
	}
	return &value, nil
}

func readInterfaceIPv4SysctlsWithSysctl(sysctlInterface sysctl.Interface, ifName string) (*InterfaceIPv4Sysctls, error) {
	config := &InterfaceIPv4Sysctls{}
	var err error

	config.RPFilter, err = readOptionalInterfaceIPv4Sysctl(sysctlInterface, ifName, "rp_filter")
	if err != nil {
		return nil, err
	}
	config.ARPIgnore, err = readOptionalInterfaceIPv4Sysctl(sysctlInterface, ifName, "arp_ignore")
	if err != nil {
		return nil, err
	}
	config.ARPAnnounce, err = readOptionalInterfaceIPv4Sysctl(sysctlInterface, ifName, "arp_announce")
	if err != nil {
		return nil, err
	}
	config.AcceptLocal, err = readOptionalInterfaceIPv4Sysctl(sysctlInterface, ifName, "accept_local")
	if err != nil {
		return nil, err
	}
	config.ARPFilter, err = readOptionalInterfaceIPv4Sysctl(sysctlInterface, ifName, "arp_filter")
	if err != nil {
		return nil, err
	}

	if config.RPFilter == nil && config.ARPIgnore == nil && config.ARPAnnounce == nil &&
		config.AcceptLocal == nil && config.ARPFilter == nil {
		return nil, nil
	}
	return config, nil
}

func readInterfaceIPv4Sysctls(ifName string) (*InterfaceIPv4Sysctls, error) {
	return readInterfaceIPv4SysctlsWithSysctl(sysctl.New(), ifName)
}

func applyInterfaceIPv4SysctlsWithSysctl(sysctlInterface sysctl.Interface, ifName string, config *InterfaceIPv4Sysctls) error {
	if config == nil {
		return nil
	}

	var errorList []error
	set := func(setting string, value int) {
		name := interfaceIPv4Sysctl(ifName, setting)
		if err := sysctlInterface.SetSysctl(name, value); err != nil {
			errorList = append(errorList, fmt.Errorf("failed to set %s: %w", name, err))
		}
	}

	if config.RPFilter != nil {
		set("rp_filter", *config.RPFilter)
	}
	if config.ARPIgnore != nil {
		set("arp_ignore", *config.ARPIgnore)
	}
	if config.ARPAnnounce != nil {
		set("arp_announce", *config.ARPAnnounce)
	}
	if config.AcceptLocal != nil {
		set("accept_local", *config.AcceptLocal)
	}
	if config.ARPFilter != nil {
		set("arp_filter", *config.ARPFilter)
	}
	return errors.Join(errorList...)
}

func applyInterfaceIPv4Sysctls(ifName string, config *InterfaceIPv4Sysctls) error {
	return applyInterfaceIPv4SysctlsWithSysctl(sysctl.New(), ifName, config)
}

func applyInterfaceIPv4SysctlsInNamespace(containerNsPath, ifName string, config *InterfaceIPv4Sysctls) error {
	if config == nil {
		return nil
	}

	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		unlockThread := true
		defer func() {
			if unlockThread {
				runtime.UnlockOSThread()
			}
		}()

		originalNs, err := netns.Get()
		if err != nil {
			result <- fmt.Errorf("failed to get current network namespace: %w", err)
			return
		}
		defer originalNs.Close()

		containerNs, err := netns.GetFromPath(containerNsPath)
		if err != nil {
			result <- fmt.Errorf("could not get network namespace from path %s: %w", containerNsPath, err)
			return
		}
		defer containerNs.Close()

		if err := netns.Set(containerNs); err != nil {
			result <- fmt.Errorf("failed to join network namespace %s: %w", containerNsPath, err)
			return
		}

		applyErr := applyInterfaceIPv4Sysctls(ifName, config)
		if err := netns.Set(originalNs); err != nil {
			// The goroutine must exit while locked so the runtime discards
			// the thread instead of reusing it in the wrong namespace.
			unlockThread = false
			result <- errors.Join(applyErr, fmt.Errorf("failed to restore network namespace: %w", err))
			return
		}
		result <- applyErr
	}()
	return <-result
}
