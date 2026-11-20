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

package process

import (
	"context"
	"errors"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/durationpb"
)

func setupTestServer(t *testing.T) ateenvv1alpha.ProcessServiceClient {
	t.Helper()
	return setupTestServerWithConfig(t, DefaultConfig(t.TempDir()))
}

func setupTestServerWithConfig(t *testing.T, cfg TrackerConfig) ateenvv1alpha.ProcessServiceClient {
	t.Helper()
	if cfg.LogDir == "" {
		cfg.LogDir = t.TempDir()
	}

	tracker, err := NewTracker(cfg)
	if err != nil {
		t.Fatalf("failed to create tracker: %v", err)
	}

	lis := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	ateenvv1alpha.RegisterProcessServiceServer(server, NewService(tracker))
	go func() { _ = server.Serve(lis) }()

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("failed to dial bufnet: %v", err)
	}

	t.Cleanup(func() {
		tracker.Close()
		conn.Close()
		server.Stop()
		lis.Close()
		_ = os.RemoveAll(cfg.LogDir)
	})
	return ateenvv1alpha.NewProcessServiceClient(conn)
}

func start(t *testing.T, client ateenvv1alpha.ProcessServiceClient, req *ateenvv1alpha.StartProcessRequest) *ateenvv1alpha.Process {
	t.Helper()
	proc, err := client.StartProcess(context.Background(), req)
	if err != nil {
		t.Fatalf("StartProcess failed: %v", err)
	}
	if proc.GetProcessId() == "" || proc.GetPid() == 0 {
		t.Fatalf("expected process id and pid, got %v", proc)
	}
	if proc.GetState() != ateenvv1alpha.ProcessState_PROCESS_STATE_RUNNING {
		t.Fatalf("expected RUNNING right after start, got %v", proc.GetState())
	}
	return proc
}

func sh(t *testing.T, client ateenvv1alpha.ProcessServiceClient, script string) *ateenvv1alpha.Process {
	t.Helper()
	return start(t, client, &ateenvv1alpha.StartProcessRequest{Command: []string{"sh", "-c", script}})
}

// wait follows the output stream with offsets past the spool end, so only
// the exit message is delivered.
func wait(t *testing.T, client ateenvv1alpha.ProcessServiceClient, id string) *ateenvv1alpha.Process {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := client.StreamProcessOutput(ctx, &ateenvv1alpha.StreamProcessOutputRequest{
		ProcessId: id, Follow: true, StdoutOffset: math.MaxInt64, StderrOffset: math.MaxInt64,
	})
	if err != nil {
		t.Fatalf("StreamProcessOutput failed: %v", err)
	}
	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("waiting for process: %v", err)
	}
	proc := msg.GetExit()
	if proc == nil {
		t.Fatalf("expected only an exit message with offsets past the end, got %v", msg)
	}
	if proc.GetState() != ateenvv1alpha.ProcessState_PROCESS_STATE_EXITED {
		t.Fatalf("expected EXITED after wait, got %v", proc.GetState())
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF after exit, got %v", err)
	}
	return proc
}

// collect drains an output stream into stdout, stderr and the final exit message.
func collect(t *testing.T, client ateenvv1alpha.ProcessServiceClient, req *ateenvv1alpha.StreamProcessOutputRequest) (string, string, *ateenvv1alpha.Process) {
	t.Helper()
	stream, err := client.StreamProcessOutput(context.Background(), req)
	if err != nil {
		t.Fatalf("StreamProcessOutput failed: %v", err)
	}
	var stdout, stderr strings.Builder
	var exit *ateenvv1alpha.Process
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return stdout.String(), stderr.String(), exit
		}
		if err != nil {
			t.Fatalf("stream recv: %v", err)
		}
		switch out := msg.GetOutput().(type) {
		case *ateenvv1alpha.ProcessOutput_Stdout:
			stdout.Write(out.Stdout)
		case *ateenvv1alpha.ProcessOutput_Stderr:
			stderr.Write(out.Stderr)
		case *ateenvv1alpha.ProcessOutput_Exit:
			if exit != nil {
				t.Fatalf("received two exit messages")
			}
			exit = out.Exit
		default:
			t.Fatalf("unexpected output %T", out)
		}
		if exit != nil {
			// exit must be the last message.
			if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
				t.Fatalf("expected EOF after exit message, got %v", err)
			}
			return stdout.String(), stderr.String(), exit
		}
	}
}

func TestStartAndWait(t *testing.T) {
	client := setupTestServer(t)
	started := sh(t, client, "echo 'hello from substrate'")
	if got := started.GetCommand(); len(got) != 3 || got[0] != "sh" {
		t.Fatalf("command not echoed back: %v", got)
	}

	proc := wait(t, client, started.GetProcessId())
	if proc.GetExitCode() != 0 {
		t.Fatalf("expected exit_code 0, got %d", proc.GetExitCode())
	}
	if proc.GetStartedAt() == nil || proc.GetFinishedAt() == nil {
		t.Fatalf("expected non-nil timestamps")
	}

	got, err := client.GetProcess(context.Background(), &ateenvv1alpha.GetProcessRequest{ProcessId: started.GetProcessId()})
	if err != nil {
		t.Fatalf("GetProcess: %v", err)
	}
	if got.GetState() != ateenvv1alpha.ProcessState_PROCESS_STATE_EXITED {
		t.Fatalf("GetProcess state = %v, want EXITED", got.GetState())
	}
}

func TestNonZeroExitCode(t *testing.T) {
	client := setupTestServer(t)
	proc := wait(t, client, sh(t, client, "exit 42").GetProcessId())
	if proc.GetExitCode() != 42 {
		t.Fatalf("expected exit_code 42, got %d", proc.GetExitCode())
	}
}

func TestSelfSignalDeath(t *testing.T) {
	client := setupTestServer(t)
	proc := wait(t, client, sh(t, client, "kill -15 $$").GetProcessId())
	if proc.GetExitCode() != 143 {
		t.Fatalf("expected exit_code 143 (128+SIGTERM), got %d", proc.GetExitCode())
	}
}

func TestStreamOutputFollowEndsWithExit(t *testing.T) {
	client := setupTestServer(t)
	started := sh(t, client, "echo 'out1'; echo 'err1' >&2; sleep 0.1; echo 'out2'; exit 3")

	stdout, stderr, exit := collect(t, client, &ateenvv1alpha.StreamProcessOutputRequest{
		ProcessId: started.GetProcessId(), Follow: true,
	})
	if stdout != "out1\nout2\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	if stderr != "err1\n" {
		t.Fatalf("stderr = %q", stderr)
	}
	if exit == nil || exit.GetExitCode() != 3 {
		t.Fatalf("expected exit message with code 3, got %v", exit)
	}
}

func TestStreamOutputOffsets(t *testing.T) {
	client := setupTestServer(t)
	started := sh(t, client, "printf abcdef; printf 123456 >&2")
	wait(t, client, started.GetProcessId())

	stdout, stderr, exit := collect(t, client, &ateenvv1alpha.StreamProcessOutputRequest{
		ProcessId: started.GetProcessId(), StdoutOffset: 4, StderrOffset: 2,
	})
	if stdout != "ef" || stderr != "3456" {
		t.Fatalf("got stdout %q stderr %q", stdout, stderr)
	}
	if exit == nil {
		t.Fatalf("snapshot of an exited process should end with exit")
	}
}

func TestStreamOutputSnapshotOfRunningProcess(t *testing.T) {
	client := setupTestServer(t)
	started := sh(t, client, "echo 'instant-output'; sleep 5")
	time.Sleep(100 * time.Millisecond)

	begin := time.Now()
	stdout, _, exit := collect(t, client, &ateenvv1alpha.StreamProcessOutputRequest{ProcessId: started.GetProcessId()})
	if d := time.Since(begin); d > 2*time.Second {
		t.Fatalf("snapshot took %v, should return immediately", d)
	}
	if stdout != "instant-output\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	if exit != nil {
		t.Fatalf("running process must not produce an exit message")
	}
	_, _ = client.SignalProcess(context.Background(), &ateenvv1alpha.SignalProcessRequest{
		ProcessId: started.GetProcessId(), Signal: ateenvv1alpha.Signal_SIGNAL_KILL,
	})
}

func TestSignalProcess(t *testing.T) {
	client := setupTestServer(t)
	ctx := context.Background()
	started := sh(t, client, "sleep 60")

	proc, err := client.SignalProcess(ctx, &ateenvv1alpha.SignalProcessRequest{
		ProcessId: started.GetProcessId(), Signal: ateenvv1alpha.Signal_SIGNAL_TERM,
	})
	if err != nil {
		t.Fatalf("SignalProcess failed: %v", err)
	}
	if proc.GetProcessId() != started.GetProcessId() {
		t.Fatalf("SignalProcess returned wrong process %v", proc)
	}

	final := wait(t, client, started.GetProcessId())
	if final.GetExitCode() != 143 {
		t.Fatalf("expected exit_code 143 (128+SIGTERM), got %d", final.GetExitCode())
	}

	// Signalling an exited process is a failed precondition.
	_, err = client.SignalProcess(ctx, &ateenvv1alpha.SignalProcessRequest{
		ProcessId: started.GetProcessId(), Signal: ateenvv1alpha.Signal_SIGNAL_KILL,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}

	// Unspecified signal is rejected.
	_, err = client.SignalProcess(ctx, &ateenvv1alpha.SignalProcessRequest{ProcessId: started.GetProcessId()})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

func TestSignalTrappedByProcess(t *testing.T) {
	client := setupTestServer(t)
	started := sh(t, client, `trap 'echo got-usr1; exit 7' USR1; while :; do sleep 0.05; done`)
	time.Sleep(150 * time.Millisecond)

	if _, err := client.SignalProcess(context.Background(), &ateenvv1alpha.SignalProcessRequest{
		ProcessId: started.GetProcessId(), Signal: ateenvv1alpha.Signal_SIGNAL_USR1,
	}); err != nil {
		t.Fatalf("SignalProcess failed: %v", err)
	}

	stdout, _, exit := collect(t, client, &ateenvv1alpha.StreamProcessOutputRequest{ProcessId: started.GetProcessId(), Follow: true})
	if !strings.Contains(stdout, "got-usr1") {
		t.Fatalf("handler did not run, stdout = %q", stdout)
	}
	if exit.GetExitCode() != 7 {
		t.Fatalf("expected exit code 7 from handler, got %d", exit.GetExitCode())
	}
}

func TestStopAndContinue(t *testing.T) {
	client := setupTestServer(t)
	ctx := context.Background()
	started := sh(t, client, "sleep 0.2; echo done")

	signal := func(sig ateenvv1alpha.Signal) {
		if _, err := client.SignalProcess(ctx, &ateenvv1alpha.SignalProcessRequest{ProcessId: started.GetProcessId(), Signal: sig}); err != nil {
			t.Fatalf("signal %v: %v", sig, err)
		}
	}
	signal(ateenvv1alpha.Signal_SIGNAL_STOP)

	// Well past the sleep: a stopped process must not have progressed.
	time.Sleep(500 * time.Millisecond)
	proc, err := client.GetProcess(ctx, &ateenvv1alpha.GetProcessRequest{ProcessId: started.GetProcessId()})
	if err != nil {
		t.Fatalf("GetProcess: %v", err)
	}
	if proc.GetState() != ateenvv1alpha.ProcessState_PROCESS_STATE_RUNNING {
		t.Fatalf("stopped process should still be RUNNING, got %v", proc.GetState())
	}

	signal(ateenvv1alpha.Signal_SIGNAL_CONT)
	if final := wait(t, client, started.GetProcessId()); final.GetExitCode() != 0 {
		t.Fatalf("expected clean exit after CONT, got %d", final.GetExitCode())
	}
}

func TestStdinStreaming(t *testing.T) {
	client := setupTestServer(t)
	ctx := context.Background()
	started := start(t, client, &ateenvv1alpha.StartProcessRequest{Command: []string{"cat"}, Stdin: true})

	in, err := client.WriteProcessInput(ctx)
	if err != nil {
		t.Fatalf("WriteProcessInput: %v", err)
	}
	if err := in.Send(&ateenvv1alpha.WriteProcessInputRequest{ProcessId: started.GetProcessId(), Data: []byte("hello ")}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := in.Send(&ateenvv1alpha.WriteProcessInputRequest{Data: []byte("world\n")}); err != nil {
		t.Fatalf("send: %v", err)
	}
	resp, err := in.CloseAndRecv()
	if err != nil {
		t.Fatalf("CloseAndRecv: %v", err)
	}
	if resp.GetBytesWritten() != int64(len("hello world\n")) {
		t.Fatalf("bytes_written = %d", resp.GetBytesWritten())
	}

	// cat is still running: stdin was not closed. Snapshot shows the echo.
	deadline := time.Now().Add(2 * time.Second)
	for {
		stdout, _, _ := collect(t, client, &ateenvv1alpha.StreamProcessOutputRequest{ProcessId: started.GetProcessId()})
		if stdout == "hello world\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cat did not echo input, stdout = %q", stdout)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if p, _ := client.GetProcess(ctx, &ateenvv1alpha.GetProcessRequest{ProcessId: started.GetProcessId()}); p.GetState() != ateenvv1alpha.ProcessState_PROCESS_STATE_RUNNING {
		t.Fatalf("cat should still be running with stdin open")
	}

	// Second call: more data plus EOF.
	in, err = client.WriteProcessInput(ctx)
	if err != nil {
		t.Fatalf("WriteProcessInput: %v", err)
	}
	_ = in.Send(&ateenvv1alpha.WriteProcessInputRequest{ProcessId: started.GetProcessId(), Data: []byte("bye\n"), Close: true})
	if _, err := in.CloseAndRecv(); err != nil {
		t.Fatalf("CloseAndRecv: %v", err)
	}

	stdout, _, exit := collect(t, client, &ateenvv1alpha.StreamProcessOutputRequest{ProcessId: started.GetProcessId(), Follow: true})
	if stdout != "hello world\nbye\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	if exit.GetExitCode() != 0 {
		t.Fatalf("cat exit = %d", exit.GetExitCode())
	}

	// Writing after close fails.
	in, _ = client.WriteProcessInput(ctx)
	_ = in.Send(&ateenvv1alpha.WriteProcessInputRequest{ProcessId: started.GetProcessId(), Data: []byte("late")})
	if _, err := in.CloseAndRecv(); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition writing after close, got %v", err)
	}
}

func TestStdinRejectedWhenNotRequested(t *testing.T) {
	client := setupTestServer(t)
	ctx := context.Background()
	started := start(t, client, &ateenvv1alpha.StartProcessRequest{Command: []string{"cat"}})

	// Without a stdin pipe cat sees EOF immediately and exits 0.
	if proc := wait(t, client, started.GetProcessId()); proc.GetExitCode() != 0 {
		t.Fatalf("cat without stdin should exit 0, got %d", proc.GetExitCode())
	}

	in, _ := client.WriteProcessInput(ctx)
	_ = in.Send(&ateenvv1alpha.WriteProcessInputRequest{ProcessId: started.GetProcessId(), Data: []byte("x")})
	if _, err := in.CloseAndRecv(); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}

	in, _ = client.WriteProcessInput(ctx)
	_ = in.Send(&ateenvv1alpha.WriteProcessInputRequest{ProcessId: "nope", Data: []byte("x")})
	if _, err := in.CloseAndRecv(); status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}

	in, _ = client.WriteProcessInput(ctx)
	if _, err := in.CloseAndRecv(); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for empty stream, got %v", err)
	}
}

func TestNotFound(t *testing.T) {
	client := setupTestServer(t)
	ctx := context.Background()
	if _, err := client.GetProcess(ctx, &ateenvv1alpha.GetProcessRequest{ProcessId: "nope"}); status.Code(err) != codes.NotFound {
		t.Fatalf("GetProcess: expected NotFound, got %v", err)
	}
	stream, err := client.StreamProcessOutput(ctx, &ateenvv1alpha.StreamProcessOutputRequest{ProcessId: "nope"})
	if err != nil {
		t.Fatalf("StreamProcessOutput: %v", err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.NotFound {
		t.Fatalf("StreamProcessOutput: expected NotFound, got %v", err)
	}
	if _, err := client.GetProcess(ctx, &ateenvv1alpha.GetProcessRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty id: expected InvalidArgument, got %v", err)
	}
	if _, err := client.StartProcess(ctx, &ateenvv1alpha.StartProcessRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty command: expected InvalidArgument, got %v", err)
	}
}

func TestConcurrencyLimiter(t *testing.T) {
	cfg := DefaultConfig(t.TempDir()) // not the shared default: the cleanup removes LogDir
	cfg.MaxConcurrentProcesses = 2
	client := setupTestServerWithConfig(t, cfg)
	ctx := context.Background()

	res1 := sh(t, client, "sleep 5")
	res2 := sh(t, client, "sleep 5")

	_, err := client.StartProcess(ctx, &ateenvv1alpha.StartProcessRequest{Command: []string{"sh", "-c", "echo 'should fail'"}})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("expected ResourceExhausted for job 3, got: %v", err)
	}

	// Kill job 1 and wait for the slot to free up.
	if _, err := client.SignalProcess(ctx, &ateenvv1alpha.SignalProcessRequest{ProcessId: res1.GetProcessId(), Signal: ateenvv1alpha.Signal_SIGNAL_KILL}); err != nil {
		t.Fatalf("failed to kill job 1: %v", err)
	}
	if proc := wait(t, client, res1.GetProcessId()); proc.GetExitCode() != 137 {
		t.Fatalf("expected 137 (128+SIGKILL), got %d", proc.GetExitCode())
	}

	sh(t, client, "echo 'now succeeds'")
	_, _ = client.SignalProcess(ctx, &ateenvv1alpha.SignalProcessRequest{ProcessId: res2.GetProcessId(), Signal: ateenvv1alpha.Signal_SIGNAL_KILL})
}

func TestLogCapping(t *testing.T) {
	cfg := DefaultConfig(t.TempDir()) // not the shared default: the cleanup removes LogDir
	cfg.MaxLogBytes = 256
	client := setupTestServerWithConfig(t, cfg)

	started := sh(t, client, "for i in $(seq 1 500); do echo 'spamming-log-line-0123456789'; done")
	stdout, _, _ := collect(t, client, &ateenvv1alpha.StreamProcessOutputRequest{ProcessId: started.GetProcessId(), Follow: true})

	if !strings.Contains(stdout, "maximum log limit") || !strings.Contains(stdout, "truncated") {
		t.Fatalf("expected truncation warning in capped logs, got:\n%s", stdout)
	}
	if len(stdout) > 1000 {
		t.Fatalf("log size %d exceeded capped expectation", len(stdout))
	}
}

func TestDefaultTimeoutKills(t *testing.T) {
	cfg := DefaultConfig(t.TempDir()) // not the shared default: the cleanup removes LogDir
	cfg.DefaultProcessTimeout = 100 * time.Millisecond
	client := setupTestServerWithConfig(t, cfg)

	started := sh(t, client, "sleep 30")
	proc := wait(t, client, started.GetProcessId())
	if proc.GetExitCode() != 137 {
		t.Fatalf("expected 137 (128+SIGKILL) from watchdog, got %d", proc.GetExitCode())
	}
	if !proc.GetTimedOut() {
		t.Fatalf("expected timed_out=true from watchdog kill")
	}
}

func TestPerProcessTimeout(t *testing.T) {
	client := setupTestServer(t)
	started := start(t, client, &ateenvv1alpha.StartProcessRequest{
		Command: []string{"sleep", "30"},
		Timeout: durationpb.New(100 * time.Millisecond),
	})
	proc := wait(t, client, started.GetProcessId())
	if proc.GetExitCode() != 137 {
		t.Fatalf("expected 137 (128+SIGKILL) from per-process timeout, got %d", proc.GetExitCode())
	}
	if !proc.GetTimedOut() {
		t.Fatalf("expected timed_out=true from per-process timeout")
	}
}

func TestProcessKilledBySignalNotTimedOut(t *testing.T) {
	client := setupTestServer(t)
	started := sh(t, client, "kill -9 $$")
	proc := wait(t, client, started.GetProcessId())
	if proc.GetExitCode() != 137 {
		t.Fatalf("expected 137, got %d", proc.GetExitCode())
	}
	if proc.TimedOut == nil || *proc.TimedOut {
		t.Fatalf("timed_out = %v, want present and false for kill -9", proc.TimedOut)
	}
}

// A caller's SignalProcess(KILL) before the timeout fires is not a timeout,
// though the exit code (137) is the same.
func TestSignalProcessKillNotTimedOut(t *testing.T) {
	client := setupTestServer(t)
	started := start(t, client, &ateenvv1alpha.StartProcessRequest{
		Command: []string{"sleep", "30"},
		Timeout: durationpb.New(time.Minute),
	})
	if _, err := client.SignalProcess(context.Background(), &ateenvv1alpha.SignalProcessRequest{
		ProcessId: started.GetProcessId(), Signal: ateenvv1alpha.Signal_SIGNAL_KILL,
	}); err != nil {
		t.Fatalf("SignalProcess(KILL): %v", err)
	}
	proc := wait(t, client, started.GetProcessId())
	if proc.GetExitCode() != 137 {
		t.Fatalf("expected 137, got %d", proc.GetExitCode())
	}
	if proc.TimedOut == nil || *proc.TimedOut {
		t.Fatalf("timed_out = %v, want present and false for SignalProcess(KILL)", proc.TimedOut)
	}
}

func TestSignalRoundTrip(t *testing.T) {
	for api, sys := range signalTable {
		if got := FromSyscallSignal(sys); got != api {
			t.Errorf("FromSyscallSignal(%v) = %v, want %v", sys, got, api)
		}
	}
	if _, ok := ToSyscallSignal(ateenvv1alpha.Signal_SIGNAL_UNSPECIFIED); ok {
		t.Errorf("UNSPECIFIED must not map to a signal")
	}
}

// requirePTY skips a test that allocates a pseudo-terminal off Linux, where
// openPTY is not implemented.
func requirePTY(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("pty allocation is only supported on linux")
	}
}

func TestTTYControllingTerminalAndMergedOutput(t *testing.T) {
	requirePTY(t)
	client := setupTestServer(t)
	started := start(t, client, &ateenvv1alpha.StartProcessRequest{
		Command: []string{"sh", "-c", "tty; stty size; echo out-msg; echo err-msg >&2"},
		Tty:     &ateenvv1alpha.TTY{Rows: 24, Cols: 80},
	})

	stdout, stderr, exit := collect(t, client, &ateenvv1alpha.StreamProcessOutputRequest{
		ProcessId: started.GetProcessId(),
		Follow:    true,
	})
	if stderr != "" {
		t.Fatalf("expected empty stderr on TTY process, got %q", stderr)
	}
	for _, want := range []string{"/dev/pts/", "24 80", "out-msg", "err-msg"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("expected stdout to contain %q, got:\n%s", want, stdout)
		}
	}
	if exit == nil || exit.GetExitCode() != 0 {
		t.Fatalf("expected exit_code 0, got %v", exit)
	}
}

func TestTTYResizeAndSIGWINCH(t *testing.T) {
	requirePTY(t)
	client := setupTestServer(t)
	ctx := context.Background()
	started := start(t, client, &ateenvv1alpha.StartProcessRequest{
		Command: []string{"sh", "-c", `trap 'echo "winch:$(stty size)"; exit 0' WINCH; echo ready; while :; do sleep 0.05; done`},
		Tty:     &ateenvv1alpha.TTY{Rows: 24, Cols: 80},
	})

	deadline := time.Now().Add(3 * time.Second)
	for {
		stdout, _, _ := collect(t, client, &ateenvv1alpha.StreamProcessOutputRequest{ProcessId: started.GetProcessId()})
		if strings.Contains(stdout, "ready") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for process to arm trap; stdout=%q", stdout)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if _, err := client.ResizeProcess(ctx, &ateenvv1alpha.ResizeProcessRequest{
		ProcessId: started.GetProcessId(),
		Rows:      40,
		Cols:      120,
	}); err != nil {
		t.Fatalf("ResizeProcess failed: %v", err)
	}

	stdout, _, exit := collect(t, client, &ateenvv1alpha.StreamProcessOutputRequest{
		ProcessId: started.GetProcessId(),
		Follow:    true,
	})
	if !strings.Contains(stdout, "winch:40 120") {
		t.Fatalf("expected winch:40 120 in stdout, got:\n%s", stdout)
	}
	if exit == nil || exit.GetExitCode() != 0 {
		t.Fatalf("expected exit_code 0, got %v", exit)
	}

	// Resizing an exited process returns FailedPrecondition.
	if _, err := client.ResizeProcess(ctx, &ateenvv1alpha.ResizeProcessRequest{
		ProcessId: started.GetProcessId(),
		Rows:      50,
		Cols:      100,
	}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition on exited process, got %v", err)
	}

	// Resizing a non-TTY process returns FailedPrecondition.
	noTTY := sh(t, client, "sleep 10")
	t.Cleanup(func() {
		_, _ = client.SignalProcess(ctx, &ateenvv1alpha.SignalProcessRequest{
			ProcessId: noTTY.GetProcessId(),
			Signal:    ateenvv1alpha.Signal_SIGNAL_KILL,
		})
	})
	if _, err := client.ResizeProcess(ctx, &ateenvv1alpha.ResizeProcessRequest{
		ProcessId: noTTY.GetProcessId(),
		Rows:      50,
		Cols:      100,
	}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition on non-TTY process, got %v", err)
	}

	// Resizing an unknown process returns NotFound.
	if _, err := client.ResizeProcess(ctx, &ateenvv1alpha.ResizeProcessRequest{
		ProcessId: "unknown",
		Rows:      50,
		Cols:      100,
	}); status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound on unknown process, got %v", err)
	}

	// Empty process_id returns InvalidArgument.
	if _, err := client.ResizeProcess(ctx, &ateenvv1alpha.ResizeProcessRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument on empty process_id, got %v", err)
	}
}

func TestTTYInputAndCloseEOT(t *testing.T) {
	requirePTY(t)
	client := setupTestServer(t)
	ctx := context.Background()
	started := start(t, client, &ateenvv1alpha.StartProcessRequest{
		Command: []string{"cat"},
		Tty:     &ateenvv1alpha.TTY{Rows: 24, Cols: 80},
	})

	in, err := client.WriteProcessInput(ctx)
	if err != nil {
		t.Fatalf("WriteProcessInput: %v", err)
	}
	if err := in.Send(&ateenvv1alpha.WriteProcessInputRequest{
		ProcessId: started.GetProcessId(),
		Data:      []byte("hello-pty\n"),
		Close:     true,
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := in.CloseAndRecv(); err != nil {
		t.Fatalf("CloseAndRecv: %v", err)
	}

	stdout, _, exit := collect(t, client, &ateenvv1alpha.StreamProcessOutputRequest{
		ProcessId: started.GetProcessId(),
		Follow:    true,
	})
	if !strings.Contains(stdout, "hello-pty") {
		t.Fatalf("expected hello-pty in stdout, got %q", stdout)
	}
	if exit == nil || exit.GetExitCode() != 0 {
		t.Fatalf("expected exit_code 0 after EOT on PTY, got %v", exit)
	}
}

func TestTTYExemptFromLogCap(t *testing.T) {
	requirePTY(t)
	cfg := DefaultConfig(t.TempDir()) // not the shared default: the cleanup removes LogDir
	cfg.MaxLogBytes = 256
	client := setupTestServerWithConfig(t, cfg)

	started := start(t, client, &ateenvv1alpha.StartProcessRequest{
		Command: []string{"sh", "-c", "for i in $(seq 1 100); do echo 'tty-line-0123456789abcdef'; done"},
		Tty:     &ateenvv1alpha.TTY{Rows: 24, Cols: 80},
	})
	stdout, _, exit := collect(t, client, &ateenvv1alpha.StreamProcessOutputRequest{
		ProcessId: started.GetProcessId(),
		Follow:    true,
	})
	if strings.Contains(stdout, "truncated") {
		t.Fatalf("TTY output must not be capped by MaxLogBytes, got:\n%s", stdout)
	}
	if len(stdout) < 2000 {
		t.Fatalf("expected full TTY output (>2000 bytes), got %d bytes", len(stdout))
	}
	if exit == nil || exit.GetExitCode() != 0 {
		t.Fatalf("expected exit_code 0, got %v", exit)
	}
}

func TestTTYWindowSizeBounds(t *testing.T) {
	client := setupTestServer(t)
	ctx := context.Background()
	for _, tty := range []*ateenvv1alpha.TTY{{Rows: math.MaxUint16 + 1, Cols: 80}, {Rows: 24, Cols: math.MaxUint16 + 1}} {
		if _, err := client.StartProcess(ctx, &ateenvv1alpha.StartProcessRequest{Command: []string{"true"}, Tty: tty}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("StartProcess with tty %v: got %v, want InvalidArgument", tty, err)
		}
		if _, err := client.ResizeProcess(ctx, &ateenvv1alpha.ResizeProcessRequest{ProcessId: "p", Rows: tty.GetRows(), Cols: tty.GetCols()}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("ResizeProcess to %dx%d: got %v, want InvalidArgument", tty.GetRows(), tty.GetCols(), err)
		}
	}
}

// A descendant that ignores SIGHUP keeps the slave open after the process
// exits. The process must still be reported as exited, after the short drain
// in reap, and not only when the descendant exits.
func TestTTYExitWhileDescendantHoldsTerminal(t *testing.T) {
	requirePTY(t)
	client := setupTestServer(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	started := time.Now()
	proc := start(t, client, &ateenvv1alpha.StartProcessRequest{
		Command: []string{"sh", "-c", `(trap '' HUP; exec sleep 30) & echo $! > ` + pidFile + `; echo started`},
		Tty:     &ateenvv1alpha.TTY{Rows: 24, Cols: 80},
	})
	t.Cleanup(func() {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	exit := wait(t, client, proc.GetProcessId())
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("exit reported after %v; want soon after the shell exits", elapsed)
	}
	if exit.GetExitCode() != 0 {
		t.Fatalf("exit_code = %d, want 0", exit.GetExitCode())
	}
}

// TestExitReleasesSlotBeforeItIsObservable chains a wait and a start at
// MaxConcurrentProcesses = 1: once a caller has seen the exit message, the
// slot of that process must be free, so the next StartProcess never gets
// ResourceExhausted (#332).
func TestExitReleasesSlotBeforeItIsObservable(t *testing.T) {
	cfg := DefaultConfig(t.TempDir()) // not the shared default: the cleanup removes LogDir
	cfg.MaxConcurrentProcesses = 1
	client := setupTestServerWithConfig(t, cfg)
	ctx := context.Background()

	// 200 chains or 2 s, whichever comes first, and at least 2: a chain
	// costs a few milliseconds on Linux and about 20 ms on macOS. The first
	// chain starts before any process exited, so only the second one starts
	// after an exit it observed. Under CPU contention the old ordering
	// failed on the first start after an exit.
	start := time.Now()
	chains := 0
	for ; chains < 2 || (chains < 200 && time.Since(start) < 2*time.Second); chains++ {
		proc, err := client.StartProcess(ctx, &ateenvv1alpha.StartProcessRequest{Command: []string{"sh", "-c", "true"}})
		if status.Code(err) == codes.ResourceExhausted {
			t.Fatalf("chain %d: the previous process was seen exited but still held its slot: %v", chains, err)
		}
		if err != nil {
			t.Fatalf("chain %d: StartProcess failed: %v", chains, err)
		}
		wait(t, client, proc.GetProcessId())
	}
	if chains < 2 {
		t.Fatalf("%d chains: no start followed an observed exit", chains)
	}
	t.Logf("%d chains in %s", chains, time.Since(start).Round(time.Millisecond))
}

// TestSlotIsFreeWhenExitIsReported starts a second process from OnExit, which
// the reaper calls once the state reads EXITED and before done closes: at
// MaxConcurrentProcesses = 1 that start succeeds only if the slot of the
// exited process is already free (#332).
func TestSlotIsFreeWhenExitIsReported(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	cfg.MaxConcurrentProcesses = 1
	var (
		tracker  *Tracker
		once     sync.Once
		startErr = make(chan error, 1)
	)
	cfg.OnExit = func(time.Duration, string) {
		once.Do(func() {
			_, err := tracker.Start(StartOptions{Command: []string{"sh", "-c", "true"}})
			startErr <- err
		})
	}
	tracker, err := NewTracker(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tracker.Close)

	first, err := tracker.Start(StartOptions{Command: []string{"sh", "-c", "true"}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-first.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("first process did not exit")
	}
	select {
	case err := <-startErr:
		if err != nil {
			t.Fatalf("start from OnExit of an exited process: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("OnExit was not called")
	}
}

// TestConcurrencySlotRace is the RPC-level form of the invariant in #332: a
// process that GetProcess reports as EXITED holds no concurrency slot, so the
// next StartProcess at the cap succeeds. From #340 by sunnylovestiramisu.
func TestConcurrencySlotRace(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	cfg.MaxConcurrentProcesses = 1

	// OnExit runs after reap marks the process EXITED and before it closes
	// done; holding it open keeps reap at that point, so the second
	// StartProcess below runs while the exit is already observable.
	waitOnExit := make(chan struct{})
	var release sync.Once
	releaseReap := func() { release.Do(func() { close(waitOnExit) }) }
	t.Cleanup(releaseReap)
	cfg.OnExit = func(d time.Duration, exitClass string) {
		<-waitOnExit
	}

	client := setupTestServerWithConfig(t, cfg)
	ctx := context.Background()

	proc, err := client.StartProcess(ctx, &ateenvv1alpha.StartProcessRequest{Command: []string{"true"}})
	if err != nil {
		t.Fatalf("StartProcess failed: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		p, err := client.GetProcess(ctx, &ateenvv1alpha.GetProcessRequest{ProcessId: proc.GetProcessId()})
		if err != nil {
			t.Fatalf("GetProcess failed: %v", err)
		}
		if p.GetState() == ateenvv1alpha.ProcessState_PROCESS_STATE_EXITED {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("process not EXITED after 10 s: state %v", p.GetState())
		}
		time.Sleep(10 * time.Millisecond)
	}

	_, err = client.StartProcess(ctx, &ateenvv1alpha.StartProcessRequest{Command: []string{"true"}})
	releaseReap()

	if err != nil {
		if status.Code(err) == codes.ResourceExhausted {
			t.Fatalf("StartProcess after the previous process was reported EXITED: %v", err)
		}
		t.Fatalf("StartProcess failed with unexpected error: %v", err)
	}
}

// TestSlotIsFreeWhenSignalReportsExit signals a process until Signal reports
// ErrExited and then starts the next one: at MaxConcurrentProcesses = 1 that
// start succeeds only if the exited process no longer holds its slot (WF-14,
// #25). On a fast disk one chain can miss the window between cmd.Wait and the
// slot release in reap, so the test runs 20 chains.
func TestSlotIsFreeWhenSignalReportsExit(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	cfg.MaxConcurrentProcesses = 1
	tracker, err := NewTracker(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tracker.Close)

	proc, err := tracker.Start(StartOptions{Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	for chain := 0; chain < 20; chain++ {
		deadline := time.Now().Add(10 * time.Second)
		for {
			err := tracker.Signal(context.Background(), proc.ProcessID, 0)
			if errors.Is(err, ErrExited) {
				break
			}
			if err != nil {
				t.Fatalf("chain %d: Signal failed: %v", chain, err)
			}
			if time.Now().After(deadline) {
				t.Fatalf("chain %d: Signal did not report the exit after 10 s", chain)
			}
		}
		proc, err = tracker.Start(StartOptions{Command: []string{"true"}})
		if err != nil {
			t.Fatalf("chain %d: Signal reported the exit but the process still held its slot: %v", chain, err)
		}
	}
}

// TestSlotIsFreeWhenWriteInputReportsExit writes to the stdin of a process
// until WriteInput reports ErrExited (from EPIPE, or from the stdin pipe that
// cmd.Wait closed) and then starts the next one: at MaxConcurrentProcesses = 1
// that start succeeds only if the exited process no longer holds its slot
// (WF-14, #25). 20 chains, as in TestSlotIsFreeWhenSignalReportsExit.
func TestSlotIsFreeWhenWriteInputReportsExit(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	cfg.MaxConcurrentProcesses = 1
	tracker, err := NewTracker(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tracker.Close)

	opts := StartOptions{Command: []string{"true"}, Stdin: true}
	proc, err := tracker.Start(opts)
	if err != nil {
		t.Fatal(err)
	}
	for chain := 0; chain < 20; chain++ {
		deadline := time.Now().Add(10 * time.Second)
		for {
			_, err := tracker.WriteInput(context.Background(), proc.ProcessID, []byte("x"))
			if errors.Is(err, ErrExited) {
				break
			}
			if err != nil {
				t.Fatalf("chain %d: WriteInput failed: %v", chain, err)
			}
			if time.Now().After(deadline) {
				t.Fatalf("chain %d: WriteInput did not report the exit after 10 s", chain)
			}
		}
		proc, err = tracker.Start(opts)
		if err != nil {
			t.Fatalf("chain %d: WriteInput reported the exit but the process still held its slot: %v", chain, err)
		}
	}
}

// TestWriteInputToProcessThatClosedStdinIsNotExited writes to a process that
// closed its own stdin and keeps running: WriteInput gets EPIPE but must not
// report ErrExited, because the process still holds its slot (WF-14, #25).
func TestWriteInputToProcessThatClosedStdinIsNotExited(t *testing.T) {
	tracker, err := NewTracker(DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tracker.Close)

	proc, err := tracker.Start(StartOptions{Command: []string{"sh", "-c", "exec 0<&-; sleep 30"}, Stdin: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-proc.Pid, syscall.SIGKILL) })

	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err = tracker.WriteInput(context.Background(), proc.ProcessID, []byte("x"))
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("WriteInput did not fail after 10 s")
		}
	}
	if errors.Is(err, ErrExited) {
		t.Fatalf("WriteInput reported the exit of a running process: %v", err)
	}
	if !errors.Is(err, ErrStdinClosed) {
		t.Fatalf("WriteInput failed: %v, want ErrStdinClosed", err)
	}
	if proc.Exited() {
		t.Fatal("the process exited, but the test needs it running")
	}

	begin := time.Now()
	if _, err := tracker.WriteInput(context.Background(), proc.ProcessID, []byte("x")); !errors.Is(err, ErrStdinClosed) {
		t.Fatalf("second WriteInput: %v, want ErrStdinClosed", err)
	}
	if took := time.Since(begin); took >= reapWaitTimeout/2 {
		t.Fatalf("second WriteInput took %v, but it should fail without waiting for reap", took)
	}
}

// TestWriteInputWaitEndsWithCallerContext writes to a running process that
// closed its stdin with a cancelled context: the wait for reap ends at once
// with the context's error, not after reapWaitTimeout, and never as an exit
// (WF-14).
func TestWriteInputWaitEndsWithCallerContext(t *testing.T) {
	tracker, err := NewTracker(DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tracker.Close)

	proc, err := tracker.Start(StartOptions{Command: []string{"sh", "-c", "exec 0<&-; sleep 30"}, Stdin: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-proc.Pid, syscall.SIGKILL) })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	deadline := time.Now().Add(10 * time.Second)
	var took time.Duration
	for {
		begin := time.Now()
		_, err = tracker.WriteInput(ctx, proc.ProcessID, []byte("x"))
		took = time.Since(begin)
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("WriteInput did not fail after 10 s")
		}
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteInput: %v, want context.Canceled", err)
	}
	if took >= reapWaitTimeout/2 {
		t.Fatalf("WriteInput took %v with a cancelled context", took)
	}
}

// TestWaitReaped checks the three ways the wait for reap ends: done closed
// (also when the context has ended too), the caller's context ending, and
// reapWaitTimeout. Only the first lets a caller report ErrExited (WF-14).
func TestWaitReaped(t *testing.T) {
	done := make(chan struct{})
	close(done)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (&ProcessState{done: done}).waitReaped(ctx); err != nil {
		t.Errorf("done closed: %v, want nil", err)
	}
	if err := (&ProcessState{done: make(chan struct{})}).waitReaped(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context: %v, want context.Canceled", err)
	}
	begin := time.Now()
	if err := (&ProcessState{done: make(chan struct{})}).waitReaped(context.Background()); !errors.Is(err, errReapWaitTimeout) {
		t.Errorf("no exit: %v, want errReapWaitTimeout", err)
	}
	if took := time.Since(begin); took < reapWaitTimeout {
		t.Errorf("no exit: returned after %v, want at least %v", took, reapWaitTimeout)
	}
}

// TestRPCErrorMapsContextErrors checks the status code of the caller's own
// context ending during the wait for reap (WF-14): Canceled or
// DeadlineExceeded, without "has exited", which clients read as an exit.
func TestRPCErrorMapsContextErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want codes.Code
	}{
		{context.Canceled, codes.Canceled},
		{context.DeadlineExceeded, codes.DeadlineExceeded},
	} {
		st := status.Convert(rpcError(tc.err, "proc-x"))
		if st.Code() != tc.want {
			t.Errorf("rpcError(%v) = %v, want %v", tc.err, st.Code(), tc.want)
		}
		if strings.Contains(st.Message(), "has exited") {
			t.Errorf("rpcError(%v) message %q says the process has exited", tc.err, st.Message())
		}
	}
}

// TestSignalReachesProcessThatLeftItsGroup signals a process that moved itself
// into another process group and keeps running: kill(-pid) gets ESRCH, but the
// signal must still reach the process instead of Signal reporting ErrExited
// (WF-14, #25). The process joins the group of its own child, whose output
// goes to /dev/null so that it cannot hold up reap, and the cleanup kills that
// group.
func TestSignalReachesProcessThatLeftItsGroup(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	tracker, err := NewTracker(DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tracker.Close)

	proc, err := tracker.Start(StartOptions{Command: []string{python, "-c", `
import os, time
r, w = os.pipe()
child = os.fork()
if child == 0:
    null = os.open(os.devnull, os.O_RDWR)
    for fd in (0, 1, 2):
        os.dup2(null, fd)
    os.setpgid(0, 0)
    os.write(w, b"x")
    time.sleep(30)
    os._exit(0)
os.read(r, 1)
os.setpgid(0, child)
time.sleep(30)
`}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proc.proc.Kill() })

	deadline := time.Now().Add(10 * time.Second)
	for {
		pgid, err := syscall.Getpgid(proc.Pid)
		if err == nil && pgid != proc.Pid {
			t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the process did not leave its process group")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := tracker.Signal(context.Background(), proc.ProcessID, syscall.SIGKILL); err != nil {
		t.Fatalf("Signal to a running process that left its group: %v, want nil", err)
	}
	select {
	case <-proc.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the signal did not reach the process")
	}
	proc.mu.RLock()
	sig := proc.Signal
	proc.mu.RUnlock()
	if sig != syscall.SIGKILL {
		t.Fatalf("the process ended with signal %v, want SIGKILL", sig)
	}
}

// TestSignalAndWriteInputWhileDescendantHoldsOutput covers a process that is
// gone while reap has not recorded its exit: its child moved to its own
// session and still holds the output pipes, so cmd.Wait in reap does not
// return. Neither call may report ErrExited. Signal returns the caller's
// context error at once, or nil after reapWaitTimeout, since nothing is left
// to signal. WriteInput returns ErrStdinClosed. Both match GetProcess, which
// still reports the process as running (WF-14, #25).
func TestSignalAndWriteInputWhileDescendantHoldsOutput(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	tracker, err := NewTracker(DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tracker.Close)

	proc, err := tracker.Start(StartOptions{Stdin: true, Command: []string{python, "-c", `
import os, time
r, w = os.pipe()
child = os.fork()
if child == 0:
    os.dup2(os.open(os.devnull, os.O_RDONLY), 0)
    os.setsid()
    os.write(w, b"x")
    time.sleep(30)
    os._exit(0)
os.read(r, 1)
print(child, flush=True)
`}})
	if err != nil {
		t.Fatal(err)
	}

	// The child's PID, printed by the process, is its session's process group.
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, _, err := ReadSpool(proc.StdoutPath, 0)
		if err != nil {
			t.Fatal(err)
		}
		if child, err := strconv.Atoi(strings.TrimSpace(string(out))); err == nil && strings.HasSuffix(string(out), "\n") {
			t.Cleanup(func() { _ = syscall.Kill(-child, syscall.SIGKILL) })
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the process did not print its child's PID, stdout %q", out)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Wait until cmd.Wait has reaped the process itself.
	for !errors.Is(proc.proc.Signal(syscall.Signal(0)), os.ErrProcessDone) {
		if time.Now().After(deadline) {
			t.Fatal("the process did not exit")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Without a pidfd, os.Process reports the process done just before wait4
	// reaps it, while kill(-pid) can still reach the zombie and return nil.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var took time.Duration
	for {
		begin := time.Now()
		err = tracker.Signal(ctx, proc.ProcessID, 0)
		took = time.Since(begin)
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Signal with a cancelled context kept returning nil")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Signal with a cancelled context: %v, want context.Canceled", err)
	}
	if took >= reapWaitTimeout/2 {
		t.Fatalf("Signal with a cancelled context took %v", took)
	}
	if err := tracker.Signal(context.Background(), proc.ProcessID, 0); err != nil {
		t.Fatalf("Signal: %v, want nil", err)
	}
	if _, err := tracker.WriteInput(context.Background(), proc.ProcessID, []byte("x")); !errors.Is(err, ErrStdinClosed) {
		t.Fatalf("WriteInput: %v, want ErrStdinClosed", err)
	}
	if proc.Exited() {
		t.Fatal("reap recorded the exit, but the test needs the child to hold the output")
	}
}

// A service that a command starts in the background with its output
// redirected outlives the command: the command's exit is reported at once,
// and the timeout of the command, which kills the process group, does not
// fire after the command ended. NeMo Gym's preserve_background_services
// relies on this (docs/plans/nemo-gym, NG-19).
func TestBackgroundServiceOutlivesItsCommand(t *testing.T) {
	client := setupTestServer(t)
	started := time.Now()
	proc := start(t, client, &ateenvv1alpha.StartProcessRequest{
		Command: []string{"sh", "-c", `sleep 30 > /dev/null 2>&1 < /dev/null & echo $!`},
		Timeout: durationpb.New(2 * time.Second),
	})
	stdout, _, exit := collect(t, client, &ateenvv1alpha.StreamProcessOutputRequest{ProcessId: proc.GetProcessId(), Follow: true})
	pid, err := strconv.Atoi(strings.TrimSpace(stdout))
	if err != nil {
		t.Fatalf("service pid from stdout %q: %v", stdout, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
		t.Fatalf("exit reported after %v; want soon after the shell exits, before the timeout", elapsed)
	}
	if exit.GetExitCode() != 0 || exit.GetTimedOut() {
		t.Fatalf("exit_code = %d, timed_out = %v; want 0, false", exit.GetExitCode(), exit.GetTimedOut())
	}
	// Past the command's timeout the service is still there.
	time.Sleep(2500*time.Millisecond - time.Since(started))
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("service %d is gone after its command returned: %v", pid, err)
	}
}
