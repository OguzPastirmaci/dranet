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
	"reflect"
	"strings"
	"testing"
)

func TestSplitProviderOptions(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    map[string]string
		wantErr bool
	}{
		{name: "empty flag", value: "", want: map[string]string{}},
		{name: "single pair", value: "oke.rdma-child-ipv4-cidr=10.192.0.0/15", want: map[string]string{"oke.rdma-child-ipv4-cidr": "10.192.0.0/15"}},
		{name: "multiple pairs", value: "oke.a=1,cks.b=2", want: map[string]string{"oke.a": "1", "cks.b": "2"}},
		{name: "value with equals sign", value: "oke.a=x=y", want: map[string]string{"oke.a": "x=y"}},
		{name: "spaces around keys and values are trimmed", value: " oke.a = 1 , oke.b=2", want: map[string]string{"oke.a": "1", "oke.b": "2"}},
		{name: "empty pairs are skipped", value: ",oke.a=1,,oke.b=2,", want: map[string]string{"oke.a": "1", "oke.b": "2"}},
		{name: "only commas", value: ",,", want: map[string]string{}},
		{name: "missing equals sign", value: "oke.rdma-child-ipv4-cidr", wantErr: true},
		{name: "missing period", value: "childcidr=10.192.0.0/15", wantErr: true},
		{name: "empty provider", value: ".rdma-child-ipv4-cidr=x", wantErr: true},
		{name: "empty option name", value: "oke.=x", wantErr: true},
		{name: "duplicate key", value: "oke.a=1,oke.a=2", wantErr: true},
		{name: "duplicate key after trimming", value: "oke.a=1, oke.a=2", wantErr: true},
		{name: "space-only pair", value: "oke.a=1, ", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := splitProviderOptions(tt.value)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("splitProviderOptions(%q) returned no error, got %#v", tt.value, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("splitProviderOptions(%q) returned error: %v", tt.value, err)
			}
			if !reflect.DeepEqual(tt.want, got) {
				t.Errorf("splitProviderOptions(%q) = %#v, want %#v", tt.value, got, tt.want)
			}
		})
	}
}

func TestParseProviderOptions(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr string
	}{
		{name: "no options"},
		{name: "only commas", value: ",,"},
		{name: "a syntax error stops parsing", value: "oke.a", wantErr: "not a key=value pair"},
		{name: "OKE defines no options yet", value: "oke.a=1", wantErr: `provider oke defines no options, got "oke.a"`},
		{name: "CKS defines no options yet", value: "cks.a=1", wantErr: `provider cks defines no options, got "cks.a"`},
		{name: "a typo in the provider fails", value: "okee.a=1", wantErr: `provider okee defines no options, got "okee.a"`},
		{name: "the error names the first key", value: "oke.b=1,cks.a=1", wantErr: `got "cks.a"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseProviderOptions(tt.value)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ParseProviderOptions(%q) error = %v, want nil", tt.value, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ParseProviderOptions(%q) error = %v, want it to contain %q", tt.value, err, tt.wantErr)
			}
		})
	}
}
