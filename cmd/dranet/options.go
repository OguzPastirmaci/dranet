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

package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"sigs.k8s.io/dranet/pkg/cloudprovider/discovery"
)

// optionNamespaces maps each option namespace to the only
// --cloud-provider-hint that accepts it. Parsing rejects other namespaces
// to catch typos early.
var optionNamespaces = map[string]discovery.CloudProviderHint{
	"gce":     discovery.CloudProviderHintGCE,
	"aws":     discovery.CloudProviderHintAWS,
	"azure":   discovery.CloudProviderHintAzure,
	"oke":     discovery.CloudProviderHintOKE,
	"alibaba": discovery.CloudProviderHintAlibaba,
	"cks":     discovery.CloudProviderHintCKS,
	"webhook": discovery.CloudProviderHintWebhook,
}

// parseCloudProviderOptions parses comma-separated <provider>.<option>=<value>
// pairs. As with --feature-gates, empty pairs are skipped and spaces around
// keys and values are trimmed. Values may contain equals signs but not commas.
func parseCloudProviderOptions(value string) (map[string]string, error) {
	options := map[string]string{}
	for _, pair := range strings.Split(value, ",") {
		if pair == "" {
			continue
		}
		key, optionValue, found := strings.Cut(pair, "=")
		if !found {
			return nil, fmt.Errorf("cloud provider option %q is not a key=value pair", pair)
		}
		key, optionValue = strings.TrimSpace(key), strings.TrimSpace(optionValue)
		namespace, name, found := strings.Cut(key, ".")
		if !found || namespace == "" || name == "" {
			return nil, fmt.Errorf("cloud provider option key %q must use the <provider>.<option> format", key)
		}
		if _, ok := optionNamespaces[namespace]; !ok {
			return nil, fmt.Errorf("cloud provider option namespace %q is not supported (supported: %s)", namespace, strings.Join(slices.Sorted(maps.Keys(optionNamespaces)), ", "))
		}
		if _, exists := options[key]; exists {
			return nil, fmt.Errorf("duplicate cloud provider option %q", key)
		}
		options[key] = optionValue
	}
	return options, nil
}
