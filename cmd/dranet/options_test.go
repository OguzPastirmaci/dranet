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
	"reflect"
	"testing"
)

func TestParseCloudProviderOptions(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    map[string]string
		wantErr bool
	}{
		{name: "empty flag", value: "", want: map[string]string{}},
		{name: "single pair", value: "oke.child-ipv4-cidr=10.192.0.0/15", want: map[string]string{"oke.child-ipv4-cidr": "10.192.0.0/15"}},
		{name: "multiple pairs", value: "oke.a=1,oke.b=2", want: map[string]string{"oke.a": "1", "oke.b": "2"}},
		{name: "value with equals sign", value: "oke.a=x=y", want: map[string]string{"oke.a": "x=y"}},
		{name: "gce namespace parses", value: "gce.something=x", want: map[string]string{"gce.something": "x"}},
		{name: "missing equals sign", value: "oke.child-ipv4-cidr", wantErr: true},
		{name: "missing period", value: "childcidr=10.192.0.0/15", wantErr: true},
		{name: "empty namespace", value: ".child-ipv4-cidr=x", wantErr: true},
		{name: "empty option name", value: "oke.=x", wantErr: true},
		{name: "unknown namespace", value: "foo.something=x", wantErr: true},
		{name: "namespace typo", value: "okee.child-ipv4-cidr=x", wantErr: true},
		{name: "duplicate key", value: "oke.a=1,oke.a=2", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCloudProviderOptions(tt.value)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseCloudProviderOptions(%q) returned no error, got %#v", tt.value, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCloudProviderOptions(%q) returned error: %v", tt.value, err)
			}
			if !reflect.DeepEqual(tt.want, got) {
				t.Errorf("parseCloudProviderOptions(%q) = %#v, want %#v", tt.value, got, tt.want)
			}
		})
	}
}
