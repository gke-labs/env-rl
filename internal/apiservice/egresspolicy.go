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

package apiservice

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/agent-substrate/env/internal/ate"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/validate/content"
)

// The order of a create with egress_policy and time_to_live follows the
// plan: the actor, the policy, then the environment store's object. When a
// policy write and its rollback both fail, the environment stays with no
// deadline, and the error says to delete it (Internal).

// The limits of Substrate v1.0.0's EgressPolicy (api/ateapi/v1/ateapi.proto
// and cmd/ateapi/internal/apivalidation). Substrate's validator is internal
// to its module, thus the server checks a policy here, before it creates an
// actor (EG-3).
const (
	maxEgressRules         = 256
	maxEgressRuleHostnames = 256
	maxEgressPortNumbers   = 16
)

// egressPortKey is one thing a Ports matches: a port number, or every port.
// Substrate reports a tie only between equal keys: "all" ties with "all".
type egressPortKey struct {
	number int32
	all    bool
}

func (k egressPortKey) String() string {
	if k.all {
		return "every port"
	}
	return fmt.Sprintf("port %d", k.number)
}

// checkEgressPolicy returns InvalidArgument for a policy that Substrate
// v1.0.0 would refuse, naming the first problem. A nil policy is valid: the
// request sets none.
func checkEgressPolicy(p *ateenvv1alpha.EgressPolicy) error {
	if p == nil {
		return nil
	}
	if err := egressPolicyProblem(p); err != nil {
		return status.Errorf(codes.InvalidArgument, "egress_policy: %v", err)
	}
	return nil
}

func egressPolicyProblem(p *ateenvv1alpha.EgressPolicy) error {
	rules := p.GetRules()
	if len(rules) > maxEgressRules {
		return fmt.Errorf("%d rules, more than %d", len(rules), maxEgressRules)
	}
	type tieKey struct {
		hostname string
		port     egressPortKey
	}
	seen := map[tieKey]int{}
	for i, r := range rules {
		member, hostnames, ports, err := egressRuleFields(r)
		if err != nil {
			return fmt.Errorf("rules[%d]: %v", i, err)
		}
		if err := checkEgressHostnames(hostnames); err != nil {
			return fmt.Errorf("rules[%d].%s.hostnames: %v", i, member, err)
		}
		keys, err := egressPortKeys(member, ports)
		if err != nil {
			return fmt.Errorf("rules[%d].%s.ports: %v", i, member, err)
		}
		for _, h := range hostnames {
			for _, k := range keys {
				if prior, ok := seen[tieKey{h, k}]; ok {
					return fmt.Errorf("rules[%d] and rules[%d] tie on %q and %s", prior, i, h, k)
				}
			}
		}
		for _, h := range hostnames {
			for _, k := range keys {
				seen[tieKey{h, k}] = i
			}
		}
	}
	return nil
}

// egressRuleFields returns the protocol a rule sets, its hostnames and its
// ports, or an error for a rule that sets no protocol.
func egressRuleFields(r *ateenvv1alpha.EgressRule) (member string, hostnames []string, ports *ateenvv1alpha.Ports, err error) {
	switch {
	case r.GetHttp() != nil:
		return "http", r.GetHttp().GetHostnames(), r.GetHttp().GetPorts(), nil
	case r.GetHttps() != nil:
		return "https", r.GetHttps().GetHostnames(), r.GetHttps().GetPorts(), nil
	case r.GetTlsPassthrough() != nil:
		return "tls_passthrough", r.GetTlsPassthrough().GetHostnames(), r.GetTlsPassthrough().GetPorts(), nil
	}
	return "", nil, nil, errors.New("sets none of http, https and tls_passthrough")
}

func checkEgressHostnames(hostnames []string) error {
	if len(hostnames) == 0 {
		return errors.New("at least one hostname is required")
	}
	if len(hostnames) > maxEgressRuleHostnames {
		return fmt.Errorf("%d hostnames, more than %d", len(hostnames), maxEgressRuleHostnames)
	}
	seen := make(map[string]bool, len(hostnames))
	for _, h := range hostnames {
		if seen[h] {
			return fmt.Errorf("%q is repeated", h)
		}
		seen[h] = true
		if !validEgressHostname(h) {
			return fmt.Errorf(`%q must be a lowercase DNS hostname, "*.<hostname>" or "*"`, h)
		}
	}
	return nil
}

// validEgressHostname mirrors Substrate's ParseHostnamePattern
// (internal/egresspolicy): "*" alone, or a DNS-1123 subdomain, optionally
// after "*." for one whole leftmost label, whose last label is not all
// digits (which rules out an IPv4 address).
func validEgressHostname(h string) bool {
	if h == "*" {
		return true
	}
	name := strings.TrimPrefix(h, "*.")
	if len(content.IsDNS1123Subdomain(name)) != 0 {
		return false
	}
	last := name[strings.LastIndexByte(name, '.')+1:]
	return strings.Trim(last, "0123456789") != ""
}

// egressPortKeys checks a rule's ports and returns what they match, with
// Substrate's defaults applied: 80 for http and 443 for https when unset.
// tls_passthrough has no default.
func egressPortKeys(member string, ports *ateenvv1alpha.Ports) ([]egressPortKey, error) {
	if ports == nil {
		switch member {
		case "http":
			return []egressPortKey{{number: 80}}, nil
		case "https":
			return []egressPortKey{{number: 443}}, nil
		}
		return nil, errors.New("required for tls_passthrough")
	}
	numbers := ports.GetNumbers()
	switch {
	case ports.GetAll() != nil && len(numbers) > 0:
		return nil, errors.New("sets both all and numbers")
	case ports.GetAll() != nil:
		return []egressPortKey{{all: true}}, nil
	case len(numbers) == 0:
		return nil, errors.New("sets neither all nor numbers")
	case len(numbers) > maxEgressPortNumbers:
		return nil, fmt.Errorf("%d numbers, more than %d", len(numbers), maxEgressPortNumbers)
	}
	keys := make([]egressPortKey, 0, len(numbers))
	seen := make(map[int32]bool, len(numbers))
	for _, n := range numbers {
		if n < 1 || n > 65535 {
			return nil, fmt.Errorf("%d is outside 1..65535", n)
		}
		if seen[n] {
			return nil, fmt.Errorf("%d is repeated", n)
		}
		seen[n] = true
		keys = append(keys, egressPortKey{number: n})
	}
	return keys, nil
}

// substrateEgressRules converts a checked policy to Substrate's rules. Ports
// left unset stay unset, thus Substrate applies its defaults.
func substrateEgressRules(p *ateenvv1alpha.EgressPolicy) []*ateapipb.EgressRule {
	out := make([]*ateapipb.EgressRule, 0, len(p.GetRules()))
	for _, r := range p.GetRules() {
		switch {
		case r.GetHttp() != nil:
			out = append(out, &ateapipb.EgressRule{Http: &ateapipb.HttpRule{
				Hostnames: append([]string(nil), r.GetHttp().GetHostnames()...),
				Ports:     substratePorts(r.GetHttp().GetPorts()),
			}})
		case r.GetHttps() != nil:
			out = append(out, &ateapipb.EgressRule{Https: &ateapipb.HttpsRule{
				Hostnames: append([]string(nil), r.GetHttps().GetHostnames()...),
				Ports:     substratePorts(r.GetHttps().GetPorts()),
			}})
		case r.GetTlsPassthrough() != nil:
			out = append(out, &ateapipb.EgressRule{TlsPassthrough: &ateapipb.TlsPassthroughRule{
				Hostnames: append([]string(nil), r.GetTlsPassthrough().GetHostnames()...),
				Ports:     substratePorts(r.GetTlsPassthrough().GetPorts()),
			}})
		}
	}
	return out
}

func substratePorts(p *ateenvv1alpha.Ports) *ateapipb.Ports {
	switch {
	case p == nil:
		return nil
	case p.GetAll() != nil:
		return &ateapipb.Ports{All: &ateapipb.AllPorts{}}
	}
	return &ateapipb.Ports{Numbers: append([]int32(nil), p.GetNumbers()...)}
}

// writeCreateEgressPolicy is the write of a create with egress_policy that
// follows CreateActor (EG-1): the actor exists, still suspended, and gets its
// policy. When the write fails, it deletes the actor with its uid as the
// precondition, thus only the actor of this create (EG-4), and returns the
// write's error code. When the delete fails too, it returns Internal with an
// error that names both, because the environment then exists with no policy
// and, for a create with time_to_live, with no deadline (EG-5).
func (s *Server) writeCreateEgressPolicy(ctx context.Context, atespace string, actor *ateapipb.Actor, p *ateenvv1alpha.EgressPolicy) error {
	id, uid := actor.GetMetadata().GetName(), actor.GetMetadata().GetUid()
	_, err := s.client.CreateEgressPolicy(ctx, atespace, id, substrateEgressRules(p))
	if err == nil {
		return nil
	}
	code := status.Code(toGRPCError(err))
	if errors.Is(err, ate.ErrAlreadyExists) {
		// ate.Client wraps AlreadyExists in its own error, which toGRPCError
		// does not map. Keep the write's code (EG-4).
		code = codes.AlreadyExists
	}
	if uid == "" {
		// Without the uid, a delete would have no precondition, and could
		// remove a later environment with the same id.
		log.Printf("apiservice: CreateEnvironment %s/%s: the egress policy was not written (%v); the environment stays with no policy", atespace, id, err)
		return status.Errorf(codes.Internal, "the egress policy of environment %q was not written (%v); the control plane returned the actor with no uid, thus the environment exists with no policy: delete it", id, err)
	}
	// The context of the caller can be the reason the write failed.
	rollback, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeWriteTimeout)
	defer cancel()
	delErr := s.client.DeleteWithUID(rollback, atespace, id, uid)
	// An actor that is gone, or that is another one by now (Aborted), is not
	// the environment of this create: nothing is left to roll back.
	if delErr != nil && !errors.Is(delErr, ate.ErrNotFound) && status.Code(delErr) != codes.Aborted {
		log.Printf("apiservice: CreateEnvironment %s/%s: the egress policy was not written (%v) and the delete of the actor failed (%v); the environment stays with no policy", atespace, id, err, delErr)
		return status.Errorf(codes.Internal, "the egress policy of environment %q was not written (%v), and its delete failed (%v); the environment exists with no policy: delete it", id, err, delErr)
	}
	return status.Errorf(code, "the egress policy of environment %q was not written, and the environment was deleted: %v", id, err)
}
