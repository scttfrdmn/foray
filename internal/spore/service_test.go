// Copyright 2026 Scott Friedman
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

package spore

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// stubProc is a canned long-lived process: it replays scripted stdout and records
// whether it was stopped.
type stubProc struct {
	out     io.Reader
	stopped bool
}

func (p *stubProc) Stdout() io.Reader { return p.out }
func (p *stubProc) Stop() error       { p.stopped = true; return nil }

// stubStarter records the argv it was asked to start and hands back stubProc.
type stubStarter struct {
	stdout  string
	err     error
	gotName string
	gotArgs []string
	proc    *stubProc
	// block, when set, makes stdout never deliver a line so the boot timeout wins.
	block bool
}

func (s *stubStarter) Start(_ context.Context, name string, args ...string) (Proc, error) {
	s.gotName, s.gotArgs = name, args
	if s.err != nil {
		return nil, s.err
	}
	var r io.Reader = strings.NewReader(s.stdout)
	if s.block {
		r = blockingReader{}
	}
	s.proc = &stubProc{out: r}
	return s.proc, nil
}

// blockingReader never returns, standing in for a service that never announces.
type blockingReader struct{}

func (blockingReader) Read([]byte) (int, error) {
	ch := make(chan struct{})
	<-ch // blocks forever; the test's boot timeout is what ends the wait
	return 0, io.EOF
}

const readyLine = `{"instance_id":"i-0abc","region":"us-west-2","local_url":"http://127.0.0.1:54321/?token=abc123","local_addr":"127.0.0.1:54321","remote_addr":"127.0.0.1:41000","ttl":"2h"}`

func TestServeReadsReadinessLine(t *testing.T) {
	st := &stubStarter{stdout: readyLine + "\n"}
	svc, err := NewServer(st).Serve(context.Background(), ServeSpec{
		InstanceID: "i-0abc",
		Command:    []string{"env", "FORAY_MODEL_URI=gpt2", "python3", "-m", "worker.serve"},
	})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if svc.URL != "http://127.0.0.1:54321/?token=abc123" {
		t.Errorf("URL = %q", svc.URL)
	}
	if svc.Addr != "127.0.0.1:54321" {
		t.Errorf("Addr = %q", svc.Addr)
	}
	if svc.InstanceID != "i-0abc" {
		t.Errorf("InstanceID = %q", svc.InstanceID)
	}
	if err := svc.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}
	if !st.proc.stopped {
		t.Error("Stop() did not stop the child process — the tunnel would leak")
	}
}

// The service command carries "-m", so spawn's flags must be terminated with "--"
// before it. spawn does not disable flag interspersion, so without the terminator
// cobra would reject "-m" as an unknown shorthand flag and the tunnel would never
// open. This is the kind of breakage that only shows up against the real binary,
// so it is pinned here.
func TestServeTerminatesFlagsBeforeCommand(t *testing.T) {
	st := &stubStarter{stdout: readyLine + "\n"}
	cmd := []string{"env", "FORAY_SESSION_ID=i-0abc", "python3", "-m", "worker.serve"}
	if _, err := NewServer(st).Serve(context.Background(), ServeSpec{
		InstanceID: "i-0abc",
		Command:    cmd,
	}); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	if st.gotName != "spawn" {
		t.Errorf("started %q, want spawn", st.gotName)
	}
	if st.gotArgs[0] != "service" {
		t.Errorf("args[0] = %q, want service", st.gotArgs[0])
	}

	dash := -1
	for i, a := range st.gotArgs {
		if a == "--" {
			dash = i
			break
		}
	}
	if dash < 0 {
		t.Fatalf("no -- terminator in args: %v", st.gotArgs)
	}
	// Everything spawn needs comes before the terminator...
	before := strings.Join(st.gotArgs[:dash], " ")
	for _, want := range []string{"--host i-0abc", "-o json", "--boot-timeout"} {
		if !strings.Contains(before, want) {
			t.Errorf("missing %q before --: %v", want, st.gotArgs[:dash])
		}
	}
	// ...and the command, verbatim, comes after it.
	got := strings.Join(st.gotArgs[dash+1:], " ")
	if got != strings.Join(cmd, " ") {
		t.Errorf("command after -- = %q, want %q", got, strings.Join(cmd, " "))
	}
}

// Log chatter before the result must not derail the parse: spawn reserves stdout
// for the result, but tolerating a stray line costs nothing and means a future
// banner does not break foray.
func TestServeSkipsNonResultLines(t *testing.T) {
	st := &stubStarter{stdout: "starting up\nnot json at all\n" + readyLine + "\n"}
	svc, err := NewServer(st).Serve(context.Background(), ServeSpec{
		InstanceID: "i-0abc",
		Command:    []string{"python3", "-m", "worker.serve"},
	})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if svc.Addr != "127.0.0.1:54321" {
		t.Errorf("Addr = %q", svc.Addr)
	}
}

// spawn exiting without a result is a failed boot, not a hang.
func TestServeNoReadinessLine(t *testing.T) {
	st := &stubStarter{stdout: "the service never became ready\n"}
	_, err := NewServer(st).Serve(context.Background(), ServeSpec{
		InstanceID: "i-0abc",
		Command:    []string{"python3", "-m", "worker.serve"},
	})
	if !errors.Is(err, ErrServiceNotReady) {
		t.Fatalf("err = %v, want ErrServiceNotReady", err)
	}
	if !st.proc.stopped {
		t.Error("a failed boot must still stop the child process")
	}
}

// A service that never announces is bounded by BootTimeout rather than hanging
// the CLI forever.
func TestServeBootTimeout(t *testing.T) {
	st := &stubStarter{block: true}
	_, err := NewServer(st).Serve(context.Background(), ServeSpec{
		InstanceID:  "i-0abc",
		Command:     []string{"python3", "-m", "worker.serve"},
		BootTimeout: 50 * time.Millisecond,
	})
	if !errors.Is(err, ErrServiceNotReady) {
		t.Fatalf("err = %v, want ErrServiceNotReady", err)
	}
}

func TestServeRequiresInstanceAndCommand(t *testing.T) {
	tests := []struct {
		name string
		spec ServeSpec
	}{
		{"no instance", ServeSpec{Command: []string{"x"}}},
		{"no command", ServeSpec{InstanceID: "i-0abc"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewServer(&stubStarter{}).Serve(context.Background(), tt.spec); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

// The fake hands back the same shape the real tunnel does — a loopback URL with a
// token — so the offline loop exercises the token path instead of hiding it.
func TestFakeServerShape(t *testing.T) {
	f := NewFake()
	svc, err := f.Server.Serve(context.Background(), ServeSpec{
		InstanceID: "i-fake000001",
		Command:    []string{"python3", "-m", "worker.serve"},
	})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if !strings.HasPrefix(svc.URL, "http://127.0.0.1:") {
		t.Errorf("URL = %q, want a loopback address", svc.URL)
	}
	if !strings.Contains(svc.URL, "token=") {
		t.Errorf("URL = %q, want a token in the query", svc.URL)
	}

	fs := f.Server.(*fakeServer)
	if fs.OpenTunnels() != 1 {
		t.Errorf("open tunnels = %d, want 1", fs.OpenTunnels())
	}
	if err := svc.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if fs.OpenTunnels() != 0 {
		t.Errorf("open tunnels after Stop = %d, want 0", fs.OpenTunnels())
	}
}
