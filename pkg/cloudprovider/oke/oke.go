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
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	resourceapi "k8s.io/api/resource/v1"

	"sigs.k8s.io/dranet/pkg/cloudprovider"
)

const (
	OKEAttrPrefix = "oke.dra.net"

	// RDMA topology attributes (from /opc/v2/host/).
	AttrOKEHPCIslandId      = OKEAttrPrefix + "/" + "hpcIslandId"
	AttrOKENetworkBlockId   = OKEAttrPrefix + "/" + "networkBlockId"
	AttrOKELocalBlockId     = OKEAttrPrefix + "/" + "localBlockId"
	AttrOKERackId           = OKEAttrPrefix + "/" + "rackId"
	AttrOKEGpuMemoryFabric  = OKEAttrPrefix + "/" + "gpuMemoryFabricId"
	AttrOKERDMAFabricIPv6   = OKEAttrPrefix + "/" + "rdmaFabricIpv6"
	AttrOKERDMAFabricPlanes = OKEAttrPrefix + "/" + "rdmaFabricPlanes"
)

// RDMAFabric describes the RDMA fabric for this instance.
type RDMAFabric struct {
	IPv6   bool
	Planes int64
}

type okeMetadata struct {
	HPCIslandId     string
	NetworkBlockId  string
	LocalBlockId    string
	RackId          string
	GpuMemoryFabric string
	RDMAFabric      *RDMAFabric
}

type metadataFetcher func(context.Context) (*okeMetadata, error)

var _ cloudprovider.CloudInstance = (*OKEInstance)(nil)
var _ cloudprovider.ProfileProvider = (*OKEInstance)(nil)

// OKEInstance holds OCI/OKE specific instance topology data.
type OKEInstance struct {
	metadata           atomic.Pointer[okeMetadata]
	fetchMetadata      metadataFetcher
	requiresFabricData atomic.Bool
	refreshMetadataNow chan struct{}
	retryInterval      time.Duration
	maxRetryInterval   time.Duration
	refreshInterval    time.Duration
	requestTimeout     time.Duration
}

func newOKEInstance(metadata *okeMetadata, fetch metadataFetcher) *OKEInstance {
	instance := &OKEInstance{
		fetchMetadata:      fetch,
		refreshMetadataNow: make(chan struct{}, 1),
		retryInterval:      imdsRetryInterval,
		maxRetryInterval:   imdsMaxRetryInterval,
		refreshInterval:    imdsRefreshInterval,
		requestTimeout:     imdsRequestTimeout,
	}
	if metadata != nil {
		instance.metadata.Store(metadata)
	}
	return instance
}

// GetDeviceAttributes returns OKE-specific topology attributes for a device.
// These are node-level attributes applied to all devices since the OCI IMDS
// host endpoint exposes per-node topology, not per-NIC metadata.
func (o *OKEInstance) GetDeviceAttributes(id cloudprovider.DeviceIdentifiers) map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
	attributes := make(map[resourceapi.QualifiedName]resourceapi.DeviceAttribute)
	metadata := o.metadata.Load()
	if metadata == nil {
		return attributes
	}

	if metadata.HPCIslandId != "" {
		attributes[AttrOKEHPCIslandId] = resourceapi.DeviceAttribute{StringValue: &metadata.HPCIslandId}
	}
	if metadata.NetworkBlockId != "" {
		attributes[AttrOKENetworkBlockId] = resourceapi.DeviceAttribute{StringValue: &metadata.NetworkBlockId}
	}
	if metadata.LocalBlockId != "" {
		attributes[AttrOKELocalBlockId] = resourceapi.DeviceAttribute{StringValue: &metadata.LocalBlockId}
	}
	if metadata.RackId != "" {
		attributes[AttrOKERackId] = resourceapi.DeviceAttribute{StringValue: &metadata.RackId}
	}
	if metadata.GpuMemoryFabric != "" {
		attributes[AttrOKEGpuMemoryFabric] = resourceapi.DeviceAttribute{StringValue: &metadata.GpuMemoryFabric}
	}
	if metadata.RDMAFabric != nil {
		attributes[AttrOKERDMAFabricIPv6] = resourceapi.DeviceAttribute{BoolValue: &metadata.RDMAFabric.IPv6}
		attributes[AttrOKERDMAFabricPlanes] = resourceapi.DeviceAttribute{IntValue: &metadata.RDMAFabric.Planes}
	}

	return attributes
}

// ocidSuffix returns the unique identifier suffix of an OCI OCID — the segment
// after the last '.'. DRA string attributes are capped at 64 bytes, but full
// OCIDs are ~90+ characters; the suffix is always 60 characters and is unique
// per resource within a tenancy, making it safe to use as an attribute value.
func ocidSuffix(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	if !strings.Contains(s, "ocid") {
		return "", fmt.Errorf("not a valid OCID (missing 'ocid' prefix): %q", s)
	}
	i := strings.LastIndex(s, ".")
	if i < 0 {
		return "", fmt.Errorf("not a valid OCID (missing '.' separator): %q", s)
	}
	suffix := s[i+1:]
	if len(suffix) > 60 {
		suffix = suffix[len(suffix)-60:]
	}
	return suffix, nil
}
