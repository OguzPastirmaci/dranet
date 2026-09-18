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

package discovery

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"sigs.k8s.io/dranet/pkg/cloudprovider/oke"
)

// ProviderOptions holds the checked --cloud-provider-options of each provider.
type ProviderOptions struct {
	OKE []oke.Option
}

// ParseProviderOptions parses comma-separated <provider>.<option>=<value>
// pairs and checks them with the rules of their provider. The checks need no
// instance data, so a bad option stops DRANET before discovery.
func ParseProviderOptions(value string) (ProviderOptions, error) {
	options, err := splitProviderOptions(value)
	if err != nil {
		return ProviderOptions{}, err
	}
	okeOptions := map[string]string{}
	for _, key := range slices.Sorted(maps.Keys(options)) {
		// A provider that defines options adds a case here.
		switch provider, _, _ := strings.Cut(key, "."); provider {
		case "oke":
			okeOptions[key] = options[key]
		default:
			return ProviderOptions{}, fmt.Errorf("provider %s defines no options, got %q", provider, key)
		}
	}
	okeOpts, err := oke.ParseOptions(okeOptions)
	if err != nil {
		return ProviderOptions{}, err
	}
	return ProviderOptions{OKE: okeOpts}, nil
}

// splitProviderOptions splits the flag value into pairs. As with
// --feature-gates, empty pairs are skipped and spaces around keys and values
// are trimmed. Values may contain equals signs but not commas.
func splitProviderOptions(value string) (map[string]string, error) {
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
		provider, name, found := strings.Cut(key, ".")
		if !found || provider == "" || name == "" {
			return nil, fmt.Errorf("cloud provider option key %q must use the <provider>.<option> format", key)
		}
		if _, exists := options[key]; exists {
			return nil, fmt.Errorf("duplicate cloud provider option %q", key)
		}
		options[key] = optionValue
	}
	return options, nil
}
