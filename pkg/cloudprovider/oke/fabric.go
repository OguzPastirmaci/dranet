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
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	"sigs.k8s.io/dranet/pkg/apis"
	"sigs.k8s.io/dranet/pkg/cloudprovider"
)

const (
	fabricInterfacePrefix     = "rdma"
	okeRDMAIPv4Profile        = "oke-rdma-ipv4"
	okeRDMASBRTableBase       = 100
	okeRDMARailPrefixBits     = 19
	arpHardwareTypeEthernet   = 1
	arpHardwareTypeInfiniBand = 32

	// OCA assigns B4 RDMA parent addresses from this /15. DRANET uses a
	// separate /15 for one deterministic IPvlan child per parent.
	okeRDMAParentIPv4CIDR = "10.224.0.0/15"
	okeRDMAChildIPv4CIDR  = "10.240.0.0/15"
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
	hardwareType, err := interfaceHardwareType(ifName)
	if err != nil {
		klog.Warningf("Could not read OKE interface type for device %s: %v", id.Name, err)
		return config
	}
	if hardwareType == arpHardwareTypeInfiniBand {
		return nil
	}
	if hardwareType != arpHardwareTypeEthernet {
		klog.Warningf("OKE interface %s for device %s has unsupported ARPHRD type %d", ifName, id.Name, hardwareType)
		return config
	}
	o.requireFabricData()
	metadata := o.metadata.Load()
	if metadata == nil || metadata.RDMAFabric == nil {
		klog.Warningf("OKE RDMA fabric data is not available for device %s", id.Name)
		return config
	}
	if metadata.RDMAFabric.IPv6 {
		config.Profile = ""
		config.SubInterface.AddressMode = apis.SubInterfaceAddressModeSLAAC
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
	hardwareType, err := interfaceHardwareType(ifName)
	if err != nil {
		return nil, err
	}
	if hardwareType != arpHardwareTypeEthernet {
		return nil, fmt.Errorf("OKE profile %q requires an Ethernet parent, but %s has ARPHRD type %d", config.Profile, ifName, hardwareType)
	}
	railIndex, err := fabricInterfaceIndex(ifName)
	if err != nil {
		return nil, err
	}

	parentAddr, err := parentIPv4Address(ifName)
	if err != nil {
		return nil, err
	}
	if err := validateClassicRailLayout(parentAddr, railIndex); err != nil {
		return nil, fmt.Errorf("could not derive OKE child address for %s: %w", ifName, err)
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

func validateClassicRailLayout(address netip.Addr, railIndex int) error {
	parentRange := netip.MustParsePrefix(okeRDMAParentIPv4CIDR).Masked()
	if !address.Is4() || !parentRange.Contains(address) {
		return fmt.Errorf("address %s is outside classic OCA range %s", address, parentRange)
	}

	addressBytes := address.As4()
	baseBytes := parentRange.Addr().As4()
	offset := binary.BigEndian.Uint32(addressBytes[:]) - binary.BigEndian.Uint32(baseBytes[:])
	railBlockSize := uint32(1 << (32 - okeRDMARailPrefixBits))
	addressRailIndex := int(offset / railBlockSize)
	if addressRailIndex != railIndex {
		return fmt.Errorf("address %s belongs to classic OCA rail %d, not rail %d", address, addressRailIndex, railIndex)
	}
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
		if err == nil && hardwareType == arpHardwareTypeEthernet {
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
