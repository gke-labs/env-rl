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

package portproxy

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// The hostname form names the target in the Host of the request:
//
//	<port>-<id>.<suffix>             the default atespace
//	<port>-<id>.<atespace>.<suffix>
//
// with Options.PortHostSuffix as the suffix. The port is the part of the
// first label before its first '-': a port holds no '-', an id can. An
// atespace and an id are DNS labels (see label), thus the atespace is a label
// of its own, and the count of labels in front of the suffix tells the two
// shapes apart. The proxy forwards the whole path and the query, as the
// header form does.
//
// The suffix and every name below it belong to this form. A request there
// never reaches another form or the API (see Dispatch), and a name that is
// not a valid target gets 400.

// maxDNSLabel is the longest label a DNS name can hold, and maxDNSName the
// longest name.
const (
	maxDNSLabel = 63
	maxDNSName  = 253
)

// NormalizeHostSuffix returns the canonical text of a suffix for the hostname
// form: lowercase, without a trailing dot. It fails for a value that is not a
// domain name of DNS labels: a scheme, a port, a wildcard, a leading dot or
// an IP address. It fails also for a name of one label, such as "com" or
// "localhost": the Host check lets each <port>-<id>.<suffix> name pass on
// the proxy's route (hostcheck.(*Checker).WrapFor), thus a top-level name
// would open that route to the names of a whole registry. There is no list
// of public suffixes: "co.uk" has two labels and passes.
func NormalizeHostSuffix(s string) (string, error) {
	suffix := strings.TrimSuffix(lowerASCII(strings.TrimSpace(s)), ".")
	if suffix == "" {
		return "", errors.New("the host suffix is empty")
	}
	if _, err := netip.ParseAddr(suffix); err == nil {
		return "", fmt.Errorf("host suffix %q is an IP address; give a domain name", s)
	}
	if len(suffix) > maxDNSName {
		return "", fmt.Errorf("host suffix %q is longer than %d bytes", s, maxDNSName)
	}
	for l := range strings.SplitSeq(suffix, ".") {
		if !label.MatchString(l) {
			return "", fmt.Errorf("host suffix %q is not a domain name (labels of letters, digits and '-', without scheme, port, wildcard or leading dot)", s)
		}
	}
	if !strings.Contains(suffix, ".") {
		return "", fmt.Errorf("host suffix %q has one label; give a domain of two labels or more, such as ports.example", s)
	}
	return suffix, nil
}

// CheckHostSuffix returns the canonical text of a suffix for the hostname
// form (see NormalizeHostSuffix). It fails when the suffix is one of
// apiHosts or above one: the suffix and every name below it serve
// environment ports only, thus the API would answer 400 on that name while
// a health probe that sends an IP address still passes. apiHosts are host
// names without a port; a leading "*." (a wildcard of the Host check) stands
// for the name behind it.
func CheckHostSuffix(s string, apiHosts []string) (string, error) {
	suffix, err := NormalizeHostSuffix(s)
	if err != nil {
		return "", err
	}
	for _, h := range apiHosts {
		name := strings.TrimSuffix(strings.TrimPrefix(lowerASCII(strings.TrimSpace(h)), "*."), ".")
		if name == suffix || strings.HasSuffix(name, "."+suffix) {
			return "", fmt.Errorf("host suffix %q covers %q, a name that callers use for ate-env-api: the suffix and each name below it serve environment ports only; give the hostname form a domain of its own", suffix, h)
		}
	}
	return suffix, nil
}

// APIHosts returns the names that callers use for ate-env-api, the apiHosts
// of CheckHostSuffix: localhost, the in-cluster names of its Service
// (hostcheck.ServiceHosts) and the names of --allowed-hosts. ate-env-api and
// ate-env manifest both build the list here, so that the two checks of a
// suffix cover the same names.
func APIHosts(serviceHosts, allowedHosts []string) []string {
	hosts := make([]string, 0, 1+len(serviceHosts)+len(allowedHosts))
	hosts = append(hosts, "localhost")
	hosts = append(hosts, serviceHosts...)
	return append(hosts, allowedHosts...)
}

// lowerASCII lowers the ASCII letters of s and keeps every other byte. A Host
// is compared this way: strings.ToLower follows Unicode, which maps U+212A
// KELVIN SIGN to "k", and a name with that rune must not read as an ASCII
// name.
func lowerASCII(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; 'A' <= c && c <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if c := b[j]; 'A' <= c && c <= 'Z' {
					b[j] = c + 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

// hostFormName returns the name in hostport, the Host of a request, when the
// hostname form is on and the name is the suffix or below it: without its
// port, without one trailing dot, in lowercase. A Host in brackets is an IP
// literal, never a name: net.SplitHostPort would take the brackets off
// "[name]:port" and leave a name.
func (p *Proxy) hostFormName(hostport string) (string, bool) {
	suffix := p.opts.PortHostSuffix
	if suffix == "" || strings.ContainsAny(hostport, "[]") {
		return "", false
	}
	name := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		name = h
	}
	name = strings.TrimSuffix(lowerASCII(name), ".")
	if name != suffix && !strings.HasSuffix(name, "."+suffix) {
		return "", false
	}
	return name, true
}

// parseHost returns the target that name names. name is the suffix or a name
// below it, as hostFormName returns it.
func parseHost(name, suffix, defaultAtespace string) (target, error) {
	// Built only on the paths that return it: a valid name takes none.
	shape := func() error {
		return fmt.Errorf("host %q does not name an environment port: the names under %s are <port>-<id>.%s and <port>-<id>.<atespace>.%s", name, suffix, suffix, suffix)
	}
	if name == suffix {
		return target{}, shape()
	}
	first, atespace, named := strings.Cut(strings.TrimSuffix(name, "."+suffix), ".")
	// Checked here: makeTarget reads an empty atespace as the default one,
	// and "8000-env1..<suffix>" must not.
	if named && !label.MatchString(atespace) {
		return target{}, shape()
	}
	portStr, id, ok := strings.Cut(first, "-")
	if !ok {
		return target{}, shape()
	}
	if len(first) > maxDNSLabel {
		return target{}, fmt.Errorf("host %q: the label %q is longer than the %d bytes of a DNS label; reach an environment with an id this long through the header form or the path form", name, first, maxDNSLabel)
	}
	t, err := makeTarget(id, atespace, portStr, defaultAtespace)
	if err != nil {
		return t, fmt.Errorf("host %q: %w", name, err)
	}
	return t, nil
}

// ServesHost reports whether hostport, the Host of a request, names an
// environment port in the hostname form. It is the question that the Host
// check asks for the proxy's route (hostcheck.(*Checker).WrapFor): such a
// name is not one of the listener's own. It is false for every Host when
// Options.PortHostSuffix is empty.
func (p *Proxy) ServesHost(hostport string) bool {
	name, ok := p.hostFormName(hostport)
	if !ok {
		return false
	}
	_, err := parseHost(name, p.opts.PortHostSuffix, p.opts.DefaultAtespace)
	return err == nil
}

// ServeHost forwards a hostname-form request to the environment port its
// Host names, with the path and the query as sent. The Host alone names the
// target: a request that also carries a routing header of the header form is
// refused, so that no caller and no front proxy can think it reached another
// target. The checks are those of ServePath, in the same order: the caller,
// the atespace, then the origin of a browser request (see sameOrigin).
func (p *Proxy) ServeHost(w http.ResponseWriter, r *http.Request) {
	name, ok := p.hostFormName(r.Host)
	if !ok {
		http.Error(w, fmt.Sprintf("host %q is not under the port proxy's host suffix", r.Host), http.StatusBadRequest)
		return
	}
	t, err := parseHost(name, p.opts.PortHostSuffix, p.opts.DefaultAtespace)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for _, h := range []string{EnvIDHeader, EnvAtespaceHeader, EnvPortHeader} {
		if len(r.Header.Values(h)) > 0 {
			http.Error(w, fmt.Sprintf("host %q names the environment port; remove %s, or send the request to a host of ate-env-api to use the header form", name, h), http.StatusBadRequest)
			return
		}
	}
	if r.URL.Opaque == "" {
		t.path, t.rawPath = r.URL.Path, requestPath(r.URL)
	}
	if !p.authenticate(w, r, &t) || !p.inScope(w, t) || !p.sameOrigin(w, r) {
		return
	}
	p.forward(w, r, t)
}
