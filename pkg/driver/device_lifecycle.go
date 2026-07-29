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
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/containerd/nri/pkg/api"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
)

type lifecycleDevice struct {
	name   string
	config DeviceConfig
}

func (np *NetworkDriver) deviceLifecycleRequest(
	pod *api.PodSandbox,
	deviceName string,
	config DeviceConfig,
	networkNamespace string,
) cloudprovider.DeviceLifecycleRequest {
	id := cloudprovider.DeviceIdentifiers{Name: deviceName}
	if config.DeviceSnapshot != nil {
		if mac := config.DeviceSnapshot.Attributes[apis.AttrMac].StringValue; mac != nil {
			id.MAC = *mac
		}
		if pci := config.DeviceSnapshot.Attributes[apis.AttrPCIAddress].StringValue; pci != nil {
			id.PCIAddress = *pci
		}
	}

	networkConfig := config.NetworkInterfaceConfigInPod
	return cloudprovider.DeviceLifecycleRequest{
		Device: id,
		Claim: cloudprovider.ObjectReference{
			Namespace: config.Claim.Namespace,
			Name:      config.Claim.Name,
			UID:       config.ClaimUID,
		},
		Pod: cloudprovider.ObjectReference{
			Namespace: pod.GetNamespace(),
			Name:      pod.GetName(),
			UID:       types.UID(pod.GetUid()),
		},
		NodeName:          np.nodeName,
		NetworkNamespace:  networkNamespace,
		HostInterfaceName: config.NetworkInterfaceConfigInHost.Interface.Name,
		PodInterfaceName:  config.NetworkInterfaceConfigInPod.Interface.Name,
		RDMADeviceName:    config.RDMADevice.LinkDev,
		Config:            &networkConfig,
	}
}

func (np *NetworkDriver) lifecycleDevices(podConfig PodConfig) []lifecycleDevice {
	devices := make([]lifecycleDevice, 0, len(podConfig.DeviceConfigs))
	for deviceName, config := range podConfig.DeviceConfigs {
		hasNetdev := config.NetworkInterfaceConfigInHost.Interface.Name != ""
		hasExclusiveRDMA := !np.rdmaSharedMode && config.RDMADevice.LinkDev != ""
		if hasNetdev || hasExclusiveRDMA {
			devices = append(devices, lifecycleDevice{name: deviceName, config: config})
		}
	}
	return devices
}

func (np *NetworkDriver) attachedLifecycleDevices(podConfig PodConfig) []lifecycleDevice {
	devices := np.lifecycleDevices(podConfig)
	attached := devices[:0]
	for _, device := range devices {
		if device.config.RuntimeAttached {
			attached = append(attached, device)
		}
	}
	return attached
}

func (np *NetworkDriver) setRuntimeAttached(
	podUID types.UID,
	devices []lifecycleDevice,
	attached bool,
) error {
	var storeErrors []error
	for _, device := range devices {
		config := device.config
		config.RuntimeAttached = attached
		if err := np.podConfigStore.SetDeviceConfig(podUID, device.name, config); err != nil {
			storeErrors = append(storeErrors, fmt.Errorf(
				"persist runtime attachment state for device %s: %w",
				device.name,
				err,
			))
		}
	}
	return errors.Join(storeErrors...)
}

func (np *NetworkDriver) deviceLifecycleRequests(
	pod *api.PodSandbox,
	devices []lifecycleDevice,
	networkNamespace string,
) []cloudprovider.DeviceLifecycleRequest {
	requests := make([]cloudprovider.DeviceLifecycleRequest, 0, len(devices))
	for _, device := range devices {
		requests = append(requests, np.deviceLifecycleRequest(
			pod,
			device.name,
			device.config,
			networkNamespace,
		))
	}
	return requests
}

func (np *NetworkDriver) callDeviceLifecycle(
	ctx context.Context,
	operation string,
	requests []cloudprovider.DeviceLifecycleRequest,
	call func(context.Context, cloudprovider.DeviceLifecycleRequest) error,
) error {
	if np.deviceLifecycleProvider == nil || len(requests) == 0 {
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, deviceLifecycleTimeoutOrDefault(np.deviceLifecycleTimeout))
	defer cancel()

	results := make(chan error, len(requests))
	for _, request := range requests {
		go func() {
			if err := call(callCtx, request); err != nil {
				results <- fmt.Errorf("%s failed for device %s: %w", operation, request.Device.Name, err)
				return
			}
			results <- nil
		}()
	}

	var callErrors []error
	for range requests {
		select {
		case err := <-results:
			if err != nil {
				callErrors = append(callErrors, err)
			}
		case <-callCtx.Done():
			callErrors = append(callErrors, fmt.Errorf("%s timed out: %w", operation, callCtx.Err()))
			return errors.Join(callErrors...)
		}
	}
	return errors.Join(callErrors...)
}

func (np *NetworkDriver) postAttachDevices(
	ctx context.Context,
	pod *api.PodSandbox,
	devices []lifecycleDevice,
	networkNamespace string,
) error {
	if np.deviceLifecycleProvider == nil {
		return nil
	}
	return np.callDeviceLifecycle(
		ctx,
		"PostAttachDevice",
		np.deviceLifecycleRequests(pod, devices, networkNamespace),
		np.deviceLifecycleProvider.PostAttachDevice,
	)
}

func (np *NetworkDriver) postAttachRequests(
	ctx context.Context,
	requests []cloudprovider.DeviceLifecycleRequest,
) error {
	if np.deviceLifecycleProvider == nil {
		return nil
	}
	return np.callDeviceLifecycle(
		ctx,
		"PostAttachDevice",
		requests,
		np.deviceLifecycleProvider.PostAttachDevice,
	)
}

func (np *NetworkDriver) preDetachDevices(
	ctx context.Context,
	pod *api.PodSandbox,
	devices []lifecycleDevice,
	networkNamespace string,
) error {
	if np.deviceLifecycleProvider == nil {
		return nil
	}
	return np.callDeviceLifecycle(
		ctx,
		"PreDetachDevice",
		np.deviceLifecycleRequests(pod, devices, networkNamespace),
		np.deviceLifecycleProvider.PreDetachDevice,
	)
}

// rollbackDeviceAttach returns devices to the host after a failed PostAttachDevice.
func (np *NetworkDriver) rollbackDeviceAttach(
	ctx context.Context,
	pod *api.PodSandbox,
	devices []lifecycleDevice,
	networkNamespace string,
) {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "netns", networkNamespace)
	if err := np.preDetachDevices(ctx, pod, devices, networkNamespace); err != nil {
		logger.Error(err, "PostAttachDevice rollback notifications failed")
	}

	needsRescan := false
	for i := len(devices) - 1; i >= 0; i-- {
		device := devices[i]
		deviceLogger := klog.LoggerWithValues(logger, "device", device.name)

		rdmaDetached := false
		if !np.rdmaSharedMode && device.config.RDMADevice.LinkDev != "" {
			if err := nsDetachRdmadev(networkNamespace, device.config.RDMADevice.LinkDev); err != nil {
				deviceLogger.Error(err, "PostAttachDevice rollback failed to return RDMA device")
			} else {
				rdmaDetached = true
			}
		}

		netdevDetached := false
		podIfName := device.config.NetworkInterfaceConfigInPod.Interface.Name
		hostIfName := device.config.NetworkInterfaceConfigInHost.Interface.Name
		if hostIfName != "" {
			if err := nsDetachNetdev(networkNamespace, podIfName, hostIfName); err != nil {
				deviceLogger.Error(err, "PostAttachDevice rollback failed to return network device")
			} else {
				netdevDetached = true
			}
		}

		if needsRescanAfterDetach(rdmaDetached, netdevDetached) {
			needsRescan = true
		}
		if np.deviceDetachComplete(device.config, rdmaDetached, netdevDetached) {
			if err := np.setRuntimeAttached(types.UID(pod.GetUid()), []lifecycleDevice{device}, false); err != nil {
				deviceLogger.Error(err, "PostAttachDevice rollback failed to clear runtime attachment state")
			}
		}
	}
	if np.netdb != nil && needsRescan {
		np.netdb.RequestRescan()
	}
}

func (np *NetworkDriver) deviceDetachComplete(config DeviceConfig, rdmaDetached, netdevDetached bool) bool {
	needsRDMA := !np.rdmaSharedMode && config.RDMADevice.LinkDev != ""
	needsNetdev := config.NetworkInterfaceConfigInHost.Interface.Name != ""
	return (!needsRDMA || rdmaDetached) && (!needsNetdev || netdevDetached)
}

func deviceLifecycleTimeoutOrDefault(timeout time.Duration) time.Duration {
	if timeout > 0 {
		return timeout
	}
	return time.Second
}
