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
	"fmt"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	"sigs.k8s.io/dranet/pkg/cloudprovider"
)

const (
	// imdsEndpoint is the Oracle Cloud Instance Metadata Service endpoint.
	imdsEndpoint = "http://169.254.169.254/opc/v2"

	imdsInitialRetryInterval = 1 * time.Second
	imdsInitialWait          = 15 * time.Second
	imdsRetryInterval        = 10 * time.Second
	imdsMaxRetryInterval     = 30 * time.Second
	imdsRefreshInterval      = 15 * time.Minute
	imdsRequestTimeout       = 15 * time.Second
)

// imdsHostRDMATopologyData contains the RDMA topology fields from the OCI
// IMDS host metadata response. This is only populated when RDMA topology
// data is enabled for the tenancy.
type imdsHostRDMATopologyData struct {
	CustomerGpuMemoryFabric string `json:"customerGpuMemoryFabric"`
	CustomerHPCIslandId     string `json:"customerHPCIslandId"`
	CustomerHostId          string `json:"customerHostId"`
	CustomerLocalBlock      string `json:"customerLocalBlock"`
	CustomerNetworkBlock    string `json:"customerNetworkBlock"`
}

// imdsRDMAFabricData contains /opc/v2/host/rdmaFabricData from OCI IMDS.
type imdsRDMAFabricData struct {
	IPv6   *bool  `json:"ipv6"`
	Planes *int64 `json:"planes"`
}

// imdsHostMetadata contains the fields we care about from the OCI IMDS
// host metadata response at /opc/v2/host/.
type imdsHostMetadata struct {
	NetworkBlockId   string                    `json:"networkBlockId"`
	RackId           string                    `json:"rackId"`
	RDMATopologyData *imdsHostRDMATopologyData `json:"rdmaTopologyData"`
	RDMAFabricData   *imdsRDMAFabricData       `json:"rdmaFabricData"`
}

// OnOKE returns true if running on an Oracle Cloud Infrastructure instance.
// Detection is done by probing the OCI IMDS v2 endpoint.
func OnOKE(ctx context.Context) bool {
	pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return wait.PollUntilContextCancel(pollCtx, 1*time.Second, true, func(ctx context.Context) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, imdsEndpoint+"/instance/", nil)
		if err != nil {
			return false, nil
		}
		req.Header.Set("Authorization", "Bearer Oracle")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false, nil
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK, nil
	}) == nil
}

func queryRDMAFabricData(ctx context.Context, client *http.Client, endpoint string) (*imdsRDMAFabricData, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("could not create OCI IMDS RDMA fabric request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer Oracle")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach OCI IMDS RDMA fabric endpoint: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OCI IMDS RDMA fabric endpoint returned status %d", resp.StatusCode)
	}

	var data imdsRDMAFabricData
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("could not parse OCI IMDS RDMA fabric response: %w", err)
	}
	return &data, nil
}

func queryHostMetadata(ctx context.Context, client *http.Client, endpoint string) (*imdsHostMetadata, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("could not create OCI IMDS host request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer Oracle")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach OCI IMDS host endpoint: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OCI IMDS host endpoint returned status %d", resp.StatusCode)
	}

	var metadata imdsHostMetadata
	if err := json.NewDecoder(resp.Body).Decode(&metadata); err != nil {
		return nil, fmt.Errorf("could not parse OCI IMDS host response: %w", err)
	}
	return &metadata, nil
}

func parseRDMAFabricData(data *imdsRDMAFabricData) (*RDMAFabric, error) {
	if data == nil {
		return nil, nil
	}
	if data.IPv6 == nil {
		return nil, errors.New("OCI IMDS RDMA fabric response does not contain ipv6")
	}
	if data.Planes == nil {
		return nil, errors.New("OCI IMDS RDMA fabric response does not contain planes")
	}
	return &RDMAFabric{IPv6: *data.IPv6, Planes: *data.Planes}, nil
}

func metadataFromIMDS(host *imdsHostMetadata, fabric *RDMAFabric) (*okeMetadata, error) {
	metadata := &okeMetadata{
		NetworkBlockId: host.NetworkBlockId,
		RackId:         host.RackId,
		RDMAFabric:     fabric,
	}
	topo := host.RDMATopologyData
	if topo == nil {
		return metadata, nil
	}

	var err error
	metadata.HPCIslandId, err = ocidSuffix(topo.CustomerHPCIslandId)
	if err != nil {
		return nil, fmt.Errorf("invalid HPCIslandId: %w", err)
	}
	metadata.NetworkBlockId, err = ocidSuffix(topo.CustomerNetworkBlock)
	if err != nil {
		return nil, fmt.Errorf("invalid NetworkBlockId: %w", err)
	}
	metadata.LocalBlockId, err = ocidSuffix(topo.CustomerLocalBlock)
	if err != nil {
		return nil, fmt.Errorf("invalid LocalBlockId: %w", err)
	}
	metadata.GpuMemoryFabric, err = ocidSuffix(topo.CustomerGpuMemoryFabric)
	if err != nil {
		return nil, fmt.Errorf("invalid GpuMemoryFabric: %w", err)
	}
	return metadata, nil
}

func getRDMAFabricData(ctx context.Context, client *http.Client, endpoint string, host *imdsHostMetadata, required bool) *RDMAFabric {
	if required {
		direct, err := queryRDMAFabricData(ctx, client, endpoint+"/host/rdmaFabricData/")
		if err == nil {
			fabric, parseErr := parseRDMAFabricData(direct)
			if parseErr == nil && fabric != nil {
				return fabric
			}
			if parseErr != nil {
				klog.Warningf("Could not use OCI IMDS RDMA fabric endpoint: %v", parseErr)
			}
		} else {
			klog.Warningf("Could not query OCI IMDS RDMA fabric endpoint: %v", err)
		}
	}

	fabric, err := parseRDMAFabricData(host.RDMAFabricData)
	if err != nil {
		klog.Warningf("Could not use RDMA fabric data from OCI IMDS host metadata: %v", err)
		return nil
	}
	return fabric
}

func fetchOKEMetadata(ctx context.Context, client *http.Client, endpoint string, requiresFabricData bool) (*okeMetadata, error) {
	host, err := queryHostMetadata(ctx, client, endpoint+"/host/")
	if err != nil {
		return nil, err
	}

	fabric := getRDMAFabricData(ctx, client, endpoint, host, requiresFabricData)
	return metadataFromIMDS(host, fabric)
}

func mergeMetadata(current, next *okeMetadata) (*okeMetadata, bool) {
	if current == nil {
		return next, false
	}

	merged := *next
	if merged.HPCIslandId == "" {
		merged.HPCIslandId = current.HPCIslandId
	}
	if merged.NetworkBlockId == "" {
		merged.NetworkBlockId = current.NetworkBlockId
	}
	if merged.LocalBlockId == "" {
		merged.LocalBlockId = current.LocalBlockId
	}
	if merged.RackId == "" {
		merged.RackId = current.RackId
	}
	if merged.GpuMemoryFabric == "" {
		merged.GpuMemoryFabric = current.GpuMemoryFabric
	}

	familyChanged := false
	if merged.RDMAFabric == nil {
		merged.RDMAFabric = current.RDMAFabric
	} else if current.RDMAFabric != nil {
		fabric := *merged.RDMAFabric
		if fabric.IPv6 != current.RDMAFabric.IPv6 {
			familyChanged = true
			fabric.IPv6 = current.RDMAFabric.IPv6
		}
		merged.RDMAFabric = &fabric
	}
	return &merged, familyChanged
}

func (o *OKEInstance) refreshMetadata(ctx context.Context) error {
	if o.fetchMetadata == nil {
		return errors.New("OKE metadata fetcher is not configured")
	}

	next, err := o.fetchMetadata(ctx)
	if err != nil {
		return err
	}
	if next == nil {
		return errors.New("OCI IMDS returned empty metadata")
	}

	merged, familyChanged := mergeMetadata(o.metadata.Load(), next)
	if familyChanged {
		klog.Errorf("OCI IMDS changed the RDMA fabric IP version; keeping the initial value")
	}
	o.metadata.Store(merged)
	return nil
}

func (o *OKEInstance) metadataComplete() bool {
	metadata := o.metadata.Load()
	return metadata != nil && (!o.requiresFabricData.Load() || metadata.RDMAFabric != nil)
}

func (o *OKEInstance) requireFabricData() {
	if !o.requiresFabricData.CompareAndSwap(false, true) || o.metadataComplete() {
		return
	}
	select {
	case o.refreshMetadataNow <- struct{}{}:
	default:
	}
}

func (o *OKEInstance) refreshLoop(ctx context.Context) {
	retrying := !o.metadataComplete()
	retryInterval := o.retryInterval
	for {
		interval := o.refreshInterval
		if retrying {
			interval = retryInterval
		}

		timer := time.NewTimer(wait.Jitter(interval, 0.1))
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-o.refreshMetadataNow:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}

		requestCtx, cancel := context.WithTimeout(ctx, o.requestTimeout)
		err := o.refreshMetadata(requestCtx)
		cancel()
		if err != nil {
			klog.Warningf("Could not refresh OCI IMDS metadata; keeping the last known values: %v", err)
		}
		if err != nil || !o.metadataComplete() {
			retrying = true
			retryInterval *= 2
			if retryInterval > o.maxRetryInterval {
				retryInterval = o.maxRetryInterval
			}
			continue
		}
		retrying = false
		retryInterval = o.retryInterval
	}
}

// GetInstance reads OKE topology and RDMA fabric metadata from OCI IMDS.
// It refreshes the metadata and keeps the last complete fabric data.
func GetInstance(ctx context.Context) (cloudprovider.CloudInstance, error) {
	instance := newOKEInstance(nil, nil)
	instance.fetchMetadata = func(ctx context.Context) (*okeMetadata, error) {
		return fetchOKEMetadata(ctx, http.DefaultClient, imdsEndpoint, instance.requiresFabricData.Load())
	}
	instance.requiresFabricData.Store(hasEthernetFabricInterface())
	err := wait.PollUntilContextTimeout(ctx, imdsInitialRetryInterval, imdsInitialWait, true, func(ctx context.Context) (bool, error) {
		if err := instance.refreshMetadata(ctx); err != nil {
			klog.Infof("Could not get complete OCI IMDS metadata; retrying: %v", err)
			return false, nil
		}
		if !instance.metadataComplete() {
			klog.Infof("OCI IMDS RDMA fabric data is incomplete; retrying")
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		klog.Warningf("OCI IMDS metadata is not ready; continuing retries in the background: %v", err)
	}
	go instance.refreshLoop(ctx)
	return instance, nil
}
