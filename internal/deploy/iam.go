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
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
)

// iamAPI is the slice of IAM this package uses. The teardown half (List*, Detach*,
// Delete*) is as load-bearing as the create half — see role.remove.
type iamAPI interface {
	GetRole(ctx context.Context, in *iam.GetRoleInput, opts ...func(*iam.Options)) (*iam.GetRoleOutput, error)
	CreateRole(ctx context.Context, in *iam.CreateRoleInput, opts ...func(*iam.Options)) (*iam.CreateRoleOutput, error)
	DeleteRole(ctx context.Context, in *iam.DeleteRoleInput, opts ...func(*iam.Options)) (*iam.DeleteRoleOutput, error)
	UpdateAssumeRolePolicy(ctx context.Context, in *iam.UpdateAssumeRolePolicyInput, opts ...func(*iam.Options)) (*iam.UpdateAssumeRolePolicyOutput, error)
	TagRole(ctx context.Context, in *iam.TagRoleInput, opts ...func(*iam.Options)) (*iam.TagRoleOutput, error)

	PutRolePolicy(ctx context.Context, in *iam.PutRolePolicyInput, opts ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error)
	DeleteRolePolicy(ctx context.Context, in *iam.DeleteRolePolicyInput, opts ...func(*iam.Options)) (*iam.DeleteRolePolicyOutput, error)
	ListRolePolicies(ctx context.Context, in *iam.ListRolePoliciesInput, opts ...func(*iam.Options)) (*iam.ListRolePoliciesOutput, error)

	AttachRolePolicy(ctx context.Context, in *iam.AttachRolePolicyInput, opts ...func(*iam.Options)) (*iam.AttachRolePolicyOutput, error)
	DetachRolePolicy(ctx context.Context, in *iam.DetachRolePolicyInput, opts ...func(*iam.Options)) (*iam.DetachRolePolicyOutput, error)
	ListAttachedRolePolicies(ctx context.Context, in *iam.ListAttachedRolePoliciesInput, opts ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error)

	GetInstanceProfile(ctx context.Context, in *iam.GetInstanceProfileInput, opts ...func(*iam.Options)) (*iam.GetInstanceProfileOutput, error)
	CreateInstanceProfile(ctx context.Context, in *iam.CreateInstanceProfileInput, opts ...func(*iam.Options)) (*iam.CreateInstanceProfileOutput, error)
	DeleteInstanceProfile(ctx context.Context, in *iam.DeleteInstanceProfileInput, opts ...func(*iam.Options)) (*iam.DeleteInstanceProfileOutput, error)
	AddRoleToInstanceProfile(ctx context.Context, in *iam.AddRoleToInstanceProfileInput, opts ...func(*iam.Options)) (*iam.AddRoleToInstanceProfileOutput, error)
	RemoveRoleFromInstanceProfile(ctx context.Context, in *iam.RemoveRoleFromInstanceProfileInput, opts ...func(*iam.Options)) (*iam.RemoveRoleFromInstanceProfileOutput, error)
}

// Role names, fixed so the two deployment paths produce the same account. They
// match deploy/terraform/iam.tf exactly — a rename would orphan whatever the other
// path created.
const (
	RoleGatewayLambda = "foray-gateway-lambda"
	RoleWebAPILambda  = "foray-webapi-lambda"
	RoleSpawnInstance = "foray-spawn-instance"
)

// policyDoc is an IAM policy document.
type policyDoc struct {
	Version   string      `json:"Version"`
	Statement []statement `json:"Statement"`
}

// statement is one policy statement. Condition and Principal are `any` because
// IAM's shapes vary by key, and modeling each would buy nothing — these documents
// are written once, here, and asserted on by tests.
type statement struct {
	Sid       string `json:"Sid,omitempty"`
	Effect    string `json:"Effect"`
	Action    any    `json:"Action,omitempty"`
	Resource  any    `json:"Resource,omitempty"`
	Principal any    `json:"Principal,omitempty"`
	Condition any    `json:"Condition,omitempty"`
}

const policyVersion = "2012-10-17"

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		// These documents are literals built in this file; a marshal failure is a
		// programming error, not a runtime condition.
		panic(fmt.Sprintf("deploy: marshal policy: %v", err))
	}
	return string(b)
}

// assumeRolePolicy is the trust policy letting one AWS service assume the role.
func assumeRolePolicy(service string) string {
	return mustJSON(policyDoc{
		Version: policyVersion,
		Statement: []statement{{
			Effect:    "Allow",
			Action:    "sts:AssumeRole",
			Principal: map[string]string{"Service": service},
		}},
	})
}

// inlinePolicy is one named inline policy on a role.
type inlinePolicy struct {
	name string
	doc  string
}

// role provisions an IAM role with its trust policy, managed-policy attachments
// and inline policies.
type role struct {
	api         iamAPI
	roleName    string
	assumeDoc   string
	managed     []string
	inline      []inlinePolicy
	description string
}

func (r *role) kind() string { return "iam role" }
func (r *role) name() string { return r.roleName }

func (r *role) ensure(ctx context.Context) (Action, error) {
	act := Action{Kind: r.kind(), Name: r.roleName}

	exists, err := r.exists(ctx)
	if err != nil {
		return act, err
	}
	if exists {
		act.Op = OpExists
		// Converge the trust policy. A role whose trust policy drifted is a role
		// nothing can assume, which fails at invoke time rather than deploy time —
		// the worst place to find out.
		if _, err := r.api.UpdateAssumeRolePolicy(ctx, &iam.UpdateAssumeRolePolicyInput{
			RoleName:       aws.String(r.roleName),
			PolicyDocument: aws.String(r.assumeDoc),
		}); err != nil {
			return act, fmt.Errorf("update assume-role policy: %w", err)
		}
		if _, err := r.api.TagRole(ctx, &iam.TagRoleInput{
			RoleName: aws.String(r.roleName),
			Tags:     iamTags(Tags(r.roleName)),
		}); err != nil {
			return act, fmt.Errorf("tag role: %w", err)
		}
	} else {
		if _, err := r.api.CreateRole(ctx, &iam.CreateRoleInput{
			RoleName:                 aws.String(r.roleName),
			AssumeRolePolicyDocument: aws.String(r.assumeDoc),
			Description:              aws.String(r.description),
			Tags:                     iamTags(Tags(r.roleName)),
		}); err != nil {
			var exists *iamtypes.EntityAlreadyExistsException
			if !errors.As(err, &exists) {
				return act, fmt.Errorf("create role: %w", err)
			}
			// Lost a race with a concurrent apply; that is success here.
			act.Op = OpExists
		}
		if act.Op == "" {
			act.Op = OpCreate
		}
	}

	// Attachments and inline policies are Puts — idempotent, so they converge on
	// every run whether the role is new or not. That is deliberate: a role that
	// exists with the wrong permissions is the failure mode worth fixing silently.
	for _, arn := range r.managed {
		if _, err := r.api.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{
			RoleName:  aws.String(r.roleName),
			PolicyArn: aws.String(arn),
		}); err != nil {
			return act, fmt.Errorf("attach %s: %w", arn, err)
		}
	}
	for _, p := range r.inline {
		if _, err := r.api.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
			RoleName:       aws.String(r.roleName),
			PolicyName:     aws.String(p.name),
			PolicyDocument: aws.String(p.doc),
		}); err != nil {
			return act, fmt.Errorf("put inline policy %s: %w", p.name, err)
		}
	}
	if act.Detail == "" {
		act.Detail = fmt.Sprintf("%d inline, %d managed", len(r.inline), len(r.managed))
	}
	return act, nil
}

// remove deletes the role, first stripping everything IAM requires be gone.
//
// DeleteRole fails while the role still has inline policies or attached managed
// policies, so both are enumerated and removed — enumerated rather than assumed
// from r.inline/r.managed, because a role that drifted (or was created by the
// Terraform path with a different policy set) must still come off cleanly. Leaving
// a role behind does not bill, but it does block the next deploy from recreating
// it and makes teardown-verify report the account unclean.
func (r *role) remove(ctx context.Context) (Action, error) {
	act := Action{Kind: r.kind(), Name: r.roleName}
	exists, err := r.exists(ctx)
	if err != nil {
		return act, err
	}
	if !exists {
		act.Op = OpAbsent
		return act, nil
	}

	inlineNames, err := r.api.ListRolePolicies(ctx, &iam.ListRolePoliciesInput{RoleName: aws.String(r.roleName)})
	if err != nil {
		return act, fmt.Errorf("list inline policies: %w", err)
	}
	for _, n := range inlineNames.PolicyNames {
		if _, err := r.api.DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{
			RoleName:   aws.String(r.roleName),
			PolicyName: aws.String(n),
		}); err != nil {
			return act, fmt.Errorf("delete inline policy %s: %w", n, err)
		}
	}

	attached, err := r.api.ListAttachedRolePolicies(ctx, &iam.ListAttachedRolePoliciesInput{RoleName: aws.String(r.roleName)})
	if err != nil {
		return act, fmt.Errorf("list attached policies: %w", err)
	}
	for _, p := range attached.AttachedPolicies {
		if _, err := r.api.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{
			RoleName:  aws.String(r.roleName),
			PolicyArn: p.PolicyArn,
		}); err != nil {
			return act, fmt.Errorf("detach %s: %w", aws.ToString(p.PolicyArn), err)
		}
	}

	if _, err := r.api.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: aws.String(r.roleName)}); err != nil {
		return act, fmt.Errorf("delete role: %w", err)
	}
	act.Op = OpDelete
	return act, nil
}

func (r *role) exists(ctx context.Context) (bool, error) {
	_, err := r.api.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(r.roleName)})
	if err == nil {
		return true, nil
	}
	var notFound *iamtypes.NoSuchEntityException
	if errors.As(err, &notFound) {
		return false, nil
	}
	return false, fmt.Errorf("get role: %w", err)
}

// instanceProfile wraps the spawn role so EC2 can be launched with it. spawn passes
// the profile name when it launches a session's GPU.
type instanceProfile struct {
	api         iamAPI
	profileName string
	roleName    string
}

func (p *instanceProfile) kind() string { return "instance profile" }
func (p *instanceProfile) name() string { return p.profileName }

func (p *instanceProfile) ensure(ctx context.Context) (Action, error) {
	act := Action{Kind: p.kind(), Name: p.profileName}

	cur, err := p.get(ctx)
	if err != nil {
		return act, err
	}
	if cur == nil {
		if _, err := p.api.CreateInstanceProfile(ctx, &iam.CreateInstanceProfileInput{
			InstanceProfileName: aws.String(p.profileName),
			Tags:                iamTags(Tags(p.profileName)),
		}); err != nil {
			var exists *iamtypes.EntityAlreadyExistsException
			if !errors.As(err, &exists) {
				return act, fmt.Errorf("create instance profile: %w", err)
			}
		}
		act.Op = OpCreate
	} else {
		act.Op = OpExists
	}

	// AddRoleToInstanceProfile errors if the profile already holds a role — and a
	// profile holds at most one — so only add when it is not already there.
	if cur == nil || !profileHasRole(cur, p.roleName) {
		if _, err := p.api.AddRoleToInstanceProfile(ctx, &iam.AddRoleToInstanceProfileInput{
			InstanceProfileName: aws.String(p.profileName),
			RoleName:            aws.String(p.roleName),
		}); err != nil {
			var limit *iamtypes.LimitExceededException
			if errors.As(err, &limit) {
				return act, fmt.Errorf("instance profile %s already holds a different role; remove it first: %w", p.profileName, err)
			}
			return act, fmt.Errorf("add role %s to instance profile: %w", p.roleName, err)
		}
	}
	act.Detail = "role " + p.roleName
	return act, nil
}

// remove takes the role out of the profile before deleting it — DeleteInstanceProfile
// fails while a role is still attached.
func (p *instanceProfile) remove(ctx context.Context) (Action, error) {
	act := Action{Kind: p.kind(), Name: p.profileName}
	cur, err := p.get(ctx)
	if err != nil {
		return act, err
	}
	if cur == nil {
		act.Op = OpAbsent
		return act, nil
	}
	// Remove whatever roles are actually in there, not just the one we expect: a
	// drifted profile must still come off.
	for _, r := range cur.Roles {
		if _, err := p.api.RemoveRoleFromInstanceProfile(ctx, &iam.RemoveRoleFromInstanceProfileInput{
			InstanceProfileName: aws.String(p.profileName),
			RoleName:            r.RoleName,
		}); err != nil {
			return act, fmt.Errorf("remove role %s from instance profile: %w", aws.ToString(r.RoleName), err)
		}
	}
	if _, err := p.api.DeleteInstanceProfile(ctx, &iam.DeleteInstanceProfileInput{
		InstanceProfileName: aws.String(p.profileName),
	}); err != nil {
		return act, fmt.Errorf("delete instance profile: %w", err)
	}
	act.Op = OpDelete
	return act, nil
}

func (p *instanceProfile) get(ctx context.Context) (*iamtypes.InstanceProfile, error) {
	out, err := p.api.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{
		InstanceProfileName: aws.String(p.profileName),
	})
	if err != nil {
		var notFound *iamtypes.NoSuchEntityException
		if errors.As(err, &notFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("get instance profile: %w", err)
	}
	return out.InstanceProfile, nil
}

func profileHasRole(p *iamtypes.InstanceProfile, roleName string) bool {
	for _, r := range p.Roles {
		if aws.ToString(r.RoleName) == roleName {
			return true
		}
	}
	return false
}

func iamTags(m map[string]string) []iamtypes.Tag {
	out := make([]iamtypes.Tag, 0, len(m))
	for k, v := range m {
		out = append(out, iamtypes.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out
}

// decodePolicyDocument parses a policy document as IAM returns it. GetRole
// URL-encodes the document, so a test comparing it to what was sent has to decode
// first — a detail that silently breaks such comparisons otherwise.
func decodePolicyDocument(s string) (policyDoc, error) {
	if dec, err := url.QueryUnescape(s); err == nil {
		s = dec
	}
	var d policyDoc
	if err := json.Unmarshal([]byte(s), &d); err != nil {
		return d, fmt.Errorf("parse policy document: %w", err)
	}
	return d, nil
}
