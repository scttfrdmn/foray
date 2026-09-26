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

package deploy

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"net/url"
	"sort"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// In-memory stand-ins for the AWS APIs this package calls — the FORAY_FAKE=1
// rehearsal path and the test doubles, in one place (the house pattern, cf.
// internal/spore/fake.go).
//
// They model just enough real behavior for idempotency to be a genuine question:
// a table or bucket that exists stays existing, a missing one raises the same
// not-found shape the SDK does, S3 refuses to delete a non-empty bucket, and
// DeleteObjects can report per-key failures in a 200 body. The mutating calls are
// counted so a dry run can be proven inert.
//
// The failure-injection fields (failCreate, failDelete, forbidden,
// deleteObjectErrors) exist for tests and the rehearsal; nothing in the real path
// sets them.

// --- DynamoDB ---------------------------------------------------------------

type fakeTable struct {
	billingMode ddbtypes.BillingMode
	status      ddbtypes.TableStatus
	ttlEnabled  bool
	tags        map[string]string
}

type fakeDynamo struct {
	tables  map[string]*fakeTable
	creates int
	// ttlUpdates counts UpdateTimeToLive calls, so "enable TTL only when it is not
	// already on" is observable (the real API errors on a redundant enable).
	ttlUpdates int
}

func newFakeDynamo() *fakeDynamo {
	return &fakeDynamo{tables: map[string]*fakeTable{}}
}

func (f *fakeDynamo) DescribeTable(_ context.Context, in *dynamodb.DescribeTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	t, ok := f.tables[aws.ToString(in.TableName)]
	if !ok {
		return nil, &ddbtypes.ResourceNotFoundException{Message: aws.String("no such table")}
	}
	return &dynamodb.DescribeTableOutput{Table: &ddbtypes.TableDescription{
		TableName:          in.TableName,
		TableStatus:        t.status,
		BillingModeSummary: &ddbtypes.BillingModeSummary{BillingMode: t.billingMode},
	}}, nil
}

func (f *fakeDynamo) CreateTable(_ context.Context, in *dynamodb.CreateTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error) {
	f.creates++
	tags := map[string]string{}
	for _, tg := range in.Tags {
		tags[aws.ToString(tg.Key)] = aws.ToString(tg.Value)
	}
	f.tables[aws.ToString(in.TableName)] = &fakeTable{
		billingMode: in.BillingMode,
		// ACTIVE immediately: the wait loop is exercised separately with a
		// CREATING-then-ACTIVE table.
		status: ddbtypes.TableStatusActive,
		tags:   tags,
	}
	return &dynamodb.CreateTableOutput{}, nil
}

func (f *fakeDynamo) DeleteTable(_ context.Context, in *dynamodb.DeleteTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteTableOutput, error) {
	delete(f.tables, aws.ToString(in.TableName))
	return &dynamodb.DeleteTableOutput{}, nil
}

func (f *fakeDynamo) UpdateTimeToLive(_ context.Context, in *dynamodb.UpdateTimeToLiveInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateTimeToLiveOutput, error) {
	f.ttlUpdates++
	if t, ok := f.tables[aws.ToString(in.TableName)]; ok {
		t.ttlEnabled = true
	}
	return &dynamodb.UpdateTimeToLiveOutput{}, nil
}

func (f *fakeDynamo) DescribeTimeToLive(_ context.Context, in *dynamodb.DescribeTimeToLiveInput, _ ...func(*dynamodb.Options)) (*dynamodb.DescribeTimeToLiveOutput, error) {
	status := ddbtypes.TimeToLiveStatusDisabled
	if t, ok := f.tables[aws.ToString(in.TableName)]; ok && t.ttlEnabled {
		status = ddbtypes.TimeToLiveStatusEnabled
	}
	return &dynamodb.DescribeTimeToLiveOutput{
		TimeToLiveDescription: &ddbtypes.TimeToLiveDescription{TimeToLiveStatus: status},
	}, nil
}

// --- S3 ---------------------------------------------------------------------

type fakeBucket struct {
	region      string
	objects     map[string]struct{}
	publicBlock bool
	encrypted   bool
	lifecycle   bool
	// lifecycleInput is retained so a test can assert the rule filters on the
	// export-bundle TAG rather than a prefix — a prefix rule would delete the
	// user's saved activations.
	lifecycleInput *s3types.BucketLifecycleConfiguration
	tags           map[string]string
}

type fakeS3 struct {
	buckets map[string]*fakeBucket
	creates int
	// failCreate/failDelete inject per-bucket failures.
	failCreate map[string]error
	failDelete map[string]error
	// forbidden marks a name as owned by another account (HeadBucket → 403).
	forbidden map[string]bool
	// deleteObjectErrors makes DeleteObjects report per-key failures in the body,
	// which the real API does with a 200 status.
	deleteObjectErrors map[string]bool
}

func newFakeS3() *fakeS3 {
	return &fakeS3{
		buckets:            map[string]*fakeBucket{},
		failCreate:         map[string]error{},
		failDelete:         map[string]error{},
		forbidden:          map[string]bool{},
		deleteObjectErrors: map[string]bool{},
	}
}

// forbiddenErr mimics the SDK's 403 shape closely enough for isS3Forbidden.
type forbiddenErr struct{}

func (forbiddenErr) Error() string       { return "api error Forbidden: 403" }
func (forbiddenErr) ErrorCode() string   { return "Forbidden" }
func (forbiddenErr) HTTPStatusCode() int { return 403 }

func (f *fakeS3) HeadBucket(_ context.Context, in *s3.HeadBucketInput, _ ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	name := aws.ToString(in.Bucket)
	if f.forbidden[name] {
		return nil, forbiddenErr{}
	}
	if _, ok := f.buckets[name]; !ok {
		return nil, &s3types.NotFound{Message: aws.String("no such bucket")}
	}
	return &s3.HeadBucketOutput{}, nil
}

func (f *fakeS3) CreateBucket(_ context.Context, in *s3.CreateBucketInput, _ ...func(*s3.Options)) (*s3.CreateBucketOutput, error) {
	name := aws.ToString(in.Bucket)
	if err, ok := f.failCreate[name]; ok {
		return nil, err
	}
	f.creates++
	region := "us-east-1"
	if in.CreateBucketConfiguration != nil {
		region = string(in.CreateBucketConfiguration.LocationConstraint)
	}
	f.buckets[name] = &fakeBucket{region: region, objects: map[string]struct{}{}, tags: map[string]string{}}
	return &s3.CreateBucketOutput{}, nil
}

func (f *fakeS3) DeleteBucket(_ context.Context, in *s3.DeleteBucketInput, _ ...func(*s3.Options)) (*s3.DeleteBucketOutput, error) {
	name := aws.ToString(in.Bucket)
	if err, ok := f.failDelete[name]; ok {
		return nil, err
	}
	// Model S3's refusal to delete a non-empty bucket — the reason Teardown empties
	// first, and the reason "nothing left billing" needs that step.
	if b, ok := f.buckets[name]; ok && len(b.objects) > 0 {
		return nil, &s3types.NoSuchBucket{Message: aws.String("BucketNotEmpty")}
	}
	delete(f.buckets, name)
	return &s3.DeleteBucketOutput{}, nil
}

func (f *fakeS3) PutBucketTagging(_ context.Context, in *s3.PutBucketTaggingInput, _ ...func(*s3.Options)) (*s3.PutBucketTaggingOutput, error) {
	if b, ok := f.buckets[aws.ToString(in.Bucket)]; ok && in.Tagging != nil {
		for _, tg := range in.Tagging.TagSet {
			b.tags[aws.ToString(tg.Key)] = aws.ToString(tg.Value)
		}
	}
	return &s3.PutBucketTaggingOutput{}, nil
}

func (f *fakeS3) PutPublicAccessBlock(_ context.Context, in *s3.PutPublicAccessBlockInput, _ ...func(*s3.Options)) (*s3.PutPublicAccessBlockOutput, error) {
	if b, ok := f.buckets[aws.ToString(in.Bucket)]; ok {
		b.publicBlock = true
	}
	return &s3.PutPublicAccessBlockOutput{}, nil
}

func (f *fakeS3) PutBucketEncryption(_ context.Context, in *s3.PutBucketEncryptionInput, _ ...func(*s3.Options)) (*s3.PutBucketEncryptionOutput, error) {
	if b, ok := f.buckets[aws.ToString(in.Bucket)]; ok {
		b.encrypted = true
	}
	return &s3.PutBucketEncryptionOutput{}, nil
}

func (f *fakeS3) PutBucketLifecycleConfiguration(_ context.Context, in *s3.PutBucketLifecycleConfigurationInput, _ ...func(*s3.Options)) (*s3.PutBucketLifecycleConfigurationOutput, error) {
	if b, ok := f.buckets[aws.ToString(in.Bucket)]; ok {
		b.lifecycle = true
		b.lifecycleInput = in.LifecycleConfiguration
	}
	return &s3.PutBucketLifecycleConfigurationOutput{}, nil
}

func (f *fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	b, ok := f.buckets[aws.ToString(in.Bucket)]
	if !ok {
		return nil, &s3types.NoSuchBucket{}
	}
	out := &s3.ListObjectsV2Output{IsTruncated: aws.Bool(false)}
	for k := range b.objects {
		out.Contents = append(out.Contents, s3types.Object{Key: aws.String(k)})
	}
	return out, nil
}

func (f *fakeS3) DeleteObjects(_ context.Context, in *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	name := aws.ToString(in.Bucket)
	if f.deleteObjectErrors[name] {
		return &s3.DeleteObjectsOutput{Errors: []s3types.Error{{
			Key:     aws.String("stuck-object"),
			Message: aws.String("AccessDenied"),
		}}}, nil
	}
	b, ok := f.buckets[name]
	if !ok {
		return nil, &s3types.NoSuchBucket{}
	}
	if in.Delete != nil {
		for _, o := range in.Delete.Objects {
			delete(b.objects, aws.ToString(o.Key))
		}
	}
	return &s3.DeleteObjectsOutput{}, nil
}

// seedObject puts an object in a bucket so teardown's empty step has work to do.
func (f *fakeS3) seedObject(bucket, key string) {
	if b, ok := f.buckets[bucket]; ok {
		b.objects[key] = struct{}{}
	}
}

// unused, but keeps the poll seam honest if a wait test needs a clock.
var _ = time.Sleep

// NewFake returns a Deployer over in-memory AWS stand-ins — the FORAY_FAKE=1 path.
// It exercises the real resource list, ordering and idempotency rules with no AWS
// account, no credentials and no cost, which is what lets `foray deploy` be
// rehearsed (and CI-gated) offline.
func NewFake(cfg Config) (*Deployer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.AccountID == "" {
		// A syntactically valid 12-digit account id, so the policy ARNs the fake
		// builds look like the real ones.
		cfg.AccountID = "000000000000"
	}
	return newWith(cfg, newFakeDynamo(), newFakeS3(), newFakeIAM()), nil
}

// --- IAM --------------------------------------------------------------------

type fakeRole struct {
	assumeDoc string
	inline    map[string]string
	attached  map[string]struct{}
	tags      map[string]string
}

type fakeProfile struct {
	roles map[string]struct{}
	tags  map[string]string
}

type fakeIAM struct {
	roles    map[string]*fakeRole
	profiles map[string]*fakeProfile
}

func newFakeIAM() *fakeIAM {
	return &fakeIAM{roles: map[string]*fakeRole{}, profiles: map[string]*fakeProfile{}}
}

func (f *fakeIAM) GetRole(_ context.Context, in *iam.GetRoleInput, _ ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	name := aws.ToString(in.RoleName)
	r, ok := f.roles[name]
	if !ok {
		return nil, &iamtypes.NoSuchEntityException{Message: aws.String("no such role")}
	}
	return &iam.GetRoleOutput{Role: &iamtypes.Role{
		RoleName: in.RoleName,
		// IAM returns the trust policy URL-encoded; mirror that so a test comparing
		// documents has to decode, exactly as it would against the real API.
		AssumeRolePolicyDocument: aws.String(url.QueryEscape(r.assumeDoc)),
	}}, nil
}

func (f *fakeIAM) CreateRole(_ context.Context, in *iam.CreateRoleInput, _ ...func(*iam.Options)) (*iam.CreateRoleOutput, error) {
	name := aws.ToString(in.RoleName)
	if _, ok := f.roles[name]; ok {
		return nil, &iamtypes.EntityAlreadyExistsException{Message: aws.String("exists")}
	}
	tags := map[string]string{}
	for _, t := range in.Tags {
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	f.roles[name] = &fakeRole{
		assumeDoc: aws.ToString(in.AssumeRolePolicyDocument),
		inline:    map[string]string{},
		attached:  map[string]struct{}{},
		tags:      tags,
	}
	return &iam.CreateRoleOutput{}, nil
}

// DeleteRole models IAM's refusal to delete a role that still carries policies —
// the constraint that makes teardown ordering load-bearing.
func (f *fakeIAM) DeleteRole(_ context.Context, in *iam.DeleteRoleInput, _ ...func(*iam.Options)) (*iam.DeleteRoleOutput, error) {
	name := aws.ToString(in.RoleName)
	r, ok := f.roles[name]
	if !ok {
		return nil, &iamtypes.NoSuchEntityException{Message: aws.String("no such role")}
	}
	if len(r.inline) > 0 || len(r.attached) > 0 {
		return nil, &iamtypes.DeleteConflictException{
			Message: aws.String("Cannot delete entity, must delete policies first"),
		}
	}
	// A role still held by an instance profile cannot be deleted either.
	for pn, p := range f.profiles {
		if _, held := p.roles[name]; held {
			return nil, &iamtypes.DeleteConflictException{
				Message: aws.String("Cannot delete entity, must remove roles from instance profile " + pn),
			}
		}
	}
	delete(f.roles, name)
	return &iam.DeleteRoleOutput{}, nil
}

func (f *fakeIAM) UpdateAssumeRolePolicy(_ context.Context, in *iam.UpdateAssumeRolePolicyInput, _ ...func(*iam.Options)) (*iam.UpdateAssumeRolePolicyOutput, error) {
	if r, ok := f.roles[aws.ToString(in.RoleName)]; ok {
		r.assumeDoc = aws.ToString(in.PolicyDocument)
	}
	return &iam.UpdateAssumeRolePolicyOutput{}, nil
}

func (f *fakeIAM) TagRole(_ context.Context, in *iam.TagRoleInput, _ ...func(*iam.Options)) (*iam.TagRoleOutput, error) {
	if r, ok := f.roles[aws.ToString(in.RoleName)]; ok {
		for _, t := range in.Tags {
			r.tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
		}
	}
	return &iam.TagRoleOutput{}, nil
}

func (f *fakeIAM) PutRolePolicy(_ context.Context, in *iam.PutRolePolicyInput, _ ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error) {
	r, ok := f.roles[aws.ToString(in.RoleName)]
	if !ok {
		return nil, &iamtypes.NoSuchEntityException{}
	}
	r.inline[aws.ToString(in.PolicyName)] = aws.ToString(in.PolicyDocument)
	return &iam.PutRolePolicyOutput{}, nil
}

func (f *fakeIAM) DeleteRolePolicy(_ context.Context, in *iam.DeleteRolePolicyInput, _ ...func(*iam.Options)) (*iam.DeleteRolePolicyOutput, error) {
	if r, ok := f.roles[aws.ToString(in.RoleName)]; ok {
		delete(r.inline, aws.ToString(in.PolicyName))
	}
	return &iam.DeleteRolePolicyOutput{}, nil
}

func (f *fakeIAM) ListRolePolicies(_ context.Context, in *iam.ListRolePoliciesInput, _ ...func(*iam.Options)) (*iam.ListRolePoliciesOutput, error) {
	r, ok := f.roles[aws.ToString(in.RoleName)]
	if !ok {
		return nil, &iamtypes.NoSuchEntityException{}
	}
	out := &iam.ListRolePoliciesOutput{}
	for n := range r.inline {
		out.PolicyNames = append(out.PolicyNames, n)
	}
	sort.Strings(out.PolicyNames)
	return out, nil
}

func (f *fakeIAM) AttachRolePolicy(_ context.Context, in *iam.AttachRolePolicyInput, _ ...func(*iam.Options)) (*iam.AttachRolePolicyOutput, error) {
	r, ok := f.roles[aws.ToString(in.RoleName)]
	if !ok {
		return nil, &iamtypes.NoSuchEntityException{}
	}
	// Attaching twice is a no-op in IAM, which is why ensure can Put unconditionally.
	r.attached[aws.ToString(in.PolicyArn)] = struct{}{}
	return &iam.AttachRolePolicyOutput{}, nil
}

func (f *fakeIAM) DetachRolePolicy(_ context.Context, in *iam.DetachRolePolicyInput, _ ...func(*iam.Options)) (*iam.DetachRolePolicyOutput, error) {
	if r, ok := f.roles[aws.ToString(in.RoleName)]; ok {
		delete(r.attached, aws.ToString(in.PolicyArn))
	}
	return &iam.DetachRolePolicyOutput{}, nil
}

func (f *fakeIAM) ListAttachedRolePolicies(_ context.Context, in *iam.ListAttachedRolePoliciesInput, _ ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error) {
	r, ok := f.roles[aws.ToString(in.RoleName)]
	if !ok {
		return nil, &iamtypes.NoSuchEntityException{}
	}
	out := &iam.ListAttachedRolePoliciesOutput{}
	arns := make([]string, 0, len(r.attached))
	for a := range r.attached {
		arns = append(arns, a)
	}
	sort.Strings(arns)
	for _, a := range arns {
		out.AttachedPolicies = append(out.AttachedPolicies, iamtypes.AttachedPolicy{PolicyArn: aws.String(a)})
	}
	return out, nil
}

func (f *fakeIAM) GetInstanceProfile(_ context.Context, in *iam.GetInstanceProfileInput, _ ...func(*iam.Options)) (*iam.GetInstanceProfileOutput, error) {
	name := aws.ToString(in.InstanceProfileName)
	p, ok := f.profiles[name]
	if !ok {
		return nil, &iamtypes.NoSuchEntityException{Message: aws.String("no such instance profile")}
	}
	out := &iam.GetInstanceProfileOutput{InstanceProfile: &iamtypes.InstanceProfile{
		InstanceProfileName: in.InstanceProfileName,
	}}
	names := make([]string, 0, len(p.roles))
	for r := range p.roles {
		names = append(names, r)
	}
	sort.Strings(names)
	for _, r := range names {
		out.InstanceProfile.Roles = append(out.InstanceProfile.Roles, iamtypes.Role{RoleName: aws.String(r)})
	}
	return out, nil
}

func (f *fakeIAM) CreateInstanceProfile(_ context.Context, in *iam.CreateInstanceProfileInput, _ ...func(*iam.Options)) (*iam.CreateInstanceProfileOutput, error) {
	name := aws.ToString(in.InstanceProfileName)
	if _, ok := f.profiles[name]; ok {
		return nil, &iamtypes.EntityAlreadyExistsException{}
	}
	tags := map[string]string{}
	for _, t := range in.Tags {
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	f.profiles[name] = &fakeProfile{roles: map[string]struct{}{}, tags: tags}
	return &iam.CreateInstanceProfileOutput{}, nil
}

// DeleteInstanceProfile models IAM's refusal while a role is still attached.
func (f *fakeIAM) DeleteInstanceProfile(_ context.Context, in *iam.DeleteInstanceProfileInput, _ ...func(*iam.Options)) (*iam.DeleteInstanceProfileOutput, error) {
	name := aws.ToString(in.InstanceProfileName)
	p, ok := f.profiles[name]
	if !ok {
		return nil, &iamtypes.NoSuchEntityException{}
	}
	if len(p.roles) > 0 {
		return nil, &iamtypes.DeleteConflictException{
			Message: aws.String("Cannot delete entity, must remove roles from instance profile first"),
		}
	}
	delete(f.profiles, name)
	return &iam.DeleteInstanceProfileOutput{}, nil
}

// AddRoleToInstanceProfile models the one-role limit: a second role is a
// LimitExceeded, which is why ensure checks before adding.
func (f *fakeIAM) AddRoleToInstanceProfile(_ context.Context, in *iam.AddRoleToInstanceProfileInput, _ ...func(*iam.Options)) (*iam.AddRoleToInstanceProfileOutput, error) {
	p, ok := f.profiles[aws.ToString(in.InstanceProfileName)]
	if !ok {
		return nil, &iamtypes.NoSuchEntityException{}
	}
	if len(p.roles) > 0 {
		return nil, &iamtypes.LimitExceededException{
			Message: aws.String("Cannot exceed quota for InstanceSessionsPerInstanceProfile: 1"),
		}
	}
	p.roles[aws.ToString(in.RoleName)] = struct{}{}
	return &iam.AddRoleToInstanceProfileOutput{}, nil
}

func (f *fakeIAM) RemoveRoleFromInstanceProfile(_ context.Context, in *iam.RemoveRoleFromInstanceProfileInput, _ ...func(*iam.Options)) (*iam.RemoveRoleFromInstanceProfileOutput, error) {
	if p, ok := f.profiles[aws.ToString(in.InstanceProfileName)]; ok {
		delete(p.roles, aws.ToString(in.RoleName))
	}
	return &iam.RemoveRoleFromInstanceProfileOutput{}, nil
}
