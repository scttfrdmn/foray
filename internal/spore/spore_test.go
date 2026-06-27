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
	"testing"
	"time"
)

// stubRunner records the args it was called with and returns canned output, so
// the real adapters' arg-construction and JSON-parsing run with no AWS and no
// installed binary. err, when set, is returned instead of out.
type stubRunner struct {
	out      []byte
	err      error
	gotName  string
	gotArgs  []string
	gotCalls int
}

func (s *stubRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	s.gotName = name
	s.gotArgs = args
	s.gotCalls++
	if s.err != nil {
		return nil, s.err
	}
	return s.out, nil
}

// hasArg reports whether the recorded args contain v (order-independent), so a
// test asserts a flag was passed without pinning its exact position.
func (s *stubRunner) hasArg(v string) bool {
	for _, a := range s.gotArgs {
		if a == v {
			return true
		}
	}
	return false
}

// hasFlagValue reports whether flag is immediately followed by value.
func (s *stubRunner) hasFlagValue(flag, value string) bool {
	for i, a := range s.gotArgs {
		if a == flag && i+1 < len(s.gotArgs) && s.gotArgs[i+1] == value {
			return true
		}
	}
	return false
}

func TestExecRunnerToolNotFound(t *testing.T) {
	// A binary that cannot exist on PATH must yield ErrToolNotFound, not a raw
	// exec error, so callers can hint "install it or use FORAY_FAKE=1".
	r := NewExecRunner()
	_, err := r.Run(context.Background(), "no-such-spore-tool-xyzzy", "--help")
	if !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("want ErrToolNotFound, got %v", err)
	}
}

func TestDurStr(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{8 * time.Hour, "8h0m0s"},
		{5 * time.Minute, "5m0s"},
	}
	for _, tt := range tests {
		if got := durStr(tt.d); got != tt.want {
			t.Errorf("durStr(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestJoinCSV(t *testing.T) {
	if got := joinCSV([]string{"us-east-1", "us-west-2"}); got != "us-east-1,us-west-2" {
		t.Errorf("joinCSV = %q", got)
	}
	if got := joinCSV(nil); got != "" {
		t.Errorf("joinCSV(nil) = %q, want empty", got)
	}
}
