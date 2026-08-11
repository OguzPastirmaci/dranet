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
	"strings"
)

// allowedOptionNamespaces mirrors the provider hints in lowercase.
// Parsing rejects unknown namespaces to catch typos early.
var allowedOptionNamespaces = map[string]bool{
	"gce":     true,
	"aws":     true,
	"azure":   true,
	"oke":     true,
	"alibaba": true,
	"webhook": true,
}

// parseCloudProviderOptions parses comma-separated <provider>.<option>=<value>
// pairs. Values may contain equals signs but not commas.
func parseCloudProviderOptions(value string) (map[string]string, error) {
	options := map[string]string{}
	if value == "" {
		return options, nil
	}
	for _, pair := range strings.Split(value, ",") {
		key, optionValue, found := strings.Cut(pair, "=")
		if !found {
			return nil, fmt.Errorf("cloud provider option %q is not a key=value pair", pair)
		}
		namespace, name, found := strings.Cut(key, ".")
		if !found || namespace == "" || name == "" {
			return nil, fmt.Errorf("cloud provider option key %q must use the <provider>.<option> format", key)
		}
		if !allowedOptionNamespaces[namespace] {
			return nil, fmt.Errorf("cloud provider option namespace %q is not supported (supported: gce, aws, azure, oke, alibaba, webhook)", namespace)
		}
		if _, exists := options[key]; exists {
			return nil, fmt.Errorf("duplicate cloud provider option %q", key)
		}
		options[key] = optionValue
	}
	return options, nil
}
