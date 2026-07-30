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

// InterfaceIPv4Sysctls holds per-interface settings captured before a
// network device moves out of the host namespace.
type InterfaceIPv4Sysctls struct {
	ARPIgnore   *int `json:"arpIgnore,omitempty"`
	ARPAnnounce *int `json:"arpAnnounce,omitempty"`
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

	config.ARPIgnore, err = readOptionalInterfaceIPv4Sysctl(sysctlInterface, ifName, "arp_ignore")
	if err != nil {
		return nil, err
	}
	config.ARPAnnounce, err = readOptionalInterfaceIPv4Sysctl(sysctlInterface, ifName, "arp_announce")
	if err != nil {
		return nil, err
	}
	if config.ARPIgnore == nil && config.ARPAnnounce == nil {
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

	if config.ARPIgnore != nil {
		set("arp_ignore", *config.ARPIgnore)
	}
	if config.ARPAnnounce != nil {
		set("arp_announce", *config.ARPAnnounce)
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
			// Keep this thread locked so the runtime cannot reuse it in the
			// wrong network namespace.
			unlockThread = false
			result <- errors.Join(applyErr, fmt.Errorf("failed to restore network namespace: %w", err))
			return
		}
		result <- applyErr
	}()
	return <-result
}
