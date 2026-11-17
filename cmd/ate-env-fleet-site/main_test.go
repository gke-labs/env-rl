// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"strings"
	"testing"
)

const audience = "/projects/1/locations/us-central1/services/ate-fleet-site"

// FS-7: the site refuses to start without IAP unless it serves loopback only.
func TestConfigValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config
		want string
	}{
		{"production", config{listen: ":8080", bucket: "b", iapAudience: audience, eventsTable: "p-12345.fleet.actor_events_v"}, ""},
		{"local", config{listen: "127.0.0.1:8080", dir: "/tmp/snap", insecureNoIAP: true}, ""},
		{"local ipv6", config{listen: "[::1]:8080", dir: "/tmp/snap", insecureNoIAP: true}, ""},
		{"localhost", config{listen: "localhost:8080", dir: "/tmp/snap", insecureNoIAP: true}, ""},
		{"no source", config{listen: ":8080", iapAudience: audience, eventsTable: "t"}, "one of -bucket and -dir"},
		{"two sources", config{listen: ":8080", bucket: "b", dir: "d", iapAudience: audience, eventsTable: "t"}, "one of -bucket and -dir"},
		{"no audience", config{listen: ":8080", bucket: "b", eventsTable: "t"}, "-iap-audience is required"},
		{"no events", config{listen: ":8080", bucket: "b", iapAudience: audience}, "-events-table is required"},
		{"both auth modes", config{listen: "127.0.0.1:8080", bucket: "b", iapAudience: audience, insecureNoIAP: true}, "cannot be used together"},
		{"insecure on all interfaces", config{listen: ":8080", dir: "d", insecureNoIAP: true}, "loopback"},
		{"insecure on 0.0.0.0", config{listen: "0.0.0.0:8080", dir: "d", insecureNoIAP: true}, "loopback"},
		{"insecure on a host name", config{listen: "example.com:8080", dir: "d", insecureNoIAP: true}, "loopback"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validate()
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
