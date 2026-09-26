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
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/apigatewayv2"
	apitypes "github.com/aws/aws-sdk-go-v2/service/apigatewayv2/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	cwl "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwltypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
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
	// policy and cors are what the CDN writes once it knows its own identity.
	policy     string
	cors       *s3types.CORSConfiguration
	objectData map[string]fakeObject
}

// fakeObject records what an upload stored, so content type and change detection are
// observable.
type fakeObject struct {
	etag        string
	contentType string
}

type fakeS3 struct {
	buckets map[string]*fakeBucket
	creates int
	puts    int
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
	f.buckets[name] = &fakeBucket{
		region:     region,
		objects:    map[string]struct{}{},
		tags:       map[string]string{},
		objectData: map[string]fakeObject{},
	}
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
	keys := make([]string, 0, len(b.objects))
	for k := range b.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		o := s3types.Object{Key: aws.String(k)}
		if data, ok := b.objectData[k]; ok {
			// S3 quotes ETags; mirror that so the sync's trimming is exercised.
			o.ETag = aws.String(`"` + data.etag + `"`)
		}
		out.Contents = append(out.Contents, o)
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
	// The rehearsal has no built zips, so hand the Lambdas canned bytes.
	fakeZip := func(path string) ([]byte, error) { return []byte("fake-zip:" + path), nil }
	d := newWithZips(cfg, newFakeDynamo(), newFakeS3(), newFakeIAM(), newFakeLambda(), newFakeLogs(),
		newFakeAPIGW(), newFakeCloudFront(), fakeZip)
	// The rehearsal has no web/ tree either, and must not sit through a modeled
	// CloudFront propagation.
	for _, r := range d.resources {
		switch res := r.(type) {
		case *webSync:
			res.readDir = func(string) ([]string, error) { return []string{"index.html", "app.js", "styles.css"}, nil }
			res.readFile = func(p string) ([]byte, error) { return []byte("fake:" + p), nil }
		case *distribution:
			res.sleep = func(time.Duration) {}
		}
	}
	return d, nil
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

// --- Lambda -----------------------------------------------------------------

type fakeFunction struct {
	roleARN    string
	codeSHA    string
	layers     []string
	env        map[string]string
	timeout    int32
	memoryMB   int32
	arch       []lambdatypes.Architecture
	runtime    lambdatypes.Runtime
	tags       map[string]string
	codeUpdate int // how many times the code was republished
	// permissions is the function's resource policy: statement id → SourceArn.
	permissions map[string]string
}

type fakeLambda struct {
	functions map[string]*fakeFunction
	// latestLayerVersion is the newest published version; anything above it answers
	// AccessDenied, as the real public layer does.
	latestLayerVersion int32
	layerErr           error
	// layerProbes counts GetLayerVersion calls, so the search's cost is observable.
	layerProbes int
	// roleNotReady counts down the IAM-propagation window: while > 0, CreateFunction
	// rejects the role the way Lambda does before IAM has propagated.
	roleNotReady int
	// createErr fails CreateFunction outright, for the "not a propagation error"
	// path that must fail fast.
	createErr error
	creates   int
}

func newFakeLambda() *fakeLambda {
	return &fakeLambda{
		functions: map[string]*fakeFunction{},
		// Matches what the public arm64 layer was at in us-west-2 when this was
		// validated against a real account.
		latestLayerVersion: 30,
	}
}

// GetLayerVersion models the public layer's behavior: versions 1..latestLayerVersion
// are attachable, and anything beyond answers AccessDenied — NOT NotFound — because
// the resource policy is per-version and an unpublished version has none.
func (f *fakeLambda) GetLayerVersion(_ context.Context, in *lambda.GetLayerVersionInput, _ ...func(*lambda.Options)) (*lambda.GetLayerVersionOutput, error) {
	if f.layerErr != nil {
		return nil, f.layerErr
	}
	f.layerProbes++
	v := int32(aws.ToInt64(in.VersionNumber))
	if v < 1 || v > f.latestLayerVersion {
		return nil, &fakeAccessDenied{}
	}
	return &lambda.GetLayerVersionOutput{
		LayerVersionArn: aws.String(aws.ToString(in.LayerName) + fmt.Sprintf(":%d", v)),
		Version:         int64(v),
	}, nil
}

// fakeAccessDenied mimics Lambda's AccessDeniedException, which the SDK does not
// model as a service-specific type.
type fakeAccessDenied struct{}

func (fakeAccessDenied) Error() string     { return "api error AccessDeniedException: not authorized" }
func (fakeAccessDenied) ErrorCode() string { return "AccessDeniedException" }

func (f *fakeLambda) GetFunction(_ context.Context, in *lambda.GetFunctionInput, _ ...func(*lambda.Options)) (*lambda.GetFunctionOutput, error) {
	name := aws.ToString(in.FunctionName)
	fn, ok := f.functions[name]
	if !ok {
		return nil, &lambdatypes.ResourceNotFoundException{Message: aws.String("no such function")}
	}
	return &lambda.GetFunctionOutput{Configuration: &lambdatypes.FunctionConfiguration{
		FunctionName: in.FunctionName,
		FunctionArn:  aws.String("arn:aws:lambda:us-west-2:000000000000:function:" + name),
		CodeSha256:   aws.String(fn.codeSHA),
		Role:         aws.String(fn.roleARN),
	}}, nil
}

func (f *fakeLambda) CreateFunction(_ context.Context, in *lambda.CreateFunctionInput, _ ...func(*lambda.Options)) (*lambda.CreateFunctionOutput, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.roleNotReady > 0 {
		f.roleNotReady--
		// Verbatim shape of the error Lambda returns before IAM has propagated.
		return nil, &lambdatypes.InvalidParameterValueException{
			Message: aws.String("The role defined for the function cannot be assumed by Lambda."),
		}
	}
	name := aws.ToString(in.FunctionName)
	if _, ok := f.functions[name]; ok {
		return nil, &lambdatypes.ResourceConflictException{Message: aws.String("exists")}
	}
	f.creates++
	env := map[string]string{}
	if in.Environment != nil {
		for k, v := range in.Environment.Variables {
			env[k] = v
		}
	}
	tags := map[string]string{}
	for k, v := range in.Tags {
		tags[k] = v
	}
	f.functions[name] = &fakeFunction{
		roleARN:     aws.ToString(in.Role),
		codeSHA:     zipSHA256(in.Code.ZipFile),
		layers:      in.Layers,
		env:         env,
		timeout:     aws.ToInt32(in.Timeout),
		memoryMB:    aws.ToInt32(in.MemorySize),
		arch:        in.Architectures,
		runtime:     in.Runtime,
		tags:        tags,
		permissions: map[string]string{},
	}
	return &lambda.CreateFunctionOutput{}, nil
}

func (f *fakeLambda) DeleteFunction(_ context.Context, in *lambda.DeleteFunctionInput, _ ...func(*lambda.Options)) (*lambda.DeleteFunctionOutput, error) {
	delete(f.functions, aws.ToString(in.FunctionName))
	return &lambda.DeleteFunctionOutput{}, nil
}

func (f *fakeLambda) UpdateFunctionCode(_ context.Context, in *lambda.UpdateFunctionCodeInput, _ ...func(*lambda.Options)) (*lambda.UpdateFunctionCodeOutput, error) {
	fn, ok := f.functions[aws.ToString(in.FunctionName)]
	if !ok {
		return nil, &lambdatypes.ResourceNotFoundException{}
	}
	fn.codeSHA = zipSHA256(in.ZipFile)
	fn.codeUpdate++
	return &lambda.UpdateFunctionCodeOutput{}, nil
}

func (f *fakeLambda) UpdateFunctionConfiguration(_ context.Context, in *lambda.UpdateFunctionConfigurationInput, _ ...func(*lambda.Options)) (*lambda.UpdateFunctionConfigurationOutput, error) {
	fn, ok := f.functions[aws.ToString(in.FunctionName)]
	if !ok {
		return nil, &lambdatypes.ResourceNotFoundException{}
	}
	if in.Environment != nil {
		fn.env = map[string]string{}
		for k, v := range in.Environment.Variables {
			fn.env[k] = v
		}
	}
	if in.Layers != nil {
		fn.layers = in.Layers
	}
	fn.timeout = aws.ToInt32(in.Timeout)
	fn.memoryMB = aws.ToInt32(in.MemorySize)
	return &lambda.UpdateFunctionConfigurationOutput{}, nil
}

func (f *fakeLambda) TagResource(_ context.Context, in *lambda.TagResourceInput, _ ...func(*lambda.Options)) (*lambda.TagResourceOutput, error) {
	// Resource is an ARN; map it back to the function name's suffix.
	arn := aws.ToString(in.Resource)
	for name, fn := range f.functions {
		if strings.HasSuffix(arn, ":"+name) {
			for k, v := range in.Tags {
				fn.tags[k] = v
			}
		}
	}
	return &lambda.TagResourceOutput{}, nil
}

// --- CloudWatch Logs --------------------------------------------------------

type fakeLogGroup struct {
	retentionDays int32
	tags          map[string]string
}

type fakeLogs struct {
	groups map[string]*fakeLogGroup
	// tagErr makes tagging fail, so the best-effort path is exercised.
	tagErr error
}

func newFakeLogs() *fakeLogs {
	return &fakeLogs{groups: map[string]*fakeLogGroup{}}
}

func (f *fakeLogs) CreateLogGroup(_ context.Context, in *cwl.CreateLogGroupInput, _ ...func(*cwl.Options)) (*cwl.CreateLogGroupOutput, error) {
	name := aws.ToString(in.LogGroupName)
	if _, ok := f.groups[name]; ok {
		return nil, &cwltypes.ResourceAlreadyExistsException{Message: aws.String("exists")}
	}
	tags := map[string]string{}
	for k, v := range in.Tags {
		tags[k] = v
	}
	f.groups[name] = &fakeLogGroup{tags: tags}
	return &cwl.CreateLogGroupOutput{}, nil
}

func (f *fakeLogs) DeleteLogGroup(_ context.Context, in *cwl.DeleteLogGroupInput, _ ...func(*cwl.Options)) (*cwl.DeleteLogGroupOutput, error) {
	delete(f.groups, aws.ToString(in.LogGroupName))
	return &cwl.DeleteLogGroupOutput{}, nil
}

func (f *fakeLogs) PutRetentionPolicy(_ context.Context, in *cwl.PutRetentionPolicyInput, _ ...func(*cwl.Options)) (*cwl.PutRetentionPolicyOutput, error) {
	g, ok := f.groups[aws.ToString(in.LogGroupName)]
	if !ok {
		return nil, &cwltypes.ResourceNotFoundException{}
	}
	g.retentionDays = aws.ToInt32(in.RetentionInDays)
	return &cwl.PutRetentionPolicyOutput{}, nil
}

// DescribeLogGroups filters by PREFIX, as the real API does — which is why
// logGroup.exists has to compare names exactly.
func (f *fakeLogs) DescribeLogGroups(_ context.Context, in *cwl.DescribeLogGroupsInput, _ ...func(*cwl.Options)) (*cwl.DescribeLogGroupsOutput, error) {
	prefix := aws.ToString(in.LogGroupNamePrefix)
	out := &cwl.DescribeLogGroupsOutput{}
	names := make([]string, 0, len(f.groups))
	for n := range f.groups {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if prefix == "" || strings.HasPrefix(n, prefix) {
			out.LogGroups = append(out.LogGroups, cwltypes.LogGroup{LogGroupName: aws.String(n)})
		}
	}
	return out, nil
}

func (f *fakeLogs) TagResource(_ context.Context, in *cwl.TagResourceInput, _ ...func(*cwl.Options)) (*cwl.TagResourceOutput, error) {
	if f.tagErr != nil {
		return nil, f.tagErr
	}
	return &cwl.TagResourceOutput{}, nil
}

// --- Lambda resource policy -------------------------------------------------

// permissions on fakeLambda model the function resource policy the HTTP API writes.
func (f *fakeLambda) AddPermission(_ context.Context, in *lambda.AddPermissionInput, _ ...func(*lambda.Options)) (*lambda.AddPermissionOutput, error) {
	name := aws.ToString(in.FunctionName)
	fn, ok := f.functions[name]
	if !ok {
		return nil, &lambdatypes.ResourceNotFoundException{Message: aws.String("no such function")}
	}
	sid := aws.ToString(in.StatementId)
	if _, exists := fn.permissions[sid]; exists {
		// The real API rejects a duplicate statement id, which is the normal case on
		// re-apply since the id is fixed.
		return nil, &lambdatypes.ResourceConflictException{Message: aws.String("statement already exists")}
	}
	fn.permissions[sid] = aws.ToString(in.SourceArn)
	return &lambda.AddPermissionOutput{}, nil
}

func (f *fakeLambda) RemovePermission(_ context.Context, in *lambda.RemovePermissionInput, _ ...func(*lambda.Options)) (*lambda.RemovePermissionOutput, error) {
	fn, ok := f.functions[aws.ToString(in.FunctionName)]
	if !ok {
		return nil, &lambdatypes.ResourceNotFoundException{Message: aws.String("no such function")}
	}
	sid := aws.ToString(in.StatementId)
	if _, exists := fn.permissions[sid]; !exists {
		return nil, &lambdatypes.ResourceNotFoundException{Message: aws.String("no such statement")}
	}
	delete(fn.permissions, sid)
	return &lambda.RemovePermissionOutput{}, nil
}

// --- API Gateway v2 ---------------------------------------------------------

type fakeIntegration struct {
	id            string
	uri           string
	integType     apitypes.IntegrationType
	method        string
	payloadFormat string
}

type fakeRoute struct {
	id     string
	key    string
	target string
}

type fakeStage struct {
	autoDeploy bool
	logARN     string
	logFormat  string
}

type fakeAPI struct {
	id           string
	name         string
	protocol     apitypes.ProtocolType
	tags         map[string]string
	integrations map[string]*fakeIntegration // by id
	routes       map[string]*fakeRoute       // by route key
	stages       map[string]*fakeStage
}

type fakeAPIGW struct {
	apis map[string]*fakeAPI // by id
	seq  int
}

func newFakeAPIGW() *fakeAPIGW { return &fakeAPIGW{apis: map[string]*fakeAPI{}} }

// nextID mimics API Gateway's generated ids — the reason httpAPI owns everything
// that needs one rather than constructing it.
func (f *fakeAPIGW) nextID(prefix string) string {
	f.seq++
	return fmt.Sprintf("%s%04d", prefix, f.seq)
}

func (f *fakeAPIGW) GetApis(_ context.Context, _ *apigatewayv2.GetApisInput, _ ...func(*apigatewayv2.Options)) (*apigatewayv2.GetApisOutput, error) {
	out := &apigatewayv2.GetApisOutput{}
	ids := make([]string, 0, len(f.apis))
	for id := range f.apis {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		a := f.apis[id]
		out.Items = append(out.Items, apitypes.Api{ApiId: aws.String(a.id), Name: aws.String(a.name)})
	}
	return out, nil
}

func (f *fakeAPIGW) CreateApi(_ context.Context, in *apigatewayv2.CreateApiInput, _ ...func(*apigatewayv2.Options)) (*apigatewayv2.CreateApiOutput, error) {
	id := f.nextID("api")
	tags := map[string]string{}
	for k, v := range in.Tags {
		tags[k] = v
	}
	f.apis[id] = &fakeAPI{
		id:           id,
		name:         aws.ToString(in.Name),
		protocol:     in.ProtocolType,
		tags:         tags,
		integrations: map[string]*fakeIntegration{},
		routes:       map[string]*fakeRoute{},
		stages:       map[string]*fakeStage{},
	}
	return &apigatewayv2.CreateApiOutput{ApiId: aws.String(id), Name: in.Name}, nil
}

// DeleteApi cascades to integrations, routes and stages, as the real API does.
func (f *fakeAPIGW) DeleteApi(_ context.Context, in *apigatewayv2.DeleteApiInput, _ ...func(*apigatewayv2.Options)) (*apigatewayv2.DeleteApiOutput, error) {
	delete(f.apis, aws.ToString(in.ApiId))
	return &apigatewayv2.DeleteApiOutput{}, nil
}

func (f *fakeAPIGW) GetIntegrations(_ context.Context, in *apigatewayv2.GetIntegrationsInput, _ ...func(*apigatewayv2.Options)) (*apigatewayv2.GetIntegrationsOutput, error) {
	a, ok := f.apis[aws.ToString(in.ApiId)]
	if !ok {
		return nil, &apitypes.NotFoundException{Message: aws.String("no such api")}
	}
	out := &apigatewayv2.GetIntegrationsOutput{}
	ids := make([]string, 0, len(a.integrations))
	for id := range a.integrations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		it := a.integrations[id]
		out.Items = append(out.Items, apitypes.Integration{
			IntegrationId:  aws.String(it.id),
			IntegrationUri: aws.String(it.uri),
		})
	}
	return out, nil
}

func (f *fakeAPIGW) CreateIntegration(_ context.Context, in *apigatewayv2.CreateIntegrationInput, _ ...func(*apigatewayv2.Options)) (*apigatewayv2.CreateIntegrationOutput, error) {
	a, ok := f.apis[aws.ToString(in.ApiId)]
	if !ok {
		return nil, &apitypes.NotFoundException{Message: aws.String("no such api")}
	}
	id := f.nextID("int")
	a.integrations[id] = &fakeIntegration{
		id:            id,
		uri:           aws.ToString(in.IntegrationUri),
		integType:     in.IntegrationType,
		method:        aws.ToString(in.IntegrationMethod),
		payloadFormat: aws.ToString(in.PayloadFormatVersion),
	}
	return &apigatewayv2.CreateIntegrationOutput{IntegrationId: aws.String(id)}, nil
}

func (f *fakeAPIGW) GetRoutes(_ context.Context, in *apigatewayv2.GetRoutesInput, _ ...func(*apigatewayv2.Options)) (*apigatewayv2.GetRoutesOutput, error) {
	a, ok := f.apis[aws.ToString(in.ApiId)]
	if !ok {
		return nil, &apitypes.NotFoundException{Message: aws.String("no such api")}
	}
	out := &apigatewayv2.GetRoutesOutput{}
	keys := make([]string, 0, len(a.routes))
	for k := range a.routes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		r := a.routes[k]
		out.Items = append(out.Items, apitypes.Route{
			RouteId:  aws.String(r.id),
			RouteKey: aws.String(r.key),
			Target:   aws.String(r.target),
		})
	}
	return out, nil
}

func (f *fakeAPIGW) CreateRoute(_ context.Context, in *apigatewayv2.CreateRouteInput, _ ...func(*apigatewayv2.Options)) (*apigatewayv2.CreateRouteOutput, error) {
	a, ok := f.apis[aws.ToString(in.ApiId)]
	if !ok {
		return nil, &apitypes.NotFoundException{Message: aws.String("no such api")}
	}
	id := f.nextID("rte")
	a.routes[aws.ToString(in.RouteKey)] = &fakeRoute{
		id:     id,
		key:    aws.ToString(in.RouteKey),
		target: aws.ToString(in.Target),
	}
	return &apigatewayv2.CreateRouteOutput{RouteId: aws.String(id)}, nil
}

func (f *fakeAPIGW) UpdateRoute(_ context.Context, in *apigatewayv2.UpdateRouteInput, _ ...func(*apigatewayv2.Options)) (*apigatewayv2.UpdateRouteOutput, error) {
	a, ok := f.apis[aws.ToString(in.ApiId)]
	if !ok {
		return nil, &apitypes.NotFoundException{Message: aws.String("no such api")}
	}
	if r, ok := a.routes[aws.ToString(in.RouteKey)]; ok {
		r.target = aws.ToString(in.Target)
	}
	return &apigatewayv2.UpdateRouteOutput{}, nil
}

func (f *fakeAPIGW) GetStage(_ context.Context, in *apigatewayv2.GetStageInput, _ ...func(*apigatewayv2.Options)) (*apigatewayv2.GetStageOutput, error) {
	a, ok := f.apis[aws.ToString(in.ApiId)]
	if !ok {
		return nil, &apitypes.NotFoundException{Message: aws.String("no such api")}
	}
	s, ok := a.stages[aws.ToString(in.StageName)]
	if !ok {
		return nil, &apitypes.NotFoundException{Message: aws.String("no such stage")}
	}
	return &apigatewayv2.GetStageOutput{
		StageName:  in.StageName,
		AutoDeploy: aws.Bool(s.autoDeploy),
	}, nil
}

func (f *fakeAPIGW) CreateStage(_ context.Context, in *apigatewayv2.CreateStageInput, _ ...func(*apigatewayv2.Options)) (*apigatewayv2.CreateStageOutput, error) {
	a, ok := f.apis[aws.ToString(in.ApiId)]
	if !ok {
		return nil, &apitypes.NotFoundException{Message: aws.String("no such api")}
	}
	s := &fakeStage{autoDeploy: aws.ToBool(in.AutoDeploy)}
	if in.AccessLogSettings != nil {
		s.logARN = aws.ToString(in.AccessLogSettings.DestinationArn)
		s.logFormat = aws.ToString(in.AccessLogSettings.Format)
	}
	a.stages[aws.ToString(in.StageName)] = s
	return &apigatewayv2.CreateStageOutput{}, nil
}

func (f *fakeAPIGW) UpdateStage(_ context.Context, in *apigatewayv2.UpdateStageInput, _ ...func(*apigatewayv2.Options)) (*apigatewayv2.UpdateStageOutput, error) {
	a, ok := f.apis[aws.ToString(in.ApiId)]
	if !ok {
		return nil, &apitypes.NotFoundException{Message: aws.String("no such api")}
	}
	s, ok := a.stages[aws.ToString(in.StageName)]
	if !ok {
		return nil, &apitypes.NotFoundException{Message: aws.String("no such stage")}
	}
	s.autoDeploy = aws.ToBool(in.AutoDeploy)
	if in.AccessLogSettings != nil {
		s.logARN = aws.ToString(in.AccessLogSettings.DestinationArn)
		s.logFormat = aws.ToString(in.AccessLogSettings.Format)
	}
	return &apigatewayv2.UpdateStageOutput{}, nil
}

// apiByName finds a fake API by name, for tests.
func (f *fakeAPIGW) apiByName(name string) *fakeAPI {
	for _, a := range f.apis {
		if a.name == name {
			return a
		}
	}
	return nil
}

// --- S3 bucket policy / CORS / SPA upload -----------------------------------

func (f *fakeS3) PutBucketPolicy(_ context.Context, in *s3.PutBucketPolicyInput, _ ...func(*s3.Options)) (*s3.PutBucketPolicyOutput, error) {
	b, ok := f.buckets[aws.ToString(in.Bucket)]
	if !ok {
		return nil, &s3types.NoSuchBucket{}
	}
	b.policy = aws.ToString(in.Policy)
	return &s3.PutBucketPolicyOutput{}, nil
}

func (f *fakeS3) PutBucketCors(_ context.Context, in *s3.PutBucketCorsInput, _ ...func(*s3.Options)) (*s3.PutBucketCorsOutput, error) {
	b, ok := f.buckets[aws.ToString(in.Bucket)]
	if !ok {
		return nil, &s3types.NoSuchBucket{}
	}
	b.cors = in.CORSConfiguration
	return &s3.PutBucketCorsOutput{}, nil
}

func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	name := aws.ToString(in.Bucket)
	b, ok := f.buckets[name]
	if !ok {
		return nil, &s3types.NoSuchBucket{}
	}
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	key := aws.ToString(in.Key)
	b.objects[key] = struct{}{}
	if b.objectData == nil {
		b.objectData = map[string]fakeObject{}
	}
	b.objectData[key] = fakeObject{etag: contentMD5(body), contentType: aws.ToString(in.ContentType)}
	f.puts++
	return &s3.PutObjectOutput{}, nil
}

// --- CloudFront -------------------------------------------------------------

type fakeDistribution struct {
	id      string
	comment string
	domain  string
	enabled bool
	// status is "InProgress" until deployTicks polls have elapsed, modeling the
	// minutes CloudFront takes to propagate to every edge.
	status      string
	deployTicks int
	config      *cftypes.DistributionConfig
	etag        string
	etagSeq     int
	tags        map[string]string
}

type fakeOAC struct {
	id   string
	name string
	etag string
}

type fakeCloudFront struct {
	dists map[string]*fakeDistribution
	oacs  map[string]*fakeOAC
	seq   int
	// propagationTicks is how many GetDistribution calls a change takes to deploy.
	propagationTicks int
	// deleteWhileEnabled records an attempt to delete an enabled distribution, which
	// the real API refuses.
	deleteWhileEnabled bool
	// staleIfMatch records a delete/update carrying an outdated ETag.
	staleIfMatch bool
	// illegalUpdate records an update that dropped a field CloudFront requires.
	illegalUpdate bool
}

func newFakeCloudFront() *fakeCloudFront {
	return &fakeCloudFront{
		dists:            map[string]*fakeDistribution{},
		oacs:             map[string]*fakeOAC{},
		propagationTicks: 2,
	}
}

func (f *fakeCloudFront) nextID(prefix string) string {
	f.seq++
	return fmt.Sprintf("%s%04d", prefix, f.seq)
}

func (f *fakeCloudFront) ListDistributions(_ context.Context, _ *cloudfront.ListDistributionsInput, _ ...func(*cloudfront.Options)) (*cloudfront.ListDistributionsOutput, error) {
	list := &cftypes.DistributionList{IsTruncated: aws.Bool(false)}
	ids := make([]string, 0, len(f.dists))
	for id := range f.dists {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		d := f.dists[id]
		list.Items = append(list.Items, cftypes.DistributionSummary{
			Id:         aws.String(d.id),
			Comment:    aws.String(d.comment),
			DomainName: aws.String(d.domain),
			Enabled:    aws.Bool(d.enabled),
		})
	}
	return &cloudfront.ListDistributionsOutput{DistributionList: list}, nil
}

func (f *fakeCloudFront) CreateDistributionWithTags(_ context.Context, in *cloudfront.CreateDistributionWithTagsInput, _ ...func(*cloudfront.Options)) (*cloudfront.CreateDistributionWithTagsOutput, error) {
	id := f.nextID("E")
	cfgIn := in.DistributionConfigWithTags.DistributionConfig
	tags := map[string]string{}
	if in.DistributionConfigWithTags.Tags != nil {
		for _, t := range in.DistributionConfigWithTags.Tags.Items {
			tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
		}
	}
	// CloudFront defaults fields the caller omits; Aliases is one, and its presence is
	// what makes a from-scratch update illegal.
	if cfgIn.Aliases == nil {
		cfgIn.Aliases = &cftypes.Aliases{Quantity: aws.Int32(0)}
	}
	d := &fakeDistribution{
		id:          id,
		comment:     aws.ToString(cfgIn.Comment),
		domain:      strings.ToLower(id) + ".cloudfront.net",
		enabled:     aws.ToBool(cfgIn.Enabled),
		status:      "InProgress",
		deployTicks: f.propagationTicks,
		config:      cfgIn,
		etag:        "etag-0",
		tags:        tags,
	}
	f.dists[id] = d
	return &cloudfront.CreateDistributionWithTagsOutput{
		Distribution: &cftypes.Distribution{
			Id:         aws.String(id),
			DomainName: aws.String(d.domain),
			Status:     aws.String(d.status),
		},
		ETag: aws.String(d.etag),
	}, nil
}

// GetDistribution counts down to Deployed, so a waiting caller actually polls.
func (f *fakeCloudFront) GetDistribution(_ context.Context, in *cloudfront.GetDistributionInput, _ ...func(*cloudfront.Options)) (*cloudfront.GetDistributionOutput, error) {
	d, ok := f.dists[aws.ToString(in.Id)]
	if !ok {
		return nil, &cftypes.NoSuchDistribution{}
	}
	if d.deployTicks > 0 {
		d.deployTicks--
		if d.deployTicks == 0 {
			d.status = "Deployed"
		}
	}
	return &cloudfront.GetDistributionOutput{
		Distribution: &cftypes.Distribution{
			Id:         aws.String(d.id),
			DomainName: aws.String(d.domain),
			Status:     aws.String(d.status),
		},
		ETag: aws.String(d.etag),
	}, nil
}

func (f *fakeCloudFront) GetDistributionConfig(_ context.Context, in *cloudfront.GetDistributionConfigInput, _ ...func(*cloudfront.Options)) (*cloudfront.GetDistributionConfigOutput, error) {
	d, ok := f.dists[aws.ToString(in.Id)]
	if !ok {
		return nil, &cftypes.NoSuchDistribution{}
	}
	cfgCopy := *d.config
	cfgCopy.Enabled = aws.Bool(d.enabled)
	return &cloudfront.GetDistributionConfigOutput{
		DistributionConfig: &cfgCopy,
		ETag:               aws.String(d.etag),
	}, nil
}

// UpdateDistribution requires the current ETag and rolls it, as the real API does.
func (f *fakeCloudFront) UpdateDistribution(_ context.Context, in *cloudfront.UpdateDistributionInput, _ ...func(*cloudfront.Options)) (*cloudfront.UpdateDistributionOutput, error) {
	d, ok := f.dists[aws.ToString(in.Id)]
	if !ok {
		return nil, &cftypes.NoSuchDistribution{}
	}
	if aws.ToString(in.IfMatch) != d.etag {
		f.staleIfMatch = true
		return nil, &cftypes.PreconditionFailed{Message: aws.String("stale ETag")}
	}
	// UpdateDistribution REPLACES the whole configuration and requires every field
	// CloudFront considers part of it, including ones a caller may never set. A config
	// built from scratch is rejected — the real API answers
	// `IllegalUpdate: Aliases are missing for the resource`. Model that, so building a
	// fresh config instead of read-modify-write fails here rather than on a live deploy.
	if d.config != nil && d.config.Aliases != nil && in.DistributionConfig.Aliases == nil {
		f.illegalUpdate = true
		return nil, &cftypes.IllegalUpdate{Message: aws.String("Aliases are missing for the resource")}
	}
	d.enabled = aws.ToBool(in.DistributionConfig.Enabled)
	d.config = in.DistributionConfig
	d.status = "InProgress"
	d.deployTicks = f.propagationTicks
	d.etagSeq++
	d.etag = fmt.Sprintf("etag-%d", d.etagSeq)
	return &cloudfront.UpdateDistributionOutput{ETag: aws.String(d.etag)}, nil
}

// DeleteDistribution refuses an enabled distribution and a stale ETag — the two
// constraints that make CloudFront teardown a three-step dance.
func (f *fakeCloudFront) DeleteDistribution(_ context.Context, in *cloudfront.DeleteDistributionInput, _ ...func(*cloudfront.Options)) (*cloudfront.DeleteDistributionOutput, error) {
	id := aws.ToString(in.Id)
	d, ok := f.dists[id]
	if !ok {
		return nil, &cftypes.NoSuchDistribution{}
	}
	if d.enabled {
		f.deleteWhileEnabled = true
		return nil, &cftypes.DistributionNotDisabled{Message: aws.String("distribution is not disabled")}
	}
	if aws.ToString(in.IfMatch) != d.etag {
		f.staleIfMatch = true
		return nil, &cftypes.PreconditionFailed{Message: aws.String("stale ETag")}
	}
	delete(f.dists, id)
	return &cloudfront.DeleteDistributionOutput{}, nil
}

func (f *fakeCloudFront) ListOriginAccessControls(_ context.Context, _ *cloudfront.ListOriginAccessControlsInput, _ ...func(*cloudfront.Options)) (*cloudfront.ListOriginAccessControlsOutput, error) {
	list := &cftypes.OriginAccessControlList{}
	ids := make([]string, 0, len(f.oacs))
	for id := range f.oacs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		o := f.oacs[id]
		list.Items = append(list.Items, cftypes.OriginAccessControlSummary{
			Id:   aws.String(o.id),
			Name: aws.String(o.name),
		})
	}
	return &cloudfront.ListOriginAccessControlsOutput{OriginAccessControlList: list}, nil
}

func (f *fakeCloudFront) CreateOriginAccessControl(_ context.Context, in *cloudfront.CreateOriginAccessControlInput, _ ...func(*cloudfront.Options)) (*cloudfront.CreateOriginAccessControlOutput, error) {
	id := f.nextID("OAC")
	f.oacs[id] = &fakeOAC{id: id, name: aws.ToString(in.OriginAccessControlConfig.Name), etag: "oac-etag-0"}
	return &cloudfront.CreateOriginAccessControlOutput{
		OriginAccessControl: &cftypes.OriginAccessControl{Id: aws.String(id)},
		ETag:                aws.String(f.oacs[id].etag),
	}, nil
}

func (f *fakeCloudFront) GetOriginAccessControl(_ context.Context, in *cloudfront.GetOriginAccessControlInput, _ ...func(*cloudfront.Options)) (*cloudfront.GetOriginAccessControlOutput, error) {
	o, ok := f.oacs[aws.ToString(in.Id)]
	if !ok {
		return nil, &cftypes.NoSuchOriginAccessControl{}
	}
	return &cloudfront.GetOriginAccessControlOutput{
		OriginAccessControl: &cftypes.OriginAccessControl{Id: aws.String(o.id)},
		ETag:                aws.String(o.etag),
	}, nil
}

func (f *fakeCloudFront) DeleteOriginAccessControl(_ context.Context, in *cloudfront.DeleteOriginAccessControlInput, _ ...func(*cloudfront.Options)) (*cloudfront.DeleteOriginAccessControlOutput, error) {
	id := aws.ToString(in.Id)
	o, ok := f.oacs[id]
	if !ok {
		return nil, &cftypes.NoSuchOriginAccessControl{}
	}
	if aws.ToString(in.IfMatch) != o.etag {
		f.staleIfMatch = true
		return nil, &cftypes.PreconditionFailed{Message: aws.String("stale ETag")}
	}
	delete(f.oacs, id)
	return &cloudfront.DeleteOriginAccessControlOutput{}, nil
}

func (f *fakeCloudFront) distByComment(comment string) *fakeDistribution {
	for _, d := range f.dists {
		if d.comment == comment {
			return d
		}
	}
	return nil
}
