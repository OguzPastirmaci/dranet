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
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	okeRDMAMaxRailIndex   = 15

	// OCA assigns B4 RDMA parent addresses from this /15. DRANET uses a
	// separate /15 for one deterministic IPvlan child per parent.
	okeRDMAParentIPv4CIDR = "10.224.0.0/15"
	okeRDMAChildIPv4CIDR  = "10.240.0.0/15"

	// RDMA topology attributes (from /opc/v2/host/).
	AttrOKEHPCIslandId     = OKEAttrPrefix + "/" + "hpcIslandId"
	AttrOKENetworkBlockId  = OKEAttrPrefix + "/" + "networkBlockId"
	AttrOKELocalBlockId    = OKEAttrPrefix + "/" + "localBlockId"
	AttrOKERackId          = OKEAttrPrefix + "/" + "rackId"
	AttrOKEGpuMemoryFabric = OKEAttrPrefix + "/" + "gpuMemoryFabricId"

	// imdsEndpoint is the Oracle Cloud Instance Metadata Service endpoint.
	imdsEndpoint = "http://169.254.169.254/opc/v2"
)

var (
	sysfsPCIDevices    = "/sys/bus/pci/devices"
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

// imdsHostMetadata contains the fields we care about from the OCI IMDS
// host metadata response at /opc/v2/host/.
type imdsHostMetadata struct {
	NetworkBlockId   string                    `json:"networkBlockId"`
	RackId           string                    `json:"rackId"`
	RDMATopologyData *imdsHostRDMATopologyData `json:"rdmaTopologyData"`
}

var _ cloudprovider.CloudInstance = (*OKEInstance)(nil)
var _ cloudprovider.ProfileProvider = (*OKEInstance)(nil)

// OKEInstance holds OCI/OKE specific instance topology data.
type OKEInstance struct {
	HPCIslandId    string
	NetworkBlockId string
	LocalBlockId   string
	RackId         string
	// GpuMemoryFabric is only populated on shapes that use a GPU memory fabric
	// interconnect (e.g. BM.GPU.GB200, BM.GPU.GB300). It will be empty on all
	// other shapes such as BM.GPU.H100.8.
	GpuMemoryFabric string
}

// GetDeviceAttributes returns OKE-specific topology attributes for a device.
// These are node-level attributes applied to all devices since the OCI IMDS
// host endpoint exposes per-node topology, not per-NIC metadata.
func (o *OKEInstance) GetDeviceAttributes(id cloudprovider.DeviceIdentifiers) map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
	attributes := make(map[resourceapi.QualifiedName]resourceapi.DeviceAttribute)

	if o.HPCIslandId != "" {
		attributes[AttrOKEHPCIslandId] = resourceapi.DeviceAttribute{StringValue: &o.HPCIslandId}
	}
	if o.NetworkBlockId != "" {
		attributes[AttrOKENetworkBlockId] = resourceapi.DeviceAttribute{StringValue: &o.NetworkBlockId}
	}
	if o.LocalBlockId != "" {
		attributes[AttrOKELocalBlockId] = resourceapi.DeviceAttribute{StringValue: &o.LocalBlockId}
	}
	if o.RackId != "" {
		attributes[AttrOKERackId] = resourceapi.DeviceAttribute{StringValue: &o.RackId}
	}
	if o.GpuMemoryFabric != "" {
		attributes[AttrOKEGpuMemoryFabric] = resourceapi.DeviceAttribute{StringValue: &o.GpuMemoryFabric}
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

// GetDeviceConfig keeps OKE fabric interfaces on the host. It selects the
// IPv4 child profile and applies the parent ARP policy to the IPvlan child.
func (o *OKEInstance) GetDeviceConfig(id cloudprovider.DeviceIdentifiers) *apis.NetworkConfig {
	ifName, err := interfaceNameForPCIAddress(id.PCIAddress)
	if err != nil || !isFabricInterface(ifName) {
		return nil
	}

	config := &apis.NetworkConfig{
		Profile:      okeRDMAIPv4Profile,
		SubInterface: &apis.SubInterfaceConfig{Type: apis.SubInterfaceTypeIPVlan},
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

	ifName, err := interfaceNameForPCIAddress(id.PCIAddress)
	if err != nil {
		return nil, err
	}
	railIndex, err := fabricInterfaceIndex(ifName)
	if err != nil {
		return nil, err
	}
	if railIndex > okeRDMAMaxRailIndex {
		return nil, fmt.Errorf("OKE IPv4 profile supports rdma0 through rdma%d, got %q", okeRDMAMaxRailIndex, ifName)
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

// GetInstance retrieves OCI instance topology by querying the IMDS host endpoint.
// On shapes with RDMA topology data (GB200, GB300), all five topology attributes
// are populated. On shapes without it (H100, etc.), only NetworkBlockId and
// RackId are available from the host metadata.
func GetInstance(ctx context.Context) (cloudprovider.CloudInstance, error) {
	var instance *OKEInstance
	err := wait.PollUntilContextTimeout(ctx, 1*time.Second, 15*time.Second, true, func(ctx context.Context) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, imdsEndpoint+"/host/", nil)
		if err != nil {
			klog.Infof("could not create OCI IMDS host request ... retrying: %v", err)
			return false, nil
		}
		req.Header.Set("Authorization", "Bearer Oracle")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			klog.Infof("could not reach OCI IMDS host endpoint ... retrying: %v", err)
			return false, nil
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			klog.Infof("OCI IMDS host endpoint returned status %d ... retrying", resp.StatusCode)
			return false, nil
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			klog.Infof("could not read OCI IMDS host response ... retrying: %v", err)
			return false, nil
		}

		var metadata imdsHostMetadata
		if err := json.Unmarshal(body, &metadata); err != nil {
			return false, fmt.Errorf("could not parse OCI IMDS host response: %w", err)
		}

		// rdmaTopologyData is absent on non-fabric shapes (H100, etc.).
		// Fall back to the top-level networkBlockId and rackId which are
		// available on all shapes that expose the /host/ endpoint.
		topo := metadata.RDMATopologyData
		if topo == nil {
			instance = &OKEInstance{
				NetworkBlockId: metadata.NetworkBlockId,
				RackId:         metadata.RackId,
			}
			return true, nil
		}

		hpcIslandId, err := ocidSuffix(topo.CustomerHPCIslandId)
		if err != nil {
			return false, fmt.Errorf("invalid HPCIslandId: %w", err)
		}
		networkBlockId, err := ocidSuffix(topo.CustomerNetworkBlock)
		if err != nil {
			return false, fmt.Errorf("invalid NetworkBlockId: %w", err)
		}
		localBlockId, err := ocidSuffix(topo.CustomerLocalBlock)
		if err != nil {
			return false, fmt.Errorf("invalid LocalBlockId: %w", err)
		}
		gpuMemoryFabric, err := ocidSuffix(topo.CustomerGpuMemoryFabric)
		if err != nil {
			return false, fmt.Errorf("invalid GpuMemoryFabric: %w", err)
		}

		instance = &OKEInstance{
			HPCIslandId:     hpcIslandId,
			NetworkBlockId:  networkBlockId,
			LocalBlockId:    localBlockId,
			RackId:          metadata.RackId,
			GpuMemoryFabric: gpuMemoryFabric,
		}
		return true, nil
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("please enable TopologyData for your tenancy: %w", err)
		}
		return nil, err
	}
	return instance, nil
}
