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
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	resourceapi "k8s.io/api/resource/v1"
	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
)

const (
	OKEAttrPrefix = "oke.dra.net"

	fabricInterfacePrefix = "rdma"
	okeRDMAIPv4Profile    = "oke-rdma-ipv4"
	okeRDMASBRTableBase   = 100

	// OCA assigns B4 RDMA parent addresses from this /15. DRANET uses a
	// separate /15 for one deterministic IPvlan child per parent.
	okeRDMAParentIPv4CIDR = "10.224.0.0/15"
	okeRDMAChildIPv4CIDR  = "10.240.0.0/15"

	// RDMA topology attributes (from /opc/v2/host/).
	AttrOKEHPCIslandId      = OKEAttrPrefix + "/" + "hpcIslandId"
	AttrOKENetworkBlockId   = OKEAttrPrefix + "/" + "networkBlockId"
	AttrOKELocalBlockId     = OKEAttrPrefix + "/" + "localBlockId"
	AttrOKERackId           = OKEAttrPrefix + "/" + "rackId"
	AttrOKEGpuMemoryFabric  = OKEAttrPrefix + "/" + "gpuMemoryFabricId"
	AttrOKERDMAFabricIPv6   = OKEAttrPrefix + "/" + "rdmaFabricIpv6"
	AttrOKERDMAFabricPlanes = OKEAttrPrefix + "/" + "rdmaFabricPlanes"

	// imdsEndpoint is the Oracle Cloud Instance Metadata Service endpoint.
	imdsEndpoint = "http://169.254.169.254/opc/v2"

	imdsInitialRetryInterval = 1 * time.Second
	imdsInitialWait          = 15 * time.Second
	imdsRetryInterval        = 10 * time.Second
	imdsMaxRetryInterval     = 30 * time.Second
	imdsRefreshInterval      = 30 * time.Minute
	imdsRequestTimeout       = 15 * time.Second
)

var (
	sysfsPCIDevices    = "/sys/bus/pci/devices"
	sysClassNet        = "/sys/class/net"
	procSysNetIPv4Conf = "/proc/sys/net/ipv4/conf"
	interfaceAddresses = func(ifName string) ([]net.Addr, error) {
		iface, err := net.InterfaceByName(ifName)
		if err != nil {
			return nil, err
		}
		return iface.Addrs()
	}
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
	metadata         atomic.Pointer[okeMetadata]
	fetchMetadata    metadataFetcher
	retryInterval    time.Duration
	maxRetryInterval time.Duration
	refreshInterval  time.Duration
	requestTimeout   time.Duration
}

func newOKEInstance(metadata *okeMetadata, fetch metadataFetcher) *OKEInstance {
	instance := &OKEInstance{
		fetchMetadata:    fetch,
		retryInterval:    imdsRetryInterval,
		maxRetryInterval: imdsMaxRetryInterval,
		refreshInterval:  imdsRefreshInterval,
		requestTimeout:   imdsRequestTimeout,
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

// GetDeviceConfig keeps OKE fabric interfaces on the host. IPv4 fabrics use
// deterministic addresses. IPv6 fabrics use address-free children for SLAAC.
func (o *OKEInstance) GetDeviceConfig(id cloudprovider.DeviceIdentifiers) *apis.NetworkConfig {
	ifName, err := interfaceNameForPCIAddress(id.PCIAddress)
	if err != nil || !isFabricInterface(ifName) {
		return nil
	}

	config := &apis.NetworkConfig{
		Profile:      okeRDMAIPv4Profile,
		SubInterface: &apis.SubInterfaceConfig{Type: apis.SubInterfaceTypeIPVlan},
	}
	metadata := o.metadata.Load()
	if metadata == nil || metadata.RDMAFabric == nil {
		klog.Warningf("OKE RDMA fabric data is not available for device %s", id.Name)
		return config
	}
	if metadata.RDMAFabric.IPv6 {
		config.Profile = ""
		return config
	}

	arpIgnore, err := readARPSysctl(ifName, "arp_ignore")
	if err != nil {
		klog.Warningf("Could not read OKE arp_ignore for device %s: %v", id.Name, err)
	} else {
		config.Interface.ARPIgnore = arpIgnore
	}

	arpAnnounce, err := readARPSysctl(ifName, "arp_announce")
	if err != nil {
		klog.Warningf("Could not read OKE arp_announce for device %s: %v", id.Name, err)
	} else {
		config.Interface.ARPAnnounce = arpAnnounce
	}

	return config
}

// GetProfileConfig derives one IPv4 address for an IPvlan child from the
// unique OCA address on its RDMA parent.
func (o *OKEInstance) GetProfileConfig(id cloudprovider.DeviceIdentifiers, _ types.UID, config *apis.NetworkConfig) (*apis.NetworkConfig, error) {
	if config == nil {
		return nil, errors.New("OKE profile configuration is required")
	}
	if config.Profile != okeRDMAIPv4Profile {
		return nil, fmt.Errorf("unsupported OKE profile %q", config.Profile)
	}
	if config.SubInterface == nil || config.SubInterface.Type != apis.SubInterfaceTypeIPVlan {
		return nil, fmt.Errorf("OKE profile %q requires an ipvlan subinterface", config.Profile)
	}
	metadata := o.metadata.Load()
	if metadata == nil || metadata.RDMAFabric == nil {
		return nil, errors.New("OKE RDMA fabric data is not available")
	}
	if metadata.RDMAFabric.IPv6 {
		return nil, fmt.Errorf("OKE profile %q does not support an IPv6 RDMA fabric", config.Profile)
	}

	ifName, err := interfaceNameForPCIAddress(id.PCIAddress)
	if err != nil {
		return nil, err
	}
	railIndex, err := fabricInterfaceIndex(ifName)
	if err != nil {
		return nil, err
	}

	parentAddr, err := parentIPv4Address(ifName)
	if err != nil {
		return nil, err
	}
	childAddr, err := translateIPv4Address(
		parentAddr,
		netip.MustParsePrefix(okeRDMAParentIPv4CIDR),
		netip.MustParsePrefix(okeRDMAChildIPv4CIDR),
	)
	if err != nil {
		return nil, fmt.Errorf("could not derive OKE child address for %s: %w", ifName, err)
	}

	childPrefix := netip.PrefixFrom(childAddr, netip.MustParsePrefix(okeRDMAChildIPv4CIDR).Bits())
	sourcePrefix := netip.PrefixFrom(childAddr, childAddr.BitLen())
	table := okeRDMASBRTableBase + railIndex

	return &apis.NetworkConfig{
		SubInterface: &apis.SubInterfaceConfig{
			Addresses: []string{childPrefix.String()},
		},
		Routes: []apis.RouteConfig{{
			Destination: okeRDMAChildIPv4CIDR,
			Source:      childAddr.String(),
			Scope:       253,
			Table:       table,
		}},
		Rules: []apis.RuleConfig{{
			Priority: 32000,
			Source:   sourcePrefix.String(),
			Table:    table,
		}},
	}, nil
}

// ReleaseProfileConfig has no work because the child address is deterministic.
func (o *OKEInstance) ReleaseProfileConfig(cloudprovider.DeviceIdentifiers, types.UID, *apis.NetworkConfig) error {
	return nil
}

func parentIPv4Address(ifName string) (netip.Addr, error) {
	addresses, err := interfaceAddresses(ifName)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("could not read addresses for %s: %w", ifName, err)
	}
	parentRange := netip.MustParsePrefix(okeRDMAParentIPv4CIDR)
	for _, address := range addresses {
		prefix, err := netip.ParsePrefix(address.String())
		if err == nil && prefix.Addr().Is4() && parentRange.Contains(prefix.Addr()) {
			return prefix.Addr(), nil
		}
	}
	return netip.Addr{}, fmt.Errorf("interface %s has no IPv4 address in %s", ifName, parentRange)
}

func translateIPv4Address(address netip.Addr, source, target netip.Prefix) (netip.Addr, error) {
	if !address.Is4() || !source.Addr().Is4() || !target.Addr().Is4() {
		return netip.Addr{}, errors.New("address ranges must use IPv4")
	}
	if source.Bits() != target.Bits() {
		return netip.Addr{}, fmt.Errorf("source prefix length %d does not match target prefix length %d", source.Bits(), target.Bits())
	}
	source = source.Masked()
	target = target.Masked()
	if !source.Contains(address) {
		return netip.Addr{}, fmt.Errorf("address %s is outside source range %s", address, source)
	}

	addressBytes := address.As4()
	sourceBytes := source.Addr().As4()
	targetBytes := target.Addr().As4()
	offset := binary.BigEndian.Uint32(addressBytes[:]) - binary.BigEndian.Uint32(sourceBytes[:])
	value := binary.BigEndian.Uint32(targetBytes[:]) + offset
	var translatedBytes [4]byte
	binary.BigEndian.PutUint32(translatedBytes[:], value)
	translated := netip.AddrFrom4(translatedBytes)
	if !target.Contains(translated) {
		return netip.Addr{}, fmt.Errorf("translated address %s is outside target range %s", translated, target)
	}
	return translated, nil
}

func isFabricInterface(ifName string) bool {
	_, err := fabricInterfaceIndex(ifName)
	return err == nil
}

func hasFabricInterface() bool {
	entries, err := os.ReadDir(sysClassNet)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if isFabricInterface(entry.Name()) {
			return true
		}
	}
	return false
}

func fabricInterfaceIndex(ifName string) (int, error) {
	index, ok := strings.CutPrefix(ifName, fabricInterfacePrefix)
	if !ok || index == "" {
		return 0, fmt.Errorf("network interface %q does not use the rdmaN name format", ifName)
	}
	for _, r := range index {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("network interface %q does not use the rdmaN name format", ifName)
		}
	}
	value, err := strconv.Atoi(index)
	if err != nil {
		return 0, fmt.Errorf("could not parse RDMA rail index from %q: %w", ifName, err)
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

func readARPSysctl(ifName, setting string) (*int32, error) {
	name := filepath.Join(procSysNetIPv4Conf, ifName, setting)
	data, err := os.ReadFile(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("could not read %s: %w", name, err)
	}

	value, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 32)
	if err != nil {
		return nil, fmt.Errorf("could not parse %s: %w", name, err)
	}
	if setting == "arp_ignore" && value != 0 && value != 1 && value != 2 && value != 3 && value != 8 {
		return nil, fmt.Errorf("%s has unsupported value %d", name, value)
	}
	if setting == "arp_announce" && (value < 0 || value > 2) {
		return nil, fmt.Errorf("%s has unsupported value %d", name, value)
	}

	result := int32(value)
	return &result, nil
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

func metadataFromIMDS(host *imdsHostMetadata, fabricData *imdsRDMAFabricData) (*okeMetadata, error) {
	fabric, err := parseRDMAFabricData(fabricData)
	if err != nil {
		return nil, err
	}

	metadata := &okeMetadata{
		NetworkBlockId: host.NetworkBlockId,
		RackId:         host.RackId,
		RDMAFabric:     fabric,
	}
	topo := host.RDMATopologyData
	if topo == nil {
		return metadata, nil
	}

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

func fetchOKEMetadata(ctx context.Context, client *http.Client, endpoint string) (*okeMetadata, error) {
	host, err := queryHostMetadata(ctx, client, endpoint+"/host/")
	if err != nil {
		return nil, err
	}

	fabricData := host.RDMAFabricData
	if hasFabricInterface() {
		fabricData, err = queryRDMAFabricData(ctx, client, endpoint+"/host/rdmaFabricData/")
		if err != nil {
			return nil, err
		}
		if fabricData == nil {
			return nil, errors.New("OCI IMDS RDMA fabric response is empty")
		}
	}
	return metadataFromIMDS(host, fabricData)
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

func (o *OKEInstance) refreshLoop(ctx context.Context) {
	retrying := o.metadata.Load() == nil
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
		case <-timer.C:
		}

		requestCtx, cancel := context.WithTimeout(ctx, o.requestTimeout)
		err := o.refreshMetadata(requestCtx)
		cancel()
		if err != nil {
			klog.Warningf("Could not refresh OCI IMDS metadata; keeping the last known values: %v", err)
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

// GetInstance retrieves OCI instance topology by querying the IMDS host endpoint.
// On shapes with RDMA topology data (GB200, GB300), all five topology attributes
// are populated. On shapes without it (H100, etc.), only NetworkBlockId and
// RackId are available from the host metadata.
func GetInstance(ctx context.Context) (cloudprovider.CloudInstance, error) {
	instance := newOKEInstance(nil, func(ctx context.Context) (*okeMetadata, error) {
		return fetchOKEMetadata(ctx, http.DefaultClient, imdsEndpoint)
	})
	err := wait.PollUntilContextTimeout(ctx, imdsInitialRetryInterval, imdsInitialWait, true, func(ctx context.Context) (bool, error) {
		if err := instance.refreshMetadata(ctx); err != nil {
			klog.Infof("Could not get complete OCI IMDS metadata; retrying: %v", err)
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
