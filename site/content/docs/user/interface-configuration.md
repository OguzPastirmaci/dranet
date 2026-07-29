---
title: "Interface Configuration"
date: 2025-05-25T11:30:40Z
---

To configure network interfaces in DRANET, users can provide custom configurations through the parameters field of a ResourceClaim or ResourceClaimTemplate. This configuration adheres to the NetworkConfig structure, which defines the desired state for network interfaces and their associated routes.

### Network Configuration Overview

The primary structure for custom network configuration is NetworkConfig. It encompasses settings for the network interface itself and any specific routes and rules to be applied within the Pod's network namespace.

```go
type NetworkConfig struct {
	// Interface defines core properties of the network interface.
	// Settings here are typically managed by `ip link` commands.
	Interface InterfaceConfig `json:"interface"`

	// Routes defines static routes to be configured for this interface.
	Routes []RouteConfig `json:"routes,omitempty"`

	// Rules defines routing rules to be configured for this interface.
	Rules []RuleConfig `json:"rules,omitempty"`

	// Neighbors defines permanent neighbor (ARP/NDP) entries to be added for this interface.
	Neighbors []NeighborConfig `json:"neighbors,omitempty"`

	// Ethtool defines hardware offload features and other settings managed by `ethtool`.
	Ethtool *EthtoolConfig `json:"ethtool,omitempty"`
}
```

#### Interface Configuration

The InterfaceConfig structure allows you to specify details for a single network interface.

```go
type InterfaceConfig struct {
	// Name is the desired logical name of the interface inside the Pod (e.g., "net0", "eth_app").
	// If not specified, DRANET may use or derive a name from the original interface.
	Name string `json:"name,omitempty"`

	// Addresses is a list of IP addresses in CIDR format (e.g., "192.168.1.10/24")
	// to be assigned to the interface.
	Addresses []string `json:"addresses,omitempty"`

	// MTU is the Maximum Transmission Unit for the interface.
	MTU *int32 `json:"mtu,omitempty"`

	// HardwareAddr is the MAC address of the interface.
	HardwareAddr *string `json:"hardwareAddr,omitempty"`

	// GSOMaxSize sets the maximum Generic Segmentation Offload size for IPv6.
	// Managed by `ip link set <dev> gso_max_size <val>`. For enabling Big TCP.
	GSOMaxSize *int32 `json:"gsoMaxSize,omitempty"`

	// GROMaxSize sets the maximum Generic Receive Offload size for IPv6.
	// Managed by `ip link set <dev> gro_max_size <val>`. For enabling Big TCP.
	GROMaxSize *int32 `json:"groMaxSize,omitempty"`

	// GSOv4MaxSize sets the maximum Generic Segmentation Offload size.
	// Managed by `ip link set <dev> gso_ipv4_max_size <val>`. For enabling Big TCP.
	GSOIPv4MaxSize *int32 `json:"gsoIPv4MaxSize,omitempty"`

	// GROv4MaxSize sets the maximum Generic Receive Offload size.
	// Managed by `ip link set <dev> gro_ipv4_max_size <val>`. For enabling Big TCP.
	GROIPv4MaxSize *int32 `json:"groIPv4MaxSize,omitempty"`
}
```

* **name** (string, optional): The logical name that the interface will have inside the Pod (e.g., "eth0", "enp0s3"). If not specified, DRANET will keep the original name if compliant.
* **addresses** ([]string, optional): A list of IP addresses in CIDR format (e.g., "192.168.1.10/24", "2001:db8::1/64") to be assigned to the interface.
* **mtu** (int32, optional): The Maximum Transmission Unit for the interface.
* **hardwareAddr** (string, optional): The MAC address of the interface.
* **gsoMaxSize** (int32, optional): The maximum Generic Segmentation Offload size for IPv6.
* **groMaxSize** (int32, optional): The maximum Generic Receive Offload size for IPv6.
* **gsoIPv4MaxSize** (int32, optional): The maximum Generic Segmentation Offload size for IPv4.
* **groIPv4MaxSize** (int32, optional): The maximum Generic Receive Offload size for IPv4.

#### Route Configuration (RouteConfig)

The RouteConfig structure defines individual network routes to be added to the Pod's network namespace, associated with the configured interface.

```go
type RouteConfig struct {
	Destination string `json:"destination,omitempty"`
	Gateway     string `json:"gateway,omitempty"`
	Source      string `json:"source,omitempty"`
	Scope       uint8  `json:"scope,omitempty"`
	Metric      int    `json:"metric,omitempty"`
	Table       int    `json:"table,omitempty"`
}
```

* **destination** (string, optional): The destination network in CIDR format (e.g., "0.0.0.0/0" for a default route, "10.0.0.0/8" for a specific subnet).  
* **gateway** (string, optional): The IP address of the gateway for the route. This field is mandatory for routes with Universe scope (0).  
* **source** (string, optional): An optional source IP address for policy routing.  
* **scope** (uint8, optional): The scope of the route. Only Link (253) or Universe (0) are allowed.  
  * Link (253): Routes directly to a device without a gateway (e.g., for directly connected subnets).  
  * Universe (0): Routes to a network via a gateway.
* **metric** (int, optional): The route metric. Lower values are preferred. The value must be non-negative and defaults to 0.
* **table** (int, optional): The routing table to use for the route. Defaults to the main table (254) if not specified.

#### Rule Configuration (RuleConfig)

The RuleConfig structure defines individual routing rules to be added to the Pod's network namespace.

```go
type RuleConfig struct {
	// Priority is the priority of the rule.
	Priority int `json:"priority,omitempty"`
	// Family is the address family for the rule.
	Family int `json:"family,omitempty"`
	// Protocol identifies the origin of the rule.
	Protocol uint8 `json:"protocol,omitempty"`
	// Source is the source IP address for the rule.
	Source string `json:"source,omitempty"`
	// Destination is the destination IP address for the rule.
	Destination string `json:"destination,omitempty"`
	// IifName is the input interface name for the rule.
	IifName string `json:"iifName,omitempty"`
	// OifName is the output interface name for the rule.
	OifName string `json:"oifName,omitempty"`
	// Table is the routing table ID to look up if the rule matches.
	Table int `json:"table,omitempty"`
}
```

* **priority** (int, optional): The priority of the rule. Lower values mean higher priority. Supported values are 0 through 32767, and the field defaults to 0.
* **family** (int, optional): The address family. Supported values are 0 for unspecified, 2 for IPv4 (`AF_INET`), and 10 for IPv6 (`AF_INET6`). The source and destination must match an explicitly selected family.
* **protocol** (uint8, optional): The routing protocol that identifies the origin of the rule.
* **source** (string, optional): The source IP address or CIDR for the rule (e.g., "192.168.1.0/24").
* **destination** (string, optional): The destination IP address or CIDR for the rule (e.g., "10.0.0.0/8").
* **iifName** (string, optional): The input interface name that the rule matches.
* **oifName** (string, optional): The output interface name that the rule matches.
* **table** (int, optional): The non-negative routing table ID to look up if the rule matches.

#### Automatic Host Interface State Preservation

For each allocated network interface, DRANET captures the live host configuration before moving the interface into the Pod's network namespace. This behavior does not depend on a cloud provider, instance shape, or hard-coded values.

DRANET captures and checkpoints these per-interface IPv4 sysctls when they exist:

* `net.ipv4.conf.<interface>.rp_filter`
* `net.ipv4.conf.<interface>.arp_ignore`
* `net.ipv4.conf.<interface>.arp_announce`
* `net.ipv4.conf.<interface>.accept_local`
* `net.ipv4.conf.<interface>.arp_filter`

The captured values are applied to the interface in the Pod's network namespace. DRANET also carries supported host routes for the interface into the Pod, including the destination, gateway, source, scope, metric, and table. The local routing table, IPv6 link-local routes, and IPv6 kernel routes are not copied. If a user-configured route and a discovered route have the same destination and table, the user-configured route takes precedence inside the Pod.

DRANET carries rules for the discovered non-main and non-local route tables into the Pod. It preserves `priority`, `family`, `protocol`, `source`, `destination`, `iifName`, `oifName`, and `table`. Input and output interface names are translated when the interface is renamed in the Pod. Rules that use other selectors, such as marks, masks, TOS, tunnel IDs, `goto`, realms, suppress settings, inversion, port ranges, IP protocol selectors, or UID ranges, are skipped. When a VRF is configured, DRANET does not copy the host rules into the Pod because the VRF performs the table lookup.

When the claim is released, DRANET restores the checkpointed sysctls, brings the host interface up, and restores its routes, route metrics, and rules. If the Pod network namespace disappears before the detach hook runs, DRANET keeps the checkpoint and retries until the interface and any expected addresses are available on the host. After the first successful restore, it keeps the checkpoint for a 30-second stabilization period. It then reapplies and verifies the host state before deleting the checkpoint. This second pass recovers from a late interface down and up cycle caused by host networking or authentication services.

#### Neighbor Configuration (NeighborConfig)

The NeighborConfig structure defines permanent neighbor entries (ARP for IPv4, NDP for IPv6) to be added to the Pod's network namespace.

```go
type NeighborConfig struct {
	// Destination is the target IP address.
	Destination string `json:"destination,omitempty"`
	// HardwareAddr is the MAC address of the neighbor.
	HardwareAddr string `json:"hardwareAddr,omitempty"`
}
```

* **ipAddress** (string, required): The IP address of the neighbor (e.g., "192.168.1.1", "2001:db8::1").
* **hardwareAddr** (string, required): The MAC address of the neighbor (e.g., "00:11:22:33:44:55").

#### Ethtool Configuration (EthtoolConfig)

The EthtoolConfig structure allows for the configuration of hardware offload features and other settings managed by ethtool.

```go
// EthtoolConfig defines ethtool-based optimizations for a network interface.
// These settings correspond to features typically toggled using `ethtool -K <dev> <feature> on|off`.
type EthtoolConfig struct {
	// Features is a map of ethtool feature names to their desired state (true for on, false for off).
	// Example: {"tcp-segmentation-offload": true, "rx-checksum": true}
	Features map[string]bool `json:"features,omitempty"`

	// PrivateFlags is a map of device-specific private flag names to their desired state.
	// Example: {"my-custom-flag": true}
	PrivateFlags map[string]bool `json:"privateFlags,omitempty"`
}
```

* **features** (map[string]bool, optional): A map of ethtool feature names to their desired state (true for on, false for off). For example, {"tcp-segmentation-offload": true, "rx-checksum": true}.
* **privateFlags** (map[string]bool, optional): A map of device-specific private flag names to their desired state. For example, {"my-custom-flag": true}.

### Example: Customizing a Network Interface and Routes

Below is an example of a ResourceClaim that allocates a dummy interface, renames it to "dranet0", assigns a static IP address, configures two routes (one to a subnet via a gateway and another link-scoped route), and adds a permanent IPv4 neighbor entry. It also disables several ethtool features.

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: dummy-interface-advanced
spec:
  devices:
    requests:
    - name: req-dummy-advanced
      exactly:
        deviceClassName: dra.net
        selectors:
          - cel:
              expression: device.attributes["dra.net"].ifName == "dummy3"
    config:
    - opaque:
        driver: dra.net
        parameters:
          interface:
            name: "dranet0"
            addresses:
            - "169.254.169.14/24"
            mtu: 4321
            hardwareAddr: "00:11:22:33:44:55"
          routes:
          - destination: "169.254.169.0/24"
            gateway: "169.254.169.1"
          - destination: "169.254.169.1/32"
            scope: 253
          neighbors:
          - ipAddress: "192.168.1.1"
            hardwareAddr: "00:11:22:33:44:55"
          ethtool:
            features:
              tcp-segmentation-offload: false
              generic-receive-offload: false
              large-receive-offload: false
---
apiVersion: v1
kind: Pod
metadata:
  name: pod-advanced-cfg
  labels:
    app: pod
spec:
  containers:
  - name: ctr1
    image: registry.k8s.io/e2e-test-images/agnhost:2.54
    # Keep the container running
    command: ["sleep", "infinity"]
  resourceClaims:
  - name: dummy1
    resourceClaimName: dummy-interface-advanced
```
