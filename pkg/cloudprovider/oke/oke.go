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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	resourceapi "k8s.io/api/resource/v1"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
)

const (
	OKEAttrPrefix = "oke.dra.net"

	// RDMA topology attributes (from /opc/v2/host/).
	AttrOKEHPCIslandId     = OKEAttrPrefix + "/" + "hpcIslandId"
	AttrOKENetworkBlockId  = OKEAttrPrefix + "/" + "networkBlockId"
	AttrOKELocalBlockId    = OKEAttrPrefix + "/" + "localBlockId"
	AttrOKERackId          = OKEAttrPrefix + "/" + "rackId"
	AttrOKEGpuMemoryFabric = OKEAttrPrefix + "/" + "gpuMemoryFabricId"

	// Instance shape (from /opc/v2/instance/).
	AttrOKEShape = OKEAttrPrefix + "/" + "shape"

	// RDMA fabric attributes (from /opc/v2/host/rdmaFabricData/).
	AttrOKERDMAFabricIPv6   = OKEAttrPrefix + "/" + "rdmaFabricIpv6"
	AttrOKERDMAFabricPlanes = OKEAttrPrefix + "/" + "rdmaFabricPlanes"

	// imdsEndpoint is the Oracle Cloud Instance Metadata Service endpoint.
	imdsEndpoint = "http://169.254.169.254/opc/v2"

	imdsInitialRetryInterval  = 1 * time.Second
	imdsInitialWait           = 15 * time.Second
	imdsInitialRequestTimeout = 5 * time.Second
	imdsRetryInterval         = 10 * time.Second
	imdsRefreshInterval       = 15 * time.Minute
	imdsMaxRetryInterval      = imdsRefreshInterval
	imdsRequestTimeout        = 15 * time.Second

	// Oracle Cloud Agent names the RDMA fabric interfaces rdmaN.
	fabricInterfacePrefix = "rdma"
)

// Tests point these at a temporary directory.
var (
	sysClassNet     = "/sys/class/net"
	sysfsPCIDevices = "/sys/bus/pci/devices"
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

// imdsRDMAFabricData contains the RDMA fabric fields from /opc/v2/host/rdmaFabricData/.
// The same object is embedded in the /opc/v2/host/ response. Pointer fields
// keep a missing value apart from a valid false or 0.
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

// imdsInstanceMetadata contains the fields we care about from /opc/v2/instance/.
type imdsInstanceMetadata struct {
	Shape string `json:"shape"`
}

// rdmaFabric describes the RDMA fabric of the instance.
type rdmaFabric struct {
	IPv6   bool
	Planes int64
}

// okeMetadata is one immutable snapshot of the instance metadata.
type okeMetadata struct {
	HPCIslandId    string
	NetworkBlockId string
	LocalBlockId   string
	RackId         string
	// GpuMemoryFabric is only populated on shapes that use a GPU memory fabric
	// interconnect (e.g. BM.GPU.GB200, BM.GPU.GB300). It will be empty on all
	// other shapes such as BM.GPU.H100.8.
	GpuMemoryFabric string
	Shape           string
	RDMAFabric      *rdmaFabric
	// FabricConfirmed is true when RDMAFabric was read while fabric data was
	// required, so the direct endpoint was tried first.
	FabricConfirmed bool
}

type metadataFetcher func(context.Context) (*okeMetadata, error)

var _ cloudprovider.CloudInstance = (*OKEInstance)(nil)

// OKEInstance holds OCI/OKE specific instance metadata and refreshes it from
// IMDS in the background.
type OKEInstance struct {
	metadata      atomic.Pointer[okeMetadata]
	fetchMetadata metadataFetcher
	// requiresFabricData is set when the node has an Ethernet RDMA fabric
	// interface. Only then is the RDMA fabric data required for completion.
	requiresFabricData    atomic.Bool
	fabricDriftLogged     atomic.Bool
	metadataDriftLogged   atomic.Bool
	refreshMetadataNow    chan struct{}
	initialRetryInterval  time.Duration
	initialWait           time.Duration
	initialRequestTimeout time.Duration
	retryInterval         time.Duration
	maxRetryInterval      time.Duration
	refreshInterval       time.Duration
	requestTimeout        time.Duration
}

func newOKEInstance(metadata *okeMetadata, fetch metadataFetcher) *OKEInstance {
	instance := &OKEInstance{
		fetchMetadata:         fetch,
		refreshMetadataNow:    make(chan struct{}, 1),
		initialRetryInterval:  imdsInitialRetryInterval,
		initialWait:           imdsInitialWait,
		initialRequestTimeout: imdsInitialRequestTimeout,
		retryInterval:         imdsRetryInterval,
		maxRetryInterval:      imdsMaxRetryInterval,
		refreshInterval:       imdsRefreshInterval,
		requestTimeout:        imdsRequestTimeout,
	}
	if metadata != nil {
		instance.metadata.Store(metadata)
	}
	return instance
}

// GetDeviceAttributes returns OKE-specific attributes for a device.
// These are node-level attributes applied to all devices since the OCI IMDS
// host endpoint exposes per-node metadata, not per-NIC metadata.
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
	if metadata.Shape != "" {
		attributes[AttrOKEShape] = resourceapi.DeviceAttribute{StringValue: &metadata.Shape}
	}
	if metadata.RDMAFabric != nil {
		attributes[AttrOKERDMAFabricIPv6] = resourceapi.DeviceAttribute{BoolValue: &metadata.RDMAFabric.IPv6}
		attributes[AttrOKERDMAFabricPlanes] = resourceapi.DeviceAttribute{IntValue: &metadata.RDMAFabric.Planes}
	}

	return attributes
}

// ocidSuffix returns the unique identifier suffix of an OCI OCID, the segment
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

// GetDeviceConfig returns nil as OCI does not provide device-specific
// network configuration through IMDS. It also requests the RDMA fabric data
// when an Ethernet fabric interface appears after startup.
func (o *OKEInstance) GetDeviceConfig(id cloudprovider.DeviceIdentifiers) *apis.NetworkConfig {
	if !o.requiresFabricData.Load() && isEthernetFabricDevice(id) {
		o.requireFabricData()
	}
	return nil
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

// queryIMDS decodes the JSON response of one OCI IMDS v2 endpoint into out.
func queryIMDS(ctx context.Context, client *http.Client, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("could not create OCI IMDS request for %s: %w", url, err)
	}
	req.Header.Set("Authorization", "Bearer Oracle")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach OCI IMDS endpoint %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("OCI IMDS endpoint %s returned status %d", url, resp.StatusCode)
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("could not parse OCI IMDS response from %s: %w", url, err)
	}
	return nil
}

// parseRDMAFabricData returns nil without an error when the object is absent.
// An object that lacks either field is an error, so a partial object is never used.
func parseRDMAFabricData(data *imdsRDMAFabricData) (*rdmaFabric, error) {
	if data == nil {
		return nil, nil
	}
	if data.IPv6 == nil {
		return nil, errors.New("OCI IMDS RDMA fabric data does not contain ipv6")
	}
	if data.Planes == nil {
		return nil, errors.New("OCI IMDS RDMA fabric data does not contain planes")
	}
	return &rdmaFabric{IPv6: *data.IPv6, Planes: *data.Planes}, nil
}

func metadataFromIMDS(host *imdsHostMetadata, instance *imdsInstanceMetadata, fabric *rdmaFabric) (*okeMetadata, error) {
	metadata := &okeMetadata{
		NetworkBlockId: host.NetworkBlockId,
		RackId:         host.RackId,
		RDMAFabric:     fabric,
	}
	if instance != nil {
		metadata.Shape = instance.Shape
	}

	// rdmaTopologyData is absent on non-fabric shapes (H100, etc.).
	// Fall back to the top-level networkBlockId and rackId which are
	// available on all shapes that expose the /host/ endpoint.
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

// getRDMAFabricData prefers the direct endpoint when fabric data is required
// and falls back to the object embedded in the host response.
func getRDMAFabricData(ctx context.Context, client *http.Client, endpoint string, host *imdsHostMetadata, required bool) *rdmaFabric {
	if required {
		var direct imdsRDMAFabricData
		if err := queryIMDS(ctx, client, endpoint+"/host/rdmaFabricData/", &direct); err != nil {
			klog.V(2).Infof("Could not query OCI IMDS RDMA fabric endpoint: %v", err)
		} else if fabric, err := parseRDMAFabricData(&direct); err != nil {
			klog.V(2).Infof("Could not use OCI IMDS RDMA fabric endpoint: %v", err)
		} else if fabric != nil {
			return fabric
		}
	}

	fabric, err := parseRDMAFabricData(host.RDMAFabricData)
	if err != nil {
		klog.V(2).Infof("Could not use RDMA fabric data from OCI IMDS host metadata: %v", err)
		return nil
	}
	return fabric
}

// fetchOKEMetadata reads the host metadata first, then the required fabric
// data, and the optional instance metadata last.
func fetchOKEMetadata(ctx context.Context, client *http.Client, endpoint string, requiresFabricData bool) (*okeMetadata, error) {
	var host imdsHostMetadata
	if err := queryIMDS(ctx, client, endpoint+"/host/", &host); err != nil {
		return nil, err
	}
	fabric := getRDMAFabricData(ctx, client, endpoint, &host, requiresFabricData)

	instance := &imdsInstanceMetadata{}
	if err := queryIMDS(ctx, client, endpoint+"/instance/", instance); err != nil {
		klog.Warningf("Could not query OCI IMDS instance metadata: %v", err)
		instance = nil
	}

	metadata, err := metadataFromIMDS(&host, instance, fabric)
	if err == nil && fabric != nil {
		metadata.FabricConfirmed = requiresFabricData
	}
	return metadata, err
}

type metadataDrift struct {
	// fabric describes a rejected change to pinned fabric data, or is empty.
	fabric string
	// attributes lists the accepted changes to non-empty attribute values.
	attributes []string
}

// changedNonEmpty describes a change between two non-empty values, or returns "".
func changedNonEmpty(name, current, next string) string {
	if current == "" || next == "" || current == next {
		return ""
	}
	return fmt.Sprintf("%s from %q to %q", name, current, next)
}

// mergeMetadata keeps the last non-empty value for a field that next omits,
// accepts changed attribute values, and pins the first confirmed fabric data.
func mergeMetadata(current, next *okeMetadata) (*okeMetadata, metadataDrift) {
	if current == nil {
		return next, metadataDrift{}
	}

	merged := *next
	var drift metadataDrift
	for _, change := range []string{
		changedNonEmpty("hpcIslandId", current.HPCIslandId, merged.HPCIslandId),
		changedNonEmpty("networkBlockId", current.NetworkBlockId, merged.NetworkBlockId),
		changedNonEmpty("localBlockId", current.LocalBlockId, merged.LocalBlockId),
		changedNonEmpty("rackId", current.RackId, merged.RackId),
		changedNonEmpty("gpuMemoryFabricId", current.GpuMemoryFabric, merged.GpuMemoryFabric),
		changedNonEmpty("shape", current.Shape, merged.Shape),
	} {
		if change != "" {
			drift.attributes = append(drift.attributes, change)
		}
	}
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
	if merged.Shape == "" {
		merged.Shape = current.Shape
	}

	if merged.RDMAFabric == nil {
		merged.RDMAFabric = current.RDMAFabric
		merged.FabricConfirmed = current.FabricConfirmed
	} else if current.RDMAFabric != nil {
		if current.FabricConfirmed && *merged.RDMAFabric != *current.RDMAFabric {
			drift.fabric = fmt.Sprintf("from %+v to %+v", *current.RDMAFabric, *merged.RDMAFabric)
			fabric := *current.RDMAFabric
			merged.RDMAFabric = &fabric
			merged.FabricConfirmed = true
		} else if *merged.RDMAFabric == *current.RDMAFabric {
			merged.FabricConfirmed = current.FabricConfirmed || merged.FabricConfirmed
		}
	}
	return &merged, drift
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

	// GetInstance calls this before it starts refreshLoop. After startup,
	// refreshLoop is the only writer, so the read and write stay serialized.
	merged, drift := mergeMetadata(o.metadata.Load(), next)
	if drift.fabric != "" && o.fabricDriftLogged.CompareAndSwap(false, true) {
		klog.Errorf("OCI IMDS changed the RDMA fabric data %s; keeping the initial values", drift.fabric)
	}
	if len(drift.attributes) > 0 && o.metadataDriftLogged.CompareAndSwap(false, true) {
		klog.Warningf("OCI IMDS changed metadata fields (%s); accepting the new values", strings.Join(drift.attributes, ", "))
	}
	o.metadata.Store(merged)
	return nil
}

// metadataComplete reports whether the refresh loop can move from retrying
// to the slow refresh interval.
func (o *OKEInstance) metadataComplete() bool {
	metadata := o.metadata.Load()
	if metadata == nil {
		return false
	}
	if !o.requiresFabricData.Load() {
		return true
	}
	return metadata.RDMAFabric != nil && metadata.FabricConfirmed
}

// requireFabricData turns the fabric gate on and wakes the refresh loop.
func (o *OKEInstance) requireFabricData() {
	if !o.requiresFabricData.CompareAndSwap(false, true) {
		return
	}
	select {
	case o.refreshMetadataNow <- struct{}{}:
	default:
	}
}

// nextRetryInterval doubles the retry interval after a failed or incomplete
// refresh, up to the maximum, and resets it after a refresh that succeeded
// with complete metadata.
func (o *OKEInstance) nextRetryInterval(current time.Duration, succeeded bool) time.Duration {
	if succeeded {
		return o.retryInterval
	}
	return min(2*current, o.maxRetryInterval)
}

func (o *OKEInstance) refreshLoop(ctx context.Context) {
	retryInterval := o.retryInterval
	lastRefreshFailed := false
	for {
		interval := o.refreshInterval
		if lastRefreshFailed || !o.metadataComplete() {
			interval = retryInterval
		}

		timer := time.NewTimer(wait.Jitter(interval, 0.1))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-o.refreshMetadataNow:
			timer.Stop()
		case <-timer.C:
		}
		if ctx.Err() != nil {
			return
		}

		requestCtx, cancel := context.WithTimeout(ctx, o.requestTimeout)
		err := o.refreshMetadata(requestCtx)
		cancel()
		lastRefreshFailed = err != nil
		complete := o.metadataComplete()
		retryInterval = o.nextRetryInterval(retryInterval, err == nil && complete)
		if err != nil && ctx.Err() == nil {
			klog.Warningf("Could not refresh OCI IMDS metadata; keeping the last known values and retrying in %s: %v", retryInterval, err)
		} else if err == nil && !complete {
			klog.Warningf("OCI IMDS RDMA fabric data is not available; retrying in %s", retryInterval)
		}
	}
}

// GetInstance reads the OCI instance topology, shape, and RDMA fabric metadata
// from IMDS. It returns after the first complete read or after the startup
// window, and keeps refreshing the metadata in the background until ctx ends.
func GetInstance(ctx context.Context) (cloudprovider.CloudInstance, error) {
	instance, err := newOKEInstance(nil, nil).start(ctx, http.DefaultClient, imdsEndpoint)
	if err != nil {
		return nil, err
	}
	return instance, nil
}

// start seeds the fabric gate from sysfs, polls IMDS for the startup window,
// and runs the refresh loop until ctx ends.
func (o *OKEInstance) start(ctx context.Context, client *http.Client, endpoint string) (*OKEInstance, error) {
	o.fetchMetadata = func(ctx context.Context) (*okeMetadata, error) {
		return fetchOKEMetadata(ctx, client, endpoint, o.requiresFabricData.Load())
	}
	// GetDeviceConfig handles Ethernet fabric interfaces that appear later.
	o.requiresFabricData.Store(hasEthernetFabricInterface())

	var lastErr error
	err := wait.PollUntilContextTimeout(ctx, o.initialRetryInterval, o.initialWait, true, func(ctx context.Context) (bool, error) {
		requestCtx, cancel := context.WithTimeout(ctx, o.initialRequestTimeout)
		lastErr = o.refreshMetadata(requestCtx)
		cancel()
		if lastErr != nil {
			klog.Infof("could not get OCI IMDS metadata ... retrying: %v", lastErr)
			return false, nil
		}
		if !o.metadataComplete() {
			lastErr = errors.New("RDMA fabric data is not available")
			klog.Infof("OCI IMDS RDMA fabric data is not available ... retrying")
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if o.metadata.Load() == nil {
			klog.Warningf("OCI IMDS host metadata is not available after %s, retrying in the background. The host endpoint needs TopologyData enabled for the tenancy: %v", o.initialWait, lastErr)
		} else {
			klog.Warningf("OCI IMDS metadata is incomplete after %s, retrying in the background: %v", o.initialWait, lastErr)
		}
	}
	go o.refreshLoop(ctx)
	return o, nil
}

// isEthernetFabricDevice reports whether the device is an Ethernet rdmaN
// interface. It resolves the interface through the PCI address because the
// DRA device name can be a normalized form of the interface name.
func isEthernetFabricDevice(id cloudprovider.DeviceIdentifiers) bool {
	ifName, err := interfaceNameForPCIAddress(id.PCIAddress)
	if err != nil || !isFabricInterface(ifName) {
		return false
	}
	hardwareType, err := interfaceHardwareType(ifName)
	return err == nil && hardwareType == unix.ARPHRD_ETHER
}

func hasEthernetFabricInterface() bool {
	entries, err := os.ReadDir(sysClassNet)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !isFabricInterface(entry.Name()) {
			continue
		}
		hardwareType, err := interfaceHardwareType(entry.Name())
		if err == nil && hardwareType == unix.ARPHRD_ETHER {
			return true
		}
	}
	return false
}

// isFabricInterface reports whether ifName has the rdmaN form.
func isFabricInterface(ifName string) bool {
	index, ok := strings.CutPrefix(ifName, fabricInterfacePrefix)
	if !ok || index == "" {
		return false
	}
	for _, r := range index {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func interfaceHardwareType(ifName string) (int, error) {
	name := filepath.Join(sysClassNet, ifName, "type")
	data, err := os.ReadFile(name)
	if err != nil {
		return 0, fmt.Errorf("could not read %s: %w", name, err)
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("could not parse %s: %w", name, err)
	}
	return value, nil
}

func interfaceNameForPCIAddress(pciAddress string) (string, error) {
	if pciAddress == "" {
		return "", errors.New("device has no PCI address")
	}
	entries, err := os.ReadDir(filepath.Join(sysfsPCIDevices, pciAddress, "net"))
	if err != nil {
		return "", fmt.Errorf("could not read network interfaces for PCI device %s: %w", pciAddress, err)
	}
	if len(entries) != 1 {
		return "", fmt.Errorf("expected one network interface for PCI device %s, got %d", pciAddress, len(entries))
	}
	return entries[0].Name(), nil
}
