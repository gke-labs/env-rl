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
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func runManifestCollector(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newManifestCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(append([]string{"collector"}, args...))
	err := cmd.Execute()
	return out.String(), err
}

// The pipeline as the collector reads it, the parts the fleet site relies on.
type collectorPipelineDoc struct {
	Extensions struct {
		GoogleClientAuth struct {
			Project string `json:"project"`
		} `json:"google_client_auth"`
	} `json:"extensions"`
	Receivers struct {
		OTLP struct {
			Protocols map[string]struct {
				Endpoint string `json:"endpoint"`
			} `json:"protocols"`
		} `json:"otlp"`
	} `json:"receivers"`
	Processors struct {
		ProjectID struct {
			Attributes []map[string]string `json:"attributes"`
		} `json:"resource/gcp_project_id"`
	} `json:"processors"`
	Exporters struct {
		CloudTrace struct {
			Endpoint string `json:"endpoint"`
			Encoding string `json:"encoding"`
			Auth     struct {
				Authenticator string `json:"authenticator"`
			} `json:"auth"`
		} `json:"otlp_http/cloudtrace"`
		GCS struct {
			Bucket struct {
				Name          string `json:"name"`
				ReuseIfExists bool   `json:"reuse_if_exists"`
				FilePrefix    string `json:"file_prefix"`
				Compression   string `json:"compression"`
				Partition     struct {
					Prefix string `json:"prefix"`
					Format string `json:"format"`
				} `json:"partition"`
			} `json:"bucket"`
		} `json:"google_cloud_storage"`
	} `json:"exporters"`
	Service struct {
		Extensions []string `json:"extensions"`
		Pipelines  map[string]struct {
			Receivers  []string `json:"receivers"`
			Processors []string `json:"processors"`
			Exporters  []string `json:"exporters"`
		} `json:"pipelines"`
	} `json:"service"`
}

// FS-5: the collector exports each trace batch to Cloud Trace and as a span
// file under v1/traces/ in the fleet bucket, stamped with the cluster's
// project whatever the sender set.
func TestManifestCollector(t *testing.T) {
	out, err := runManifestCollector(t, "--bucket", "fleet-bucket", "--project", "cluster-project",
		"--gcp-service-account", "ate-env-collector@central.iam.gserviceaccount.com")
	if err != nil {
		t.Fatal(err)
	}
	docs := reporterDocs(t, out)
	if got := len(docs); got != 5 {
		t.Fatalf("%d documents, want Namespace, ServiceAccount, ConfigMap, Deployment, Service:\n%s", got, out)
	}

	var ns corev1.Namespace
	if err := yaml.Unmarshal([]byte(docs["Namespace"]), &ns); err != nil {
		t.Fatal(err)
	}
	// The collector does not call ateapi: no api-access label.
	// Apart from the acceptance fixture, so an apply never merges into it.
	if ns.Name == "ate-env-telemetry" || collectorName == "otel-collector" {
		t.Errorf("namespace %s, name %s: the fixture's tests/telemetry/otel-collector.yaml uses ate-env-telemetry/otel-collector", ns.Name, collectorName)
	}
	if ns.Name != collectorName || len(ns.Labels) != 0 {
		t.Errorf("namespace = %s %v, want %s without labels", ns.Name, ns.Labels, collectorName)
	}

	var sa corev1.ServiceAccount
	if err := yaml.Unmarshal([]byte(docs["ServiceAccount"]), &sa); err != nil {
		t.Fatal(err)
	}
	if sa.Name != collectorName || sa.Namespace != collectorName || sa.Annotations[workloadIdentityAnnotation] != "ate-env-collector@central.iam.gserviceaccount.com" {
		t.Errorf("ServiceAccount = %s/%s %v", sa.Namespace, sa.Name, sa.Annotations)
	}
	if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
		t.Error("ServiceAccount mounts a Kubernetes API token; the collector never calls the API")
	}

	var cm corev1.ConfigMap
	if err := yaml.Unmarshal([]byte(docs["ConfigMap"]), &cm); err != nil {
		t.Fatal(err)
	}
	var p collectorPipelineDoc
	if err := yaml.Unmarshal([]byte(cm.Data["config.yaml"]), &p); err != nil {
		t.Fatalf("config.yaml: %v\n%s", err, cm.Data["config.yaml"])
	}
	if len(p.Service.Pipelines) != 2 {
		t.Fatalf("pipelines = %+v, want traces/cloudtrace and traces/files", p.Service.Pipelines)
	}
	for name, exporter := range map[string]string{"traces/cloudtrace": "otlp_http/cloudtrace", "traces/files": "google_cloud_storage"} {
		pl, ok := p.Service.Pipelines[name]
		if !ok {
			t.Errorf("no pipeline %s", name)
			continue
		}
		if want := []string{exporter}; !reflect.DeepEqual(pl.Exporters, want) {
			t.Errorf("%s exporters = %v, want %v", name, pl.Exporters, want)
		}
		if want := []string{"otlp"}; !reflect.DeepEqual(pl.Receivers, want) {
			t.Errorf("%s receivers = %v, want %v", name, pl.Receivers, want)
		}
		// Every span gets the cluster's project before it leaves.
		if len(pl.Processors) != 3 || pl.Processors[0] != "memory_limiter" || pl.Processors[1] != "resource/gcp_project_id" {
			t.Errorf("%s processors = %v, want memory_limiter, resource/gcp_project_id, a batch", name, pl.Processors)
		}
	}
	ct := p.Exporters.CloudTrace
	if ct.Endpoint != "https://telemetry.googleapis.com" || ct.Encoding != "proto" || ct.Auth.Authenticator != "google_client_auth" {
		t.Errorf("Cloud Trace exporter = %+v", ct)
	}
	if p.Extensions.GoogleClientAuth.Project != "cluster-project" {
		t.Errorf("google_client_auth project = %q", p.Extensions.GoogleClientAuth.Project)
	}
	if want := []map[string]string{{"key": "gcp.project_id", "value": "cluster-project", "action": "upsert"}}; !reflect.DeepEqual(p.Processors.ProjectID.Attributes, want) {
		t.Errorf("gcp.project_id processor = %v, want %v", p.Processors.ProjectID.Attributes, want)
	}
	b := p.Exporters.GCS.Bucket
	if b.Name != "fleet-bucket" || !b.ReuseIfExists || b.Compression != "gzip" || b.Partition.Prefix != "v1/traces" || b.Partition.Format != "year=%Y/month=%m/day=%d" {
		t.Errorf("Cloud Storage exporter bucket = %+v", b)
	}
	for _, ext := range []string{"health_check", "google_client_auth"} {
		if !strings.Contains(strings.Join(p.Service.Extensions, " "), ext) {
			t.Errorf("service extensions %v lack %s", p.Service.Extensions, ext)
		}
	}

	var dep appsv1.Deployment
	if err := yaml.Unmarshal([]byte(docs["Deployment"]), &dep); err != nil {
		t.Fatal(err)
	}
	pod := dep.Spec.Template.Spec
	c := pod.Containers[0]
	if pod.ServiceAccountName != sa.Name || c.Image != defaultCollectorImage {
		t.Errorf("pod runs %s as %s", c.Image, pod.ServiceAccountName)
	}
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot {
		t.Error("pod may run as root")
	}
	if sp := pod.SecurityContext.SeccompProfile; sp == nil || sp.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("seccomp profile = %+v, want RuntimeDefault", sp)
	}
	// memory_limiter's percentages are of this limit.
	if lim := c.Resources.Limits[corev1.ResourceMemory]; lim.String() != "1Gi" {
		t.Errorf("memory limit = %s, want 1Gi", lim.String())
	}
	if got, want := dep.Spec.Template.Annotations[configHashAnnotation], fmt.Sprintf("%x", sha256.Sum256([]byte(cm.Data["config.yaml"]))); got != want {
		t.Errorf("pod annotation %s = %q, want the configuration's SHA-256 %q", configHashAnnotation, got, want)
	}
	if sc := c.SecurityContext; sc == nil || !*sc.ReadOnlyRootFilesystem || *sc.AllowPrivilegeEscalation || len(sc.Capabilities.Drop) != 1 {
		t.Errorf("container security context = %+v", sc)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet.Port.IntValue() != collectorHealthPort {
		t.Errorf("readiness probe = %+v, want the health_check port", c.ReadinessProbe)
	}
	if len(pod.Volumes) != 1 || pod.Volumes[0].ConfigMap == nil || pod.Volumes[0].ConfigMap.Name != cm.Name {
		t.Errorf("volumes = %+v, want the ConfigMap", pod.Volumes)
	}

	// The Service routes to the receiver's two endpoints.
	var svc corev1.Service
	if err := yaml.Unmarshal([]byte(docs["Service"]), &svc); err != nil {
		t.Fatal(err)
	}
	ports := map[string]int32{}
	for _, sp := range svc.Spec.Ports {
		ports[sp.Name] = sp.Port
	}
	// The documented endpoint is <name>.<namespace>: the Service is in the
	// collector's namespace and selects its pods.
	if svc.Name != collectorName || svc.Namespace != collectorName {
		t.Errorf("Service %s/%s, want %s/%s", svc.Namespace, svc.Name, collectorName, collectorName)
	}
	if !reflect.DeepEqual(svc.Spec.Selector, dep.Spec.Template.Labels) {
		t.Errorf("Service selector %v, want the pod labels %v", svc.Spec.Selector, dep.Spec.Template.Labels)
	}
	if ports["otlp-grpc"] != 4317 || ports["otlp-http"] != 4318 {
		t.Errorf("Service ports = %v, want otlp-grpc 4317 and otlp-http 4318", ports)
	}
	if p.Receivers.OTLP.Protocols["grpc"].Endpoint != "0.0.0.0:4317" || p.Receivers.OTLP.Protocols["http"].Endpoint != "0.0.0.0:4318" {
		t.Errorf("OTLP receiver = %+v", p.Receivers.OTLP.Protocols)
	}
}

func TestManifestCollectorNamespaceAndImage(t *testing.T) {
	out, err := runManifestCollector(t, "--bucket", "fleet-bucket", "--project", "cluster-project",
		"--namespace", " telemetry ", "--image", "example.com/otelcol@sha256:abc")
	if err != nil {
		t.Fatal(err)
	}
	docs := reporterDocs(t, out)
	var dep appsv1.Deployment
	if err := yaml.Unmarshal([]byte(docs["Deployment"]), &dep); err != nil {
		t.Fatal(err)
	}
	if dep.Namespace != "telemetry" || dep.Spec.Template.Spec.Containers[0].Image != "example.com/otelcol@sha256:abc" {
		t.Errorf("Deployment %s runs %s", dep.Namespace, dep.Spec.Template.Spec.Containers[0].Image)
	}
	if strings.Contains(docs["ServiceAccount"], workloadIdentityAnnotation) {
		t.Errorf("ServiceAccount carries %s without --gcp-service-account", workloadIdentityAnnotation)
	}
}

// The default image is the 0.165.0 the configuration is written for, pinned
// by digest.
func TestDefaultCollectorImage(t *testing.T) {
	if !regexp.MustCompile(`^otel/opentelemetry-collector-contrib:0\.165\.0@sha256:[0-9a-f]{64}$`).MatchString(defaultCollectorImage) {
		t.Errorf("defaultCollectorImage = %q, want otel/opentelemetry-collector-contrib:0.165.0@sha256:<digest>", defaultCollectorImage)
	}
}

// A new configuration changes the pod template, so kubectl apply restarts
// the collector, which reads its configuration at start only.
func TestManifestCollectorRestartsOnANewConfiguration(t *testing.T) {
	template := func(project string) string {
		out, err := runManifestCollector(t, "--bucket", "fleet-bucket", "--project", project)
		if err != nil {
			t.Fatal(err)
		}
		var dep appsv1.Deployment
		if err := yaml.Unmarshal([]byte(reporterDocs(t, out)["Deployment"]), &dep); err != nil {
			t.Fatal(err)
		}
		b, err := yaml.Marshal(dep.Spec.Template)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if template("my-proj-123") == template("other-proj-9") {
		t.Error("two projects render the same pod template")
	}
}

// An image without a digest gets a note on stderr.
func TestManifestCollectorNotesAnUnpinnedImage(t *testing.T) {
	cmd := newManifestCommand()
	var stderr bytes.Buffer
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"collector", "--bucket", "fleet-bucket", "--project", "cluster-project",
		"--gcp-service-account", "ate-env-collector@central-proj.iam.gserviceaccount.com", "--image", "otel/opentelemetry-collector-contrib:latest"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "not pinned by digest") {
		t.Errorf("stderr = %q, want a note about the digest", stderr.String())
	}
}

// The default image is pinned by digest: no note.
func TestManifestCollectorDefaultImageHasNoNote(t *testing.T) {
	cmd := newManifestCommand()
	var stderr bytes.Buffer
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"collector", "--bucket", "fleet-bucket", "--project", "cluster-project",
		"--gcp-service-account", "ate-env-collector@central-proj.iam.gserviceaccount.com"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr.String(), "not pinned by digest") {
		t.Errorf("stderr = %q, want no digest note for the default image %s", stderr.String(), defaultCollectorImage)
	}
}

func TestManifestCollectorAcceptedNames(t *testing.T) {
	for _, args := range [][]string{
		{"--bucket", "a.b-c_d", "--project", "example.com:cluster-project"},
		{"--bucket", strings.Repeat("a", 63) + "." + strings.Repeat("b", 63), "--project", "cluster-project"},
		{"--bucket", "fleet-bucket", "--project", "cluster-project", "--gcp-service-account", "123456789-compute@developer.gserviceaccount.com"},
		{"--bucket", "fleet-bucket", "--project", "cluster-project", "--gcp-service-account", "ate-env-collector@central-proj.example.com.iam.gserviceaccount.com"},
	} {
		if _, err := runManifestCollector(t, args...); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
}

// Without --gcp-service-account the command says the pod runs as the node.
func TestManifestCollectorNotesNoServiceAccount(t *testing.T) {
	cmd := newManifestCommand()
	var stderr bytes.Buffer
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"collector", "--bucket", "fleet-bucket", "--project", "cluster-project"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "--gcp-service-account") {
		t.Errorf("stderr = %q, want a note about --gcp-service-account", stderr.String())
	}
}

// The rendered configuration loads in the real collector. Set
// OTELCOL_CONTRIB to an otelcol-contrib 0.165.0 binary to run it; for
// example, extract /otelcol-contrib from the image of defaultCollectorImage.
// CI does not set it, so this check is manual: run it when the image or the
// configuration changes (docs/observability.md, "The fleet collector").
func TestManifestCollectorConfigValidates(t *testing.T) {
	bin := os.Getenv("OTELCOL_CONTRIB")
	if bin == "" {
		t.Skip("OTELCOL_CONTRIB is not set")
	}
	out, err := runManifestCollector(t, "--bucket", "fleet-bucket", "--project", "cluster-project")
	if err != nil {
		t.Fatal(err)
	}
	var cm corev1.ConfigMap
	if err := yaml.Unmarshal([]byte(reporterDocs(t, out)["ConfigMap"]), &cm); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(file, []byte(cm.Data["config.yaml"]), 0o600); err != nil {
		t.Fatal(err)
	}
	if msg, err := exec.Command(bin, "validate", "--config="+file).CombinedOutput(); err != nil {
		t.Fatalf("otelcol-contrib validate: %v\n%s", err, msg)
	}
}

func TestManifestCollectorRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no bucket", []string{"--project", "cluster-project"}, "--bucket is required"},
		{"gs URL", []string{"--bucket", "gs://fleet-bucket", "--project", "cluster-project"}, "not a gs:// URL"},
		{"service account not an email", []string{"--bucket", "fleet-bucket", "--project", "cluster-project", "--gcp-service-account", "not an email ${env:X}"}, "not <name>@<project>.iam.gserviceaccount.com"},
		{"service account of another domain", []string{"--bucket", "fleet-bucket", "--project", "cluster-project", "--gcp-service-account", "sa@example.com"}, "not <name>@<project>.iam.gserviceaccount.com"},
		{"compute account without a number", []string{"--bucket", "fleet-bucket", "--project", "cluster-project", "--gcp-service-account", "proj-compute@developer.gserviceaccount.com"}, "not <name>@<project>.iam.gserviceaccount.com"},
		{"service account with an empty domain part", []string{"--bucket", "fleet-bucket", "--project", "cluster-project", "--gcp-service-account", "ate-env-collector@central-proj..iam.gserviceaccount.com"}, "not <name>@<project>.iam.gserviceaccount.com"},
		{"bucket path", []string{"--bucket", "fleet-bucket/v1", "--project", "cluster-project"}, "not a Cloud Storage bucket name"},
		{"bucket expansion", []string{"--bucket", "${env:HOME}", "--project", "cluster-project"}, "not a Cloud Storage bucket name"},
		{"no project", []string{"--bucket", "fleet-bucket"}, "--project is required"},
		{"bad project", []string{"--bucket", "fleet-bucket", "--project", "Cluster_Project"}, "not a Google Cloud project id"},
		{"empty namespace", []string{"--bucket", "fleet-bucket", "--project", "cluster-project", "--namespace", " "}, "--namespace is empty"},
		{"namespace not a DNS label", []string{"--bucket", "fleet-bucket", "--project", "cluster-project", "--namespace", "Team_A"}, "not a Kubernetes namespace name"},
		{"bucket too long", []string{"--bucket", strings.Repeat("a", 64), "--project", "cluster-project"}, "longer than 63"},
		{"bucket empty part", []string{"--bucket", "a..b", "--project", "cluster-project"}, "not a Cloud Storage bucket name"},
		{"bucket IP", []string{"--bucket", "192.168.1.1", "--project", "cluster-project"}, "IP address"},
		{"bucket goog", []string{"--bucket", "goog-spans", "--project", "cluster-project"}, "goog"},
		{"bucket g00gle", []string{"--bucket", "my-g00gle-x", "--project", "cluster-project"}, "misspelling"},
		{"bucket upper case", []string{"--bucket", "Fleet", "--project", "cluster-project"}, "not a Cloud Storage bucket name"},
		{"empty image", []string{"--bucket", "fleet-bucket", "--project", "cluster-project", "--image", ""}, "--image is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runManifestCollector(t, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one with %q", err, tc.want)
			}
			// Cobra prints the usage on error; no object may precede it.
			if strings.Contains(out, "kind:") {
				t.Errorf("printed a manifest on error:\n%s", out)
			}
		})
	}
}
