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

package env_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-substrate/env/internal/internaltest/fakecontrol"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/agent-substrate/env/clients/go"
	"github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func findRepoRoot(t *testing.T) string {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(wd, "proto/testdata/client-parity")); err == nil {
			return wd
		}
		parent := filepath.Dir(wd)
		if parent == wd {
			t.Fatal("repo root not found")
		}
		wd = parent
	}
}

func readJSON(t *testing.T, filename string, v any) {
	t.Helper()
	path := filepath.Join(findRepoRoot(t), "proto/testdata/client-parity", filename)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", filename, err)
	}
}

func TestImageRefAttrParity(t *testing.T) {
	var vectors []struct {
		Ref  *string
		Want *string
	}
	readJSON(t, "image_ref.json", &vectors)
	for _, v := range vectors {
		ref := ""
		if v.Ref != nil {
			ref = *v.Ref
		}
		want := ""
		if v.Want != nil {
			want = *v.Want
		}
		if got := env.ImageRefAttrForTest(ref); got != want {
			t.Errorf("imageRefAttr(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestExecutableParity(t *testing.T) {
	var vectors []struct {
		Line string
		Argv []string // set instead of Line for a command given as words
		Want *string
	}
	readJSON(t, "executable.json", &vectors)
	for _, v := range vectors {
		want := ""
		if v.Want != nil {
			want = *v.Want
		}
		if v.Argv != nil {
			if got := env.ExecutableWordsForTest(v.Argv); got != want {
				t.Errorf("executableWords(%q) = %q, want %q", v.Argv, got, want)
			}
			continue
		}
		if got := env.ExecutableForTest(v.Line); got != want {
			t.Errorf("executable(%q) = %q, want %q", v.Line, got, want)
		}
	}
}

// OTEL_SEMCONV_STABILITY_OPT_IN parses as in otelgrpc's parseSemconvMode:
// the first rpc/* item decides, case-sensitive. A null value is unset.
func TestSemconvModeParity(t *testing.T) {
	var vectors []struct {
		Value *string
		Want  string
	}
	readJSON(t, "semconv_mode.json", &vectors)
	for _, v := range vectors {
		value := ""
		if v.Value != nil {
			value = *v.Value
		}
		if got := env.ParseRPCSemconvModeForTest(value); got != v.Want {
			t.Errorf("parseRPCSemconvMode(%q) = %q, want %q", value, got, v.Want)
		}
	}
}

func TestRPCAttrsParity(t *testing.T) {
	var vectors []struct {
		Mode    string
		Target  string
		Service string
		Method  string
		Want    map[string]any
	}
	readJSON(t, "rpc_attrs.json", &vectors)
	for _, v := range vectors {
		env.SetRPCSemconvModeForTest(t, env.ParseRPCSemconvModeForTest(v.Mode))
		gotList := env.RPCAttrsForTest(v.Service, v.Method, v.Target)
		got := make(map[string]any)
		for _, kv := range gotList {
			switch kv.Value.Type() {
			case attribute.INT64:
				got[string(kv.Key)] = float64(kv.Value.AsInt64())
			default:
				got[string(kv.Key)] = kv.Value.AsString()
			}
		}
		if len(got) != len(v.Want) {
			t.Errorf("rpcAttrs(%q, %q, %q, %q) = %v, want %v", v.Mode, v.Service, v.Method, v.Target, got, v.Want)
			continue
		}
		for k, wantVal := range v.Want {
			if got[k] != wantVal {
				t.Errorf("rpcAttrs(%q, %q, %q, %q)[%q] = %v, want %v", v.Mode, v.Service, v.Method, v.Target, k, got[k], wantVal)
			}
		}
	}
}

func TestErrorMappingParity(t *testing.T) {
	var vectors []struct {
		Code    codes.Code
		Message string
		Trailer string
		Want    struct {
			Kind      string
			Code      codes.Code
			Reason    string
			Retryable bool
		}
	}
	readJSON(t, "error_mapping.json", &vectors)

	allSentinels := []error{env.ErrNotFound, env.ErrInvalidArgument, env.ErrPermissionDenied, env.ErrFailedPrecondition, env.ErrProcessExited}

	for _, v := range vectors {
		st := status.New(v.Code, v.Message)
		var err error = st.Err()
		if v.Trailer != "" {
			err = env.MarkStartFailureForTest(context.Background(), err, v.Trailer)
		}
		got := env.FromGRPCErrorForTest(err)

		if env.Retryable(got) != v.Want.Retryable {
			t.Errorf("Retryable(%v) = %v, want %v", got, env.Retryable(got), v.Want.Retryable)
		}

		if v.Want.Kind == "start_error" {
			var se *env.StartError
			if !errors.As(got, &se) {
				t.Errorf("fromGRPCError(%v) = %T, want *StartError", err, got)
			} else {
				if se.Reason != v.Want.Reason {
					t.Errorf("StartError.Reason = %q, want %q", se.Reason, v.Want.Reason)
				}
				if se.Code != v.Want.Code {
					t.Errorf("StartError.Code = %v, want %v", se.Code, v.Want.Code)
				}
			}
		} else {
			var sentinel error
			switch v.Want.Kind {
			case "not_found":
				sentinel = env.ErrNotFound
			case "invalid_argument":
				sentinel = env.ErrInvalidArgument
			case "permission_denied":
				sentinel = env.ErrPermissionDenied
			case "failed_precondition":
				sentinel = env.ErrFailedPrecondition
			case "process_exited":
				sentinel = env.ErrProcessExited
			}

			if sentinel != nil {
				if !errors.Is(got, sentinel) {
					t.Errorf("fromGRPCError(%v) = %v, want it to wrap %v", err, got, sentinel)
				}
			} else {
				for _, s := range allSentinels {
					if errors.Is(got, s) {
						t.Errorf("fromGRPCError(%v) = %v, should not wrap %v", err, got, s)
					}
				}
				var se *env.StartError
				if errors.As(got, &se) {
					t.Errorf("fromGRPCError(%v) = %T, should not be *StartError", err, got)
				}
			}

			if status.Code(got) != v.Want.Code {
				t.Errorf("status.Code(%v) = %v, want %v", got, status.Code(got), v.Want.Code)
			}
		}
	}

	for _, v := range vectors {
		guest := failingGuest{code: v.Code, msg: v.Message, reason: v.Trailer}
		clients := failingClients(t, guest)
		for conn, client := range clients {
			e := client.Env("envs", "dev1")

			// Unary: GetProcess
			t.Run(conn+"/GetProcess/"+v.Message, func(t *testing.T) {
				_, err := e.Process("p1").Wait(context.Background())
				if err == nil {
					t.Fatalf("expected error")
				}
				if v.Want.Kind == "start_error" {
					var se *env.StartError
					if !errors.As(err, &se) {
						t.Errorf("err = %T, want *StartError", err)
					}
				} else {
					if status.Code(err) != v.Want.Code {
						t.Errorf("status.Code(%v) = %v, want %v", err, status.Code(err), v.Want.Code)
					}
				}
			})

			// Stream: WriteFile
			t.Run(conn+"/WriteFile/"+v.Message, func(t *testing.T) {
				err := e.WriteFile(context.Background(), "a.txt", strings.NewReader("hi"), 0644)
				if err == nil {
					t.Fatalf("expected error")
				}
				if v.Want.Kind == "start_error" {
					var se *env.StartError
					if !errors.As(err, &se) {
						t.Errorf("err = %T, want *StartError", err)
					}
				} else {
					if status.Code(err) != v.Want.Code {
						t.Errorf("status.Code(%v) = %v, want %v", err, status.Code(err), v.Want.Code)
					}
				}
			})
		}
	}
}

func TestMetricFamiliesParity(t *testing.T) {
	var vectors []struct {
		Name    string
		Help    string
		Labels  []string
		Buckets []float64
	}
	readJSON(t, "metric_families.json", &vectors)

	reg := prometheus.NewRegistry()
	capture := &capturingRegisterer{Registerer: reg}
	c, err := env.NewClient(env.ClientOptions{Endpoint: "dns:///localhost:1", MetricsRegisterer: capture})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	// Gather sorts label pairs by name, which hides their order. For each
	// vector, add a series whose label values are the vector's label names
	// in the vector's order: in the family whose labels are in that order,
	// each pair then has value == name.
	for _, v := range vectors {
		for _, h := range capture.vecs {
			_, _ = h.GetMetricWithLabelValues(v.Labels...) // a wrong label count is an error; skip it
		}
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]*dto.MetricFamily, len(families))
	for _, f := range families {
		byName[f.GetName()] = f
	}
	for _, v := range vectors {
		f, ok := byName[v.Name]
		if !ok {
			t.Errorf("metric family %q not registered", v.Name)
			continue
		}
		if f.GetHelp() != v.Help {
			t.Errorf("metric %q help = %q, want %q", v.Name, f.GetHelp(), v.Help)
		}
		if len(f.GetMetric()) == 0 || f.GetMetric()[0].GetHistogram() == nil {
			t.Errorf("metric %q has no histogram series", v.Name)
			continue
		}
		m := f.GetMetric()[0]
		if !hasSeriesNamingItsLabels(f, v.Labels) {
			var got []string
			for _, l := range m.GetLabel() {
				got = append(got, l.GetName())
			}
			t.Errorf("metric %q labels (sorted) = %v, want %v in that order", v.Name, got, v.Labels)
		}
		var buckets []float64
		for _, b := range m.GetHistogram().GetBucket() {
			buckets = append(buckets, b.GetUpperBound())
		}
		if !slices.Equal(buckets, v.Buckets) {
			t.Errorf("metric %q buckets = %v, want %v", v.Name, buckets, v.Buckets)
		}
	}
}

// capturingRegisterer records the HistogramVecs registered through it.
type capturingRegisterer struct {
	prometheus.Registerer
	vecs []*prometheus.HistogramVec
}

func (r *capturingRegisterer) Register(c prometheus.Collector) error {
	if h, ok := c.(*prometheus.HistogramVec); ok {
		r.vecs = append(r.vecs, h)
	}
	return r.Registerer.Register(c)
}

func (r *capturingRegisterer) MustRegister(cs ...prometheus.Collector) {
	for _, c := range cs {
		if err := r.Register(c); err != nil {
			panic(err)
		}
	}
}

// hasSeriesNamingItsLabels reports whether f has a series with exactly the
// labels in want, each set to its own name.
func hasSeriesNamingItsLabels(f *dto.MetricFamily, want []string) bool {
	for _, m := range f.GetMetric() {
		pairs := m.GetLabel()
		if len(pairs) != len(want) {
			continue
		}
		ok := true
		for _, l := range pairs {
			if l.GetName() != l.GetValue() || !slices.Contains(want, l.GetName()) {
				ok = false
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func TestRPCCoverageParity(t *testing.T) {
	var vectors map[string]map[string]string
	readJSON(t, "rpc_coverage.json", &vectors)

	fd := ateenvv1alpha.File_proto_ateenv_v1alpha_env_proto
	services := fd.Services()
	envSvc := services.ByName("EnvironmentService")
	if envSvc == nil {
		t.Fatal("EnvironmentService not found in proto")
	}

	methods := envSvc.Methods()
	for i := 0; i < methods.Len(); i++ {
		m := methods.Get(i)
		rpcName := string(m.Name())

		mapping, ok := vectors[rpcName]
		if !ok {
			t.Errorf("RPC %q not found in rpc_coverage.json vector", rpcName)
			continue
		}

		goMethod := mapping["go"]
		clientType := reflect.TypeOf((*env.Client)(nil))
		if _, ok := clientType.MethodByName(goMethod); !ok {
			t.Errorf("Go Client missing method %q for RPC %q", goMethod, rpcName)
		}
	}
}

func TestSpansParity(t *testing.T) {
	var vectors []struct {
		Name               string         `json:"name"`
		Operation          string         `json:"operation"`
		Inputs             map[string]any `json:"inputs"`
		ExpectedSpan       string         `json:"expected_span"`
		ExpectedAttributes map[string]any `json:"expected_attributes"`
		GoOnly             *struct {
			Attributes map[string]any `json:"attributes"`
			Reason     string         `json:"reason"`
		} `json:"go_only"`
	}
	readJSON(t, "spans.json", &vectors)

	seenOps := make(map[string]bool, len(vectors))
	for _, v := range vectors {
		seenOps[v.Operation] = true
	}
	for _, op := range env.SpanOperationsForTest {
		if !seenOps[op] {
			t.Errorf("spans.json has no vector for shared operation %q", op)
		}
	}

	f := newTracingFixture(t)
	storeOn(t, f.service) // set_environment_time_to_live needs the store
	f.serveGuest(t, "traced")
	labels := map[string]string{"workload": "default-env", "ate-env/domain": "d0"}
	f.control.AddWorker(fakecontrol.Worker("ate-system", "pool-d0", "node-1", labels, 8))
	for _, space := range []string{"t-space", "x-space"} {
		f.control.AddTemplate(&ateapipb.ActorTemplate{
			Metadata:       &ateapipb.ResourceMetadata{Name: "layered", Atespace: space},
			WorkerSelector: &ateapipb.Selector{MatchLabels: map[string]string{"workload": "default-env"}},
			Containers:     []*ateapipb.Container{{Name: "guest", Image: "ubuntu@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}},
		})
	}
	f.control.AddTemplate(&ateapipb.ActorTemplate{
		Metadata:   &ateapipb.ResourceMetadata{Name: "default-template", Atespace: "ate-env"},
		Containers: []*ateapipb.Container{{Name: "guest", Image: "ubuntu@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}},
	})
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	c, err := env.NewClient(env.ClientOptions{
		Endpoint:       f.addr,
		TracerProvider: tp,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	for _, v := range vectors {
		subName := v.Name
		if subName == "" {
			subName = v.Operation
		}
		t.Run(subName, func(t *testing.T) {
			ctx := context.Background()

			str := func(k string) string { s, _ := v.Inputs[k].(string); return s }
			id, atespace, template, templateAtespace := str("id"), str("atespace"), str("template"), str("template_atespace")
			rootfsImage, path, content := str("rootfs_image"), str("path"), str("content")
			var timeout time.Duration
			if ms, ok := v.Inputs["timeout_ms"].(float64); ok && ms > 0 {
				timeout = time.Duration(ms) * time.Millisecond
			}
			var command []string
			if val, ok := v.Inputs["command"].([]any); ok {
				for _, c := range val {
					command = append(command, c.(string))
				}
			}

			var tmpl *ateenvv1alpha.Template
			if template != "" || templateAtespace != "" {
				tmpl = &ateenvv1alpha.Template{Name: template, Atespace: templateAtespace}
			}

			if id != "" && v.Operation != "create_environment" {
				_, err := c.Create(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: id, Atespace: atespace, Template: tmpl})
				if err != nil && status.Code(err) != codes.AlreadyExists {
					t.Fatalf("setup create: %v", err)
				}
				if v.Operation == "read_file" {
					if err := c.Env(atespace, id).WriteFile(ctx, "notes.txt", strings.NewReader("hello world"), 0o644); err != nil {
						t.Fatalf("setup write_file: %v", err)
					}
				}
			}
			_ = tp.ForceFlush(ctx)
			recBefore := len(rec.Ended())

			e := c.Env(atespace, id)

			var err error
			switch v.Operation {
			case "create_environment":
				_, err = c.Create(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: id, Atespace: atespace, Template: tmpl, RootfsImage: rootfsImage})
			case "get_environment":
				_, err = c.Get(ctx, atespace, id)
			case "list_environments":
				_, err = c.List(ctx, &ateenvv1alpha.ListEnvironmentsRequest{Atespace: atespace})
			case "set_environment_time_to_live":
				_, err = c.SetTimeToLive(ctx, atespace, id, time.Minute)
			case "suspend_environment":
				err = c.Suspend(ctx, atespace, id)
			case "delete_environment":
				err = c.Delete(ctx, atespace, id)
			case "get_server_info":
				_, err = c.ServerInfo(ctx)
			case "get_image_status":
				_, err = c.ImageStatus(ctx, &ateenvv1alpha.GetImageStatusRequest{RootfsImage: rootfsImage, Template: tmpl, Atespace: atespace})
			case "ensure_image_template":
				_, err = c.EnsureImageTemplate(ctx, &ateenvv1alpha.EnsureImageTemplateRequest{RootfsImage: rootfsImage, Atespace: atespace, Template: tmpl})
			case "list_placement_domains":
				_, err = c.ListPlacementDomains(ctx, &ateenvv1alpha.ListPlacementDomainsRequest{Atespace: atespace, Template: tmpl})
			case "warm_image":
				_, err = c.WarmImage(ctx, &ateenvv1alpha.WarmImageRequest{RootfsImage: rootfsImage, Atespace: atespace, Template: tmpl}, nil)
			case "start_process":
				_, err = e.StartProcess(ctx, &ateenvv1alpha.StartProcessRequest{Command: command})
			case "exec":
				_, err = e.Run(ctx, env.ShellRequest{Command: strings.Join(command, " "), Timeout: timeout})
			case "read_file":
				var r io.ReadCloser
				if r, err = e.ReadFile(ctx, path); err == nil {
					_, err = io.ReadAll(r)
					if cerr := r.Close(); err == nil {
						err = cerr
					}
				}
			case "write_file":
				err = e.WriteFile(ctx, path, strings.NewReader(content), 0o644)
			default:
				t.Fatalf("unknown operation %q", v.Operation)
			}
			if err != nil {
				t.Fatalf("operation %q failed: %v", v.Operation, err)
			}
			_ = tp.ForceFlush(ctx)

			ended := rec.Ended()
			var span sdktrace.ReadOnlySpan
			expectedName := env.DefaultTraceServiceName + "." + v.ExpectedSpan
			for i := len(ended) - 1; i >= recBefore; i-- {
				if ended[i].Name() == expectedName {
					span = ended[i]
					break
				}
			}
			if span == nil {
				t.Fatalf("span %q not found", expectedName)
			}

			gotAttrs := attrs(span)

			want := make(map[string]any)
			for k, val := range v.ExpectedAttributes {
				want[k] = val
			}
			if v.GoOnly != nil {
				for k, val := range v.GoOnly.Attributes {
					want[k] = val
				}
			}

			for k, w := range want {
				if !strings.HasPrefix(k, "ate.") && !strings.HasPrefix(k, "ate_env.") {
					continue
				}
				g, ok := gotAttrs[attribute.Key(k)]
				if !ok {
					t.Errorf("missing attribute %q", k)
					continue
				}
				var gVal any
				switch g.Type() {
				case attribute.INT64:
					gVal = float64(g.AsInt64())
				case attribute.BOOL:
					gVal = g.AsBool()
				default:
					gVal = g.AsString()
				}
				if gVal != w {
					t.Errorf("attribute %q = %v, want %v", k, gVal, w)
				}
			}

			for k := range gotAttrs {
				ks := string(k)
				if !strings.HasPrefix(ks, "ate.") && !strings.HasPrefix(ks, "ate_env.") {
					continue
				}
				if _, ok := want[ks]; !ok {
					t.Errorf("unexpected attribute %q=%v", ks, gotAttrs[k].Emit())
				}
			}
		})
	}
}
