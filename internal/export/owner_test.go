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

package export

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// ownerStub records the listing it was asked for and replays a canned answer.
type ownerStub struct {
	objects   []string
	err       error
	gotPrefix string
	gotBucket string
	gotMax    int32
	calls     int
}

func (s *ownerStub) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	s.calls++
	s.gotBucket = aws.ToString(in.Bucket)
	s.gotPrefix = aws.ToString(in.Prefix)
	s.gotMax = aws.ToInt32(in.MaxKeys)
	if s.err != nil {
		return nil, s.err
	}
	out := &s3.ListObjectsV2Output{}
	for _, k := range s.objects {
		out.Contents = append(out.Contents, s3types.Object{Key: aws.String(k), Size: aws.Int64(1)})
	}
	return out, nil
}

func newOwner(stub *ownerStub) *SessionOwner {
	return &SessionOwner{api: stub, bucket: "foray-data", subject: "alice"}
}

// Ownership follows the saves, not the instance. A session whose objects exist is
// owned — which is the whole point of issue #80: a terminated session is exactly
// the one a user exports from.
func TestOwnerFromObjectPresence(t *testing.T) {
	tests := []struct {
		name      string
		objects   []string
		err       error
		sessionID string
		wantOwner string
		wantOK    bool
	}{
		{
			name:      "saves present → owned",
			objects:   []string{"sessions/i-0abc/activations/layer0.safetensors"},
			sessionID: "i-0abc",
			wantOwner: "alice",
			wantOK:    true,
		},
		{
			name:      "no saves → not owned",
			objects:   nil,
			sessionID: "i-0abc",
			wantOK:    false,
		},
		{
			// Fail closed. An authorization input that cannot be read must not
			// resolve to "permitted".
			name:      "listing error → not owned",
			err:       errors.New("access denied"),
			sessionID: "i-0abc",
			wantOK:    false,
		},
		{
			name:      "empty session id → not owned",
			objects:   []string{"sessions/i-0abc/x"},
			sessionID: "",
			wantOK:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := newOwner(&ownerStub{objects: tt.objects, err: tt.err})
			gotOwner, gotOK := o.Owner(context.Background(), tt.sessionID)
			if gotOK != tt.wantOK {
				t.Errorf("ok = %v, want %v", gotOK, tt.wantOK)
			}
			if gotOwner != tt.wantOwner {
				t.Errorf("owner = %q, want %q", gotOwner, tt.wantOwner)
			}
		})
	}
}

// The listing must be scoped to the one session's prefix and must not enumerate
// it: presence is the question, and a session can hold thousands of shards.
func TestOwnerListsOnlyTheSessionPrefixCheaply(t *testing.T) {
	stub := &ownerStub{objects: []string{"sessions/i-0abc/activations/x"}}
	if _, ok := newOwner(stub).Owner(context.Background(), "i-0abc"); !ok {
		t.Fatal("want owned")
	}
	if stub.gotBucket != "foray-data" {
		t.Errorf("bucket = %q", stub.gotBucket)
	}
	if want := "sessions/i-0abc/"; stub.gotPrefix != want {
		t.Errorf("prefix = %q, want %q — never a bucket-wide listing", stub.gotPrefix, want)
	}
	if stub.gotMax != 1 {
		t.Errorf("MaxKeys = %d, want 1", stub.gotMax)
	}
}

// An empty session id must not reach S3 at all.
func TestOwnerEmptySessionMakesNoCall(t *testing.T) {
	stub := &ownerStub{}
	if _, err := newOwner(stub).Exists(context.Background(), ""); err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if stub.calls != 0 {
		t.Errorf("made %d S3 calls for an empty session id, want 0", stub.calls)
	}
}

// Exists surfaces the listing error, unlike Owner which folds it into "not
// owned" — that split is what lets a caller say "nothing saved here" instead of
// the misleading "not yours".
func TestExistsSurfacesError(t *testing.T) {
	o := newOwner(&ownerStub{err: errors.New("access denied")})
	if _, err := o.Exists(context.Background(), "i-0abc"); err == nil {
		t.Fatal("want the listing error surfaced")
	}
}

// OwnerFunc is the adapter brain.NewCedarExportPolicy consumes.
func TestOwnerFuncBindsContext(t *testing.T) {
	o := newOwner(&ownerStub{objects: []string{"sessions/i-0abc/x"}})
	fn := o.OwnerFunc(context.Background())
	owner, ok := fn("i-0abc")
	if !ok || owner != "alice" {
		t.Errorf("OwnerFunc = (%q, %v), want (alice, true)", owner, ok)
	}
}
