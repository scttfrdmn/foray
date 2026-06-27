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
	"time"
)

// NewFake builds an Exporter with no AWS — returns a stub presigned link. For
// FORAY_FAKE=1 and the page's rehearsal mode.
func NewFake() *Exporter {
	return &Exporter{Policy: fakePolicy{}, Presigner: fakePresigner{}}
}

type fakePolicy struct{}

func (fakePolicy) PermitExport(_ context.Context, _ string) (bool, string) { return true, "" }

type fakePresigner struct{}

func (fakePresigner) Presign(_ context.Context, sessionID string, kind Kind, ttl time.Duration) (Link, error) {
	return Link{
		URL:       "https://your-bucket.s3.amazonaws.com/sessions/" + sessionID + "/" + string(kind) + ".zip?X-Amz-Signature=...(presigned, fake)",
		Kind:      kind,
		Bytes:     0,
		ExpiresAt: time.Now().Add(ttl),
	}, nil
}
