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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-substrate/env/clients/go"
	"github.com/agent-substrate/env/guest"
	"github.com/agent-substrate/env/internal/apiservice"
	"github.com/agent-substrate/env/internal/ate"
	"github.com/agent-substrate/env/internal/envstore"
	"github.com/agent-substrate/env/internal/internaltest/fakecontrol"
	"github.com/agent-substrate/env/internal/internaltest/fakerouter"
	"github.com/agent-substrate/env/internal/mcp"
	"github.com/agent-substrate/env/internal/unknownfields"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// fixture runs the full stack the SDK talks to: a fake Substrate control
// plane and router behind a real ate-env-api handler.
type fixture struct {
	router  *fakerouter.Router
	control *fakecontrol.Server
	service *apiservice.Server
	client  *env.Client
	guest   string // guest workdir
	apiURL  string
	calls   *callLog
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	t.Chdir(t.TempDir())

	control := fakecontrol.New()
	controlAddr, stopControl, err := control.Serve()
	if err != nil {
		t.Fatalf("starting fake control plane: %v", err)
	}
	t.Cleanup(stopControl)

	router := fakerouter.New()
	router.Running = func(id string) bool {
		return control.Status(id) == ateapipb.ActorState_ACTOR_STATE_RUNNING
	}
	routerAddr, stopRouter := router.Serve()
	t.Cleanup(stopRouter)

	directClient, err := ate.New(ate.Options{
		ControlAddr: controlAddr,
		RouterAddr:  routerAddr,
		SkipVerify:  true,
	})
	if err != nil {
		t.Fatalf("creating direct client: %v", err)
	}
	t.Cleanup(func() { directClient.Close() })

	service := apiservice.New(directClient, routerAddr, "")
	t.Cleanup(service.Close)

	// The interceptors ate-env-api runs, so each call the SDK makes is checked
	// for fields the server does not know, after the call log's.
	calls := newCallLog()
	grpcServer := grpc.NewServer(
		grpc.ChainUnaryInterceptor(calls.unary, unknownfields.UnaryServerInterceptor()),
		grpc.ChainStreamInterceptor(calls.stream, unknownfields.StreamServerInterceptor()),
	)
	ateenvv1alpha.RegisterEnvironmentServiceServer(grpcServer, service)
	ateenvv1alpha.RegisterProcessServiceServer(grpcServer, service)
	ateenvv1alpha.RegisterFileSystemServiceServer(grpcServer, service)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok\n")
	})
	mux.Handle("/v1alpha/envs/{id}/mcp", mcp.NewHandler(directClient))
	mux.Handle("/", grpcServer)

	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)

	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = &protocols
	srv.Config.ConnState = calls.connState
	srv.Start()
	t.Cleanup(srv.Close)

	client, err := env.NewClient(env.ClientOptions{
		Endpoint: srv.URL,
	})
	if err != nil {
		t.Fatalf("creating SDK client: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	return &fixture{router: router, control: control, service: service, client: client, guest: t.TempDir(), apiURL: srv.URL, calls: calls}
}

// create makes a env whose guest handler serves from a temp dir.
func (f *fixture) create(t *testing.T, id string) *env.Env {
	t.Helper()
	return f.createWith(t, f.client, id)
}

// createWith is create through client.
func (f *fixture) createWith(t *testing.T, client *env.Client, id string) *env.Env {
	t.Helper()
	workDir := t.TempDir()
	grpcGuestServer, cleanup, err := guest.NewServer(guest.Config{
		Workspace:        workDir,
		EnableProcess:    true,
		EnableFileSystem: true,
	})
	if err != nil {
		t.Fatalf("creating guest server: %v", err)
	}
	t.Cleanup(cleanup)

	f.router.Register(id, grpcGuestServer)

	sb, err := client.Create(t.Context(), &ateenvv1alpha.CreateEnvironmentRequest{
		Id: id,
		Template: &ateenvv1alpha.Template{
			Name:     "default-template",
			Atespace: "envs",
		},
	})
	if err != nil {
		t.Fatalf("creating env %q: %v", id, err)
	}
	return sb
}

func TestCreateStartsEnv(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-1")
	if sb.ID() != "sb-1" {
		t.Errorf("ID = %s, want sb-1", sb.ID())
	}
}

func TestSuspend(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-susp")
	ctx := t.Context()

	if err := sb.Suspend(ctx); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	if st := f.control.Status("sb-susp"); st != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		t.Errorf("status = %v, want SUSPENDED", st)
	}
	if got := f.control.SuspendModes; len(got) != 1 || got[0] != ateapipb.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS {
		t.Errorf("suspend modes = %v, want one SYNCHRONOUS", got)
	}
}

func TestSuspendLocal(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-local")
	ctx := t.Context()

	if err := sb.SuspendLocal(ctx); err != nil {
		t.Fatalf("SuspendLocal: %v", err)
	}
	if st := f.control.Status("sb-local"); st != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		t.Errorf("status = %v, want SUSPENDED", st)
	}
	if got := f.control.SuspendModes; len(got) != 1 || got[0] != ateapipb.DurabilityMode_DURABILITY_MODE_ASYNCHRONOUS {
		t.Errorf("suspend modes = %v, want one ASYNCHRONOUS", got)
	}
}

func TestDelete(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-life")
	ctx := t.Context()

	if err := sb.Delete(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestFork: Client.Fork and Env.Fork return handles to the copies the real
// handler creates (AE-11 of docs/plans/ate-env-api/acceptance-criteria.md),
// and a refused request comes back as the server's code.
func TestFork(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-src")
	ctx := t.Context()

	copies, err := f.client.Fork(ctx, &ateenvv1alpha.ForkEnvironmentRequest{
		Id: "sb-src", Atespace: sb.Atespace(), Ids: []string{"sb-c0", "sb-c1"},
	})
	if err != nil {
		t.Fatalf("Client.Fork: %v", err)
	}
	if len(copies) != 2 || copies[0].ID() != "sb-c0" || copies[1].ID() != "sb-c1" || copies[0].Atespace() != sb.Atespace() {
		t.Fatalf("Client.Fork copies = %v, want sb-c0 and sb-c1 in %s", copies, sb.Atespace())
	}
	more, err := sb.Fork(ctx, 3)
	if err != nil {
		t.Fatalf("Env.Fork: %v", err)
	}
	if len(more) != 3 {
		t.Fatalf("Env.Fork returned %d copies, want 3", len(more))
	}
	for _, c := range append(copies, more...) {
		if _, err := c.Info(ctx); err != nil {
			t.Errorf("Info(%s): %v", c.ID(), err)
		}
	}
	if _, err := sb.Fork(ctx, 0); status.Code(err) != codes.InvalidArgument {
		t.Errorf("Env.Fork(0) = %v, want InvalidArgument", err)
	}
}

// TestList: List reads the environments of an atespace page by page through
// the real handler, with the status filter (AE-6, AE-8 and AE-9 of
// docs/plans/ate-env-api/acceptance-criteria.md) and the create time (AE-10).
func TestList(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	for _, id := range []string{"sb-a", "sb-b", "sb-c"} {
		f.create(t, id)
	}
	if err := f.client.Suspend(ctx, "envs", "sb-b"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	var got []string
	req := &ateenvv1alpha.ListEnvironmentsRequest{Atespace: "envs", PageSize: 2}
	for pages := 0; ; pages++ {
		resp, err := f.client.List(ctx, req)
		if err != nil {
			t.Fatalf("List(token=%q): %v", req.GetPageToken(), err)
		}
		for _, e := range resp.GetEnvironments() {
			got = append(got, e.GetId())
			if e.GetCreateTime() == nil {
				t.Errorf("%s has create_time %v, want a time", e.GetId(), e.GetCreateTime())
			}
		}
		if req.PageToken = resp.GetNextPageToken(); req.PageToken == "" {
			if pages != 1 {
				t.Errorf("3 environments in pages of 2 took %d pages, want 2", pages+1)
			}
			break
		}
	}
	if want := []string{"sb-a", "sb-b", "sb-c"}; !slices.Equal(got, want) {
		t.Errorf("List read %v, want %v", got, want)
	}

	resp, err := f.client.List(ctx, &ateenvv1alpha.ListEnvironmentsRequest{
		Atespace: "envs",
		Statuses: []ateenvv1alpha.EnvironmentStatus{ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_SUSPENDED},
	})
	if err != nil {
		t.Fatalf("List(SUSPENDED): %v", err)
	}
	if envs := resp.GetEnvironments(); len(envs) != 1 || envs[0].GetId() != "sb-b" {
		t.Errorf("List(SUSPENDED) = %v, want sb-b alone", envs)
	}

	// A nil request lists the default atespace, which has no environments.
	resp, err = f.client.List(ctx, nil)
	if err != nil {
		t.Fatalf("List(nil): %v", err)
	}
	if len(resp.GetEnvironments()) != 0 {
		t.Errorf("List(nil) = %v, want no environments in the default atespace", resp.GetEnvironments())
	}

	if _, err := f.client.List(ctx, &ateenvv1alpha.ListEnvironmentsRequest{PageSize: -1}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("List(page_size=-1): got %v, want InvalidArgument", err)
	}
}

// storeOn turns on the environment store of a fixture's ate-env-api, on a
// fake Kubernetes that gives each object the identity the API server gives.
func storeOn(t *testing.T, service *apiservice.Server) *envstore.Store {
	t.Helper()
	kube := fake.NewSimpleClientset()
	var n atomic.Int64
	kube.PrependReactor("create", "configmaps", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		cm := action.(k8stesting.CreateAction).GetObject().(*corev1.ConfigMap)
		cm.UID, cm.ResourceVersion, cm.CreationTimestamp = types.UID(fmt.Sprintf("object-%d", n.Add(1))), "1", metav1.Now()
		return false, nil, nil
	})
	store := envstore.New(kube.CoreV1().ConfigMaps("ate-env"), "ate-env")
	if err := store.Run(t.Context(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	service.Store = store
	return store
}

// TestTimeToLive checks the client side of EM-9, EM-10 and EM-1: a create
// with TimeToLive gives the environment a deadline that Get reports,
// SetTimeToLive moves it, counted from the call, and removes it with zero,
// and a server without the environment store refuses both with
// FailedPrecondition.
func TestTimeToLive(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	// The server runs without the store.
	if _, err := f.client.Create(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: "sb-off", Atespace: "envs", TimeToLive: durationpb.New(time.Minute)}); !errors.Is(err, env.ErrFailedPrecondition) {
		t.Errorf("Create with TimeToLive, store off: got %v, want ErrFailedPrecondition", err)
	}
	plain := f.create(t, "sb-plain")
	if _, err := plain.SetTimeToLive(ctx, time.Minute); !errors.Is(err, env.ErrFailedPrecondition) {
		t.Errorf("SetTimeToLive, store off: got %v, want ErrFailedPrecondition", err)
	}

	store := storeOn(t, f.service)
	informed := func(want func(envstore.Record, bool) bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			r, ok, err := store.Get("envs", "sb-ttl")
			if err != nil {
				t.Fatal(err)
			}
			if want(r, ok) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the informer did not reach the wanted state; last %+v, %v", r, ok)
			}
		}
	}

	info, err := f.client.ServerInfo(ctx)
	if err != nil || !slices.Contains(info.GetFeatures(), "time_to_live") {
		t.Fatalf("ServerInfo features = %v, %v, want time_to_live", info.GetFeatures(), err)
	}
	e, err := f.client.Create(ctx, &ateenvv1alpha.CreateEnvironmentRequest{Id: "sb-ttl", Atespace: "envs", TimeToLive: durationpb.New(time.Minute)})
	if err != nil {
		t.Fatalf("Create with TimeToLive: %v", err)
	}
	informed(func(_ envstore.Record, ok bool) bool { return ok })
	got, err := e.Info(ctx)
	if err != nil || got.GetExpireTime() == nil || !got.GetExpireTime().AsTime().Equal(got.GetCreateTime().AsTime().Add(time.Minute)) {
		t.Fatalf("Info: expire_time = %v, create_time = %v, %v; want the create time plus one minute", got.GetExpireTime(), got.GetCreateTime(), err)
	}

	before := time.Now()
	moved, err := f.client.SetTimeToLive(ctx, "envs", "sb-ttl", time.Hour)
	if err != nil {
		t.Fatalf("SetTimeToLive: %v", err)
	}
	if at := moved.GetExpireTime().AsTime(); moved.GetId() != "sb-ttl" || at.Before(before.Add(time.Hour)) || at.After(time.Now().Add(time.Hour)) {
		t.Errorf("SetTimeToLive = %v, want sb-ttl with a deadline one hour after the call", moved)
	}
	cleared, err := e.SetTimeToLive(ctx, 0)
	if err != nil || cleared.GetExpireTime() != nil {
		t.Errorf("SetTimeToLive(0) = %v, %v, want no expire_time", cleared, err)
	}
	informed(func(r envstore.Record, ok bool) bool { return ok && r.ExpireTime.IsZero() })
	if got, err := e.Info(ctx); err != nil || got.GetExpireTime() != nil {
		t.Errorf("Info after the deadline was removed: expire_time = %v, %v", got.GetExpireTime(), err)
	}

	if _, err := e.SetTimeToLive(ctx, -time.Second); !errors.Is(err, env.ErrInvalidArgument) {
		t.Errorf("SetTimeToLive(-1s): got %v, want ErrInvalidArgument", err)
	}
	if _, err := f.client.SetTimeToLive(ctx, "envs", "sb-none", time.Minute); !errors.Is(err, env.ErrNotFound) {
		t.Errorf("SetTimeToLive of an unknown environment: got %v, want ErrNotFound", err)
	}
}

// The client's DefaultAtespace is a copy of the server's (the client imports
// no internal package outside tests); the two must stay equal.
func TestDefaultAtespaceMatchesTheServer(t *testing.T) {
	if env.DefaultAtespace != ate.DefaultAtespace {
		t.Errorf("env.DefaultAtespace = %q, ate.DefaultAtespace = %q", env.DefaultAtespace, ate.DefaultAtespace)
	}
}

func TestClientServerInfo(t *testing.T) {
	f := newFixture(t)
	info, err := f.client.ServerInfo(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if info.GetServer() != apiservice.ServerName {
		t.Errorf("server = %q, want %q", info.GetServer(), apiservice.ServerName)
	}
	if info.GetVersion() != "dev" {
		t.Errorf("version = %q, want \"dev\" for a server without a build version", info.GetVersion())
	}
	for _, want := range []string{apiservice.FeatureRootfsImage, apiservice.FeaturePTY, apiservice.FeatureSuspendMode} {
		if !slices.Contains(info.GetFeatures(), want) {
			t.Errorf("features = %v, want %q", info.GetFeatures(), want)
		}
	}
}

func TestCmdAndFilesystem(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-fs")
	ctx := t.Context()

	if err := sb.WriteFile(ctx, "project/hello.txt", strings.NewReader("hi there"), 0o644); err != nil {
		t.Fatal(err)
	}
	rc, err := sb.ReadFile(ctx, "project/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hi there" {
		t.Errorf("read back %q, want %q", data, "hi there")
	}

	res, err := sb.Shell(ctx, "echo 'hi there'")
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "hi there\n" || res.ExitCode != 0 {
		t.Errorf("cmd result = %+v, want stdout %q", res, "hi there\n")
	}
}

func TestShellStderrAndExitCode(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-stderr")
	ctx := t.Context()

	res, err := sb.Shell(ctx, "echo err-out >&2; exit 42")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Stderr) != "err-out" {
		t.Errorf("stderr = %q, want %q", res.Stderr, "err-out")
	}
	if res.ExitCode != 42 {
		t.Errorf("exit code = %d, want 42", res.ExitCode)
	}
}

func TestWriteFileAt(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-seek")
	ctx := t.Context()

	if err := sb.WriteFile(ctx, "seek.txt", strings.NewReader("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFileAt(ctx, "seek.txt", 6, strings.NewReader("W"), 0o644); err != nil {
		t.Fatal(err)
	}
	rc, err := sb.ReadFile(ctx, "seek.txt")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if string(data) != "hello World" {
		t.Errorf("after WriteFileAt: %q, want %q", data, "hello World")
	}
	if err := sb.WriteFileAt(ctx, "seek.txt", -1, strings.NewReader("x"), 0o644); err == nil {
		t.Error("negative offset should be rejected")
	}
}

func TestReadFileMissing(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-missing")
	ctx := t.Context()

	_, err := sb.ReadFile(ctx, "nonexistent.txt")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !errors.Is(err, env.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestLargeFileStreaming(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-large")
	ctx := t.Context()

	// 256 KB file across multiple stream chunks
	largeData := bytes.Repeat([]byte("0123456789abcdef"), 16*1024)
	if err := sb.WriteFile(ctx, "large.dat", bytes.NewReader(largeData), 0o644); err != nil {
		t.Fatal(err)
	}

	rc, err := sb.ReadFile(ctx, "large.dat")
	if err != nil {
		t.Fatal(err)
	}
	readBack, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(readBack) != len(largeData) {
		t.Fatalf("read %d bytes, want %d", len(readBack), len(largeData))
	}
	if !bytes.Equal(readBack, largeData) {
		t.Error("readBack content mismatch")
	}
}

func TestRunWithStdin(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-stdin")
	ctx := t.Context()

	res, err := sb.Run(ctx, env.ShellRequest{Command: "tr a-z A-Z", Stdin: []byte("shout\n")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "SHOUT\n" || res.ExitCode != 0 {
		t.Errorf("run result = %+v, want stdout %q", res, "SHOUT\n")
	}
}

func TestProcessInteractiveStdinAndOutput(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-proc")
	ctx := t.Context()

	proc, err := sb.StartProcess(ctx, &ateenvv1alpha.StartProcessRequest{Command: []string{"cat"}, Stdin: true})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := proc.Output(ctx, &ateenvv1alpha.StreamProcessOutputRequest{Follow: true})
	if err != nil {
		t.Fatal(err)
	}

	stdin, err := proc.Stdin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	out, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if string(out.GetStdout()) != "first\n" {
		t.Fatalf("first output = %v", out)
	}
	if _, err := stdin.Write([]byte("second\n")); err != nil {
		t.Fatal(err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}

	var rest string
	var exit *ateenvv1alpha.Process
	for {
		out, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		rest += string(out.GetStdout())
		if out.GetExit() != nil {
			exit = out.GetExit()
		}
	}
	if rest != "second\n" {
		t.Errorf("remaining output = %q", rest)
	}
	if exit == nil || exit.GetState() != ateenvv1alpha.ProcessState_PROCESS_STATE_EXITED || exit.GetExitCode() != 0 {
		t.Errorf("exit = %v, want exited with 0", exit)
	}

	// The handle still resolves after exit; stdin is refused.
	info, err := sb.FindProcess(ctx, proc.ID())
	if err != nil {
		t.Fatal(err)
	}
	if info.GetState() != ateenvv1alpha.ProcessState_PROCESS_STATE_EXITED || info.GetPid() == 0 || len(info.GetCommand()) != 1 {
		t.Errorf("info = %v", info)
	}
	w, err := proc.Stdin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("late"))
	if err := w.Close(); !errors.Is(err, env.ErrProcessExited) {
		t.Errorf("stdin after exit: err = %v, want ErrProcessExited", err)
	}
}

func TestProcessSignalAndKill(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-sig")
	ctx := t.Context()

	proc, err := sb.StartProcess(ctx, &ateenvv1alpha.StartProcessRequest{Command: []string{"sleep", "60"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Signal(ctx, ateenvv1alpha.Signal_SIGNAL_TERM); err != nil {
		t.Fatal(err)
	}
	info, err := proc.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.GetState() != ateenvv1alpha.ProcessState_PROCESS_STATE_EXITED || info.GetExitCode() != 143 {
		t.Errorf("after SIGTERM: %v", info)
	}
	if err := proc.Signal(ctx, ateenvv1alpha.Signal_SIGNAL_KILL); !errors.Is(err, env.ErrProcessExited) {
		t.Errorf("signal after exit: err = %v, want ErrProcessExited", err)
	}

	sleeper, err := sb.StartProcess(ctx, &ateenvv1alpha.StartProcessRequest{Command: []string{"sleep", "60"}})
	if err != nil {
		t.Fatal(err)
	}
	killed, err := sleeper.Kill(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if killed.GetExitCode() != 137 {
		t.Errorf("kill: %v", killed)
	}
	// Kill is idempotent.
	if _, err := sleeper.Kill(ctx); err != nil {
		t.Errorf("second kill: %v", err)
	}

	if _, err := sb.FindProcess(ctx, "bogus"); !errors.Is(err, env.ErrNotFound) {
		t.Errorf("bogus process: err = %v, want ErrNotFound", err)
	}
}

func TestShellKilledByTimeout(t *testing.T) {
	f := newFixture(t)
	sb := f.create(t, "sb-timeout")

	res, err := sb.Run(t.Context(), env.ShellRequest{Command: "sleep 30", Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 137 {
		t.Errorf("timed out run = %+v, want exit code 137", res)
	}
}

func TestProcessTTYAndResize(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("pty allocation is only supported on linux")
	}
	f := newFixture(t)
	sb := f.create(t, "sb-tty")
	ctx := t.Context()

	proc, err := sb.StartProcess(ctx, &ateenvv1alpha.StartProcessRequest{
		Command: []string{"sh", "-c", `trap 'echo "size:$(stty size)"; exit 0' WINCH; echo ready; while :; do sleep 0.05; done`},
		Tty:     &ateenvv1alpha.TTY{Rows: 25, Cols: 90},
	})
	if err != nil {
		t.Fatal(err)
	}

	stream, err := proc.Output(ctx, &ateenvv1alpha.StreamProcessOutputRequest{Follow: true})
	if err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder
	for !strings.Contains(buf.String(), "ready") {
		msg, err := stream.Recv()
		if err != nil {
			t.Fatalf("waiting for ready: %v", err)
		}
		buf.Write(msg.GetStdout())
	}

	if err := proc.Resize(ctx, 50, 132); err != nil {
		t.Fatalf("Resize: %v", err)
	}

	var exit *ateenvv1alpha.Process
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reading output after resize: %v", err)
		}
		buf.Write(msg.GetStdout())
		if msg.GetExit() != nil {
			exit = msg.GetExit()
		}
	}

	if !strings.Contains(buf.String(), "size:50 132") {
		t.Errorf("output = %q, want size:50 132", buf.String())
	}
	if exit == nil || exit.GetExitCode() != 0 {
		t.Errorf("exit = %v, want exit_code 0", exit)
	}
}

func TestClientAuthToken(t *testing.T) {
	t.Chdir(t.TempDir())

	control := fakecontrol.New()
	controlAddr, stopControl, err := control.Serve()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stopControl)

	router := fakerouter.New()
	router.Running = func(id string) bool {
		return control.Status(id) == ateapipb.ActorState_ACTOR_STATE_RUNNING
	}
	routerAddr, stopRouter := router.Serve()
	t.Cleanup(stopRouter)

	directClient, err := ate.New(ate.Options{
		ControlAddr: controlAddr,
		RouterAddr:  routerAddr,
		SkipVerify:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { directClient.Close() })

	service := apiservice.New(directClient, routerAddr, "")
	t.Cleanup(service.Close)

	auth, err := apiservice.NewBearerAuth("top-secret")
	if err != nil {
		t.Fatal(err)
	}

	// headers records how many authorization values each RPC carried: the
	// token must arrive exactly once, on unary and streaming RPCs alike.
	var (
		mu      sync.Mutex
		headers = map[string][]int{}
	)
	record := func(ctx context.Context, method string) {
		md, _ := metadata.FromIncomingContext(ctx)
		mu.Lock()
		headers[method] = append(headers[method], len(md.Get("authorization")))
		mu.Unlock()
	}
	grpcServer := grpc.NewServer(
		grpc.ChainUnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			record(ctx, info.FullMethod)
			return handler(ctx, req)
		}, auth.UnaryInterceptor(), unknownfields.UnaryServerInterceptor()),
		grpc.ChainStreamInterceptor(func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			record(ss.Context(), info.FullMethod)
			return handler(srv, ss)
		}, auth.StreamInterceptor(), unknownfields.StreamServerInterceptor()),
	)
	ateenvv1alpha.RegisterEnvironmentServiceServer(grpcServer, service)
	ateenvv1alpha.RegisterProcessServiceServer(grpcServer, service)
	ateenvv1alpha.RegisterFileSystemServiceServer(grpcServer, service)

	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)

	srv := httptest.NewUnstartedServer(grpcServer)
	srv.Config.Protocols = &protocols
	srv.Start()
	t.Cleanup(srv.Close)

	unauthClient, err := env.NewClient(env.ClientOptions{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unauthClient.Close() })

	if _, err := unauthClient.Create(t.Context(), &ateenvv1alpha.CreateEnvironmentRequest{Id: "sb-auth"}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unauthenticated Create: got %v, want Unauthenticated", err)
	}
	if _, err := unauthClient.ServerInfo(t.Context()); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unauthenticated ServerInfo: got %v, want Unauthenticated", err)
	}

	exercise := func(t *testing.T, client *env.Client, id string) {
		mu.Lock()
		clear(headers)
		mu.Unlock()

		grpcGuestServer, cleanup, err := guest.NewServer(guest.Config{
			Workspace:        t.TempDir(),
			EnableProcess:    true,
			EnableFileSystem: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(cleanup)
		router.Register(id, grpcGuestServer)

		sb, err := client.Create(t.Context(), &ateenvv1alpha.CreateEnvironmentRequest{Id: id})
		if err != nil {
			t.Fatalf("authenticated Create: %v", err)
		}
		if err := sb.WriteFile(t.Context(), "note.txt", strings.NewReader("hello"), 0o644); err != nil {
			t.Fatalf("authenticated WriteFile: %v", err)
		}
		rc, err := sb.ReadFile(t.Context(), "note.txt")
		if err != nil {
			t.Fatalf("authenticated ReadFile: %v", err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil || string(data) != "hello" {
			t.Fatalf("ReadFile = %q, err = %v", data, err)
		}
		res, err := sb.Run(t.Context(), env.ShellRequest{Command: "cat", Stdin: []byte("pong")})
		if err != nil || res.Stdout != "pong" {
			t.Fatalf("authenticated Run = %+v, err = %v", res, err)
		}
		if err := sb.Suspend(t.Context()); err != nil {
			t.Fatalf("authenticated Suspend: %v", err)
		}
		if err := sb.Delete(t.Context()); err != nil {
			t.Fatalf("authenticated Delete: %v", err)
		}

		mu.Lock()
		defer mu.Unlock()
		for _, m := range []string{
			"/ateenv.v1alpha.EnvironmentService/CreateEnvironment",
			"/ateenv.v1alpha.FileSystemService/WriteFile",
			"/ateenv.v1alpha.FileSystemService/ReadFile",
			"/ateenv.v1alpha.ProcessService/StreamProcessOutput",
			"/ateenv.v1alpha.EnvironmentService/DeleteEnvironment",
		} {
			if len(headers[m]) == 0 {
				t.Errorf("no call to %s recorded", m)
			}
		}
		for m, counts := range headers {
			for _, n := range counts {
				if n != 1 {
					t.Errorf("%s carried %d authorization values, want 1", m, n)
				}
			}
		}
	}

	t.Run("own connection", func(t *testing.T) {
		client, err := env.NewClient(env.ClientOptions{Endpoint: srv.URL, AuthToken: "top-secret"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { client.Close() })
		exercise(t, client, "sb-auth-own")
	})

	t.Run("caller connection", func(t *testing.T) {
		conn, err := grpc.NewClient(strings.TrimPrefix(srv.URL, "http://"), grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		client, err := env.NewClient(env.ClientOptions{GRPCConn: conn, AuthToken: "top-secret"})
		if err != nil {
			t.Fatal(err)
		}
		exercise(t, client, "sb-auth-conn")
	})
}

// ListPlacementDomains and WarmImage reach ate-env-api's placement RPCs: the
// pools come back grouped from the fake control plane's workers, and a warm
// covers a domain's nodes through the streamed probes.
func TestPlacementDomainsAndWarmImage(t *testing.T) {
	f := newFixture(t)
	labels := map[string]string{"workload": "default-env", "ate-env/domain": "d0"}
	f.control.AddWorker(fakecontrol.Worker("ate-system", "pool-d0", "node-1", labels, 8))
	f.control.AddWorker(fakecontrol.Worker("ate-system", "pool-d0", "node-2", labels, 8))
	f.control.AddTemplate(&ateapipb.ActorTemplate{
		Metadata:       &ateapipb.ResourceMetadata{Atespace: "ate-env", Name: "default-template"},
		WorkerSelector: &ateapipb.Selector{MatchLabels: map[string]string{"workload": "default-env"}},
		Containers: []*ateapipb.Container{{
			Name:        "guest",
			Image:       "registry.example/ate-env-guest@sha256:" + strings.Repeat("ab", 32),
			Command:     []string{"/ko-app/ate-env-guest"},
			WakeupProbe: &ateapipb.WakeupProbe{HttpGet: &ateapipb.HttpGetAction{Path: "/readyz", Port: 80}},
		}},
		SnapshotConfig: &ateapipb.SnapshotConfig{StorageLocation: "gs://bucket/ate-env/"},
	})

	f.control.AddWorker(fakecontrol.Worker("ate-system", "pool-other", "node-3", map[string]string{"workload": "other"}, 8))

	domains, err := f.client.ListPlacementDomains(t.Context(), nil)
	if err != nil {
		t.Fatalf("ListPlacementDomains: %v", err)
	}
	if len(domains) != 2 || domains[0].GetName() != "pool-d0" || domains[0].GetNodes() != 2 || domains[0].GetNodeSlots() != 8 || !maps.Equal(domains[0].GetSelector(), labels) {
		t.Errorf("domains = %v, want pool-d0 with 2 nodes, 8 slots and the labels, then pool-other", domains)
	}
	// With the template, only the pools its own selector matches.
	domains, err = f.client.ListPlacementDomains(t.Context(), &ateenvv1alpha.ListPlacementDomainsRequest{Template: &ateenvv1alpha.Template{Name: "default-template"}})
	if err != nil {
		t.Fatalf("ListPlacementDomains(template): %v", err)
	}
	if len(domains) != 1 || domains[0].GetName() != "pool-d0" {
		t.Errorf("domains for default-template = %v, want pool-d0 only", domains)
	}
	if _, err := f.client.ListPlacementDomains(t.Context(), &ateenvv1alpha.ListPlacementDomainsRequest{Template: &ateenvv1alpha.Template{Name: "nope"}}); !errors.Is(err, env.ErrNotFound) {
		t.Errorf("unknown template: err = %v, want ErrNotFound", err)
	}

	var probes []*ateenvv1alpha.WarmProbe
	cov, err := f.client.WarmImage(t.Context(), &ateenvv1alpha.WarmImageRequest{
		RootfsImage:    "registry.example/task@sha256:" + strings.Repeat("cd", 32),
		WorkerSelector: map[string]string{"ate-env/domain": "d0"},
		ProbesPerNode:  1,
		RunId:          "pf-1",
	}, func(p *ateenvv1alpha.WarmProbe) { probes = append(probes, p) })
	if err != nil {
		t.Fatalf("WarmImage: %v", err)
	}
	if !cov.GetComplete() || !slices.Equal(cov.GetNodes(), []string{"node-1", "node-2"}) || cov.GetRounds() != 1 || len(probes) != 2 {
		t.Errorf("coverage = %v with %d probes, want both nodes in one round of two probes", cov, len(probes))
	}
	if f.control.ActorCount() != 0 {
		t.Errorf("%d actors left after the warm, want 0", f.control.ActorCount())
	}
	if _, err := f.client.WarmImage(t.Context(), &ateenvv1alpha.WarmImageRequest{RootfsImage: "registry.example/task:latest"}, nil); err == nil {
		t.Error("an unpinned image was accepted")
	}
}
