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

// Package deploy provisions foray's ~$0 control plane directly through the AWS
// SDK — the primary deployment path (issue #85).
//
// Why a verb instead of only IaC: foray's pitch is "all you need is an AWS
// account", and requiring Terraform makes that two things. `deploy/terraform/`
// remains as the documented alternate for anyone who wants declarative infra, and
// this package mirrors it resource for resource so the two stay equivalent. The
// shape follows lagotto, the spore.host tool in the same position: a `deploy` verb
// as the primary path, declarative templates as the alternate.
//
// # No state file
//
// Terraform remembers what it made; this package does not. Every resource is
// therefore *discovered* before it is created, and creating something that
// already exists is a no-op rather than an error. That makes Apply re-runnable,
// which matters more here than it does for Terraform: without state, a half-
// finished deploy can only be fixed by running it again.
//
// # Teardown is the load-bearing half
//
// A deploy verb that cannot fully undo itself is worse than Terraform, because
// the thing left behind bills money and nothing records that it exists. So every
// resource carries Project=foray (scripts/teardown-verify.sh asserts on exactly
// that tag via the Resource Groups Tagging API), and Teardown removes what Apply
// made. Tagging is per-call here, where Terraform got it free from
// `default_tags` — an untagged resource is invisible to teardown-verify, which is
// the one way this can silently fail.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// TagProject is the tag every foray resource carries. scripts/teardown-verify.sh
// queries the Resource Groups Tagging API for exactly this key/value, so a
// resource created without it is invisible to the "nothing is left billing" check.
const (
	TagProject      = "Project"
	TagProjectValue = "foray"
	TagManagedBy    = "ManagedBy"
	// Distinguishes what this verb made from what Terraform made, so a stack
	// deployed one way is recognizable when torn down the other.
	TagManagedByValue = "foray-cli"
)

// Tags is the tag set applied to every resource, plus a per-resource Name.
func Tags(name string) map[string]string {
	return map[string]string{
		TagProject:   TagProjectValue,
		TagManagedBy: TagManagedByValue,
		"Name":       name,
	}
}

// Config is the deployment's inputs. It mirrors deploy/terraform/variables.tf so
// the two paths take the same decisions; defaults are applied by Validate.
type Config struct {
	Region string

	// WebBucket serves the SPA through CloudFront (OAC-only, never public).
	// DataBucket holds the user's in-region saves/outputs/exports. Both are
	// globally unique names, so they have no default — the deployer must choose.
	WebBucket  string
	DataBucket string

	// SessionsTable is the session<->instance map plus per-question cost receipts.
	SessionsTable string
}

// DefaultSessionsTable matches deploy/terraform/variables.tf.
const DefaultSessionsTable = "foray-sessions"

// ErrConfig marks a configuration problem — reported before any AWS call, so a
// bad deploy costs nothing.
var ErrConfig = errors.New("deploy: invalid configuration")

// Validate fills defaults and rejects a config that cannot work. It runs before
// anything is created: a deploy that fails halfway leaves resources behind, so
// the cheapest failure is the one that happens first.
func (c *Config) Validate() error {
	if c.SessionsTable == "" {
		c.SessionsTable = DefaultSessionsTable
	}
	var missing []string
	if strings.TrimSpace(c.Region) == "" {
		missing = append(missing, "Region")
	}
	if strings.TrimSpace(c.WebBucket) == "" {
		missing = append(missing, "WebBucket")
	}
	if strings.TrimSpace(c.DataBucket) == "" {
		missing = append(missing, "DataBucket")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s required", ErrConfig, strings.Join(missing, ", "))
	}
	if c.WebBucket == c.DataBucket {
		// They have different access postures — the web bucket is readable by
		// CloudFront, the data bucket holds the user's activations — so sharing one
		// bucket would quietly widen access to saved values.
		return fmt.Errorf("%w: WebBucket and DataBucket must differ", ErrConfig)
	}
	return nil
}

// Op is what happened (or would happen) to one resource.
type Op string

const (
	OpCreate Op = "create" // did not exist; created
	OpExists Op = "exists" // already present; left alone
	OpDelete Op = "delete" // removed
	OpAbsent Op = "absent" // nothing to remove
	OpPlan   Op = "plan"   // dry run: what Apply would do
)

// Action is one line of the deploy's report. The CLI prints these; tests assert
// on them. Deliberately flat and printable rather than a resource graph — the
// user's question is "what did you do to my account", and a list answers it.
type Action struct {
	Kind   string // "dynamodb table", "s3 bucket", ...
	Name   string // the resource's name/id
	Op     Op
	Detail string // optional note, e.g. why something was skipped
}

func (a Action) String() string {
	s := fmt.Sprintf("%-8s %-16s %s", a.Op, a.Kind, a.Name)
	if a.Detail != "" {
		s += "  (" + a.Detail + ")"
	}
	return s
}

// resource is one provisionable thing. Splitting the work this way keeps each
// resource's idempotency rule next to its API calls, and lets Apply and Teardown
// share one ordering.
type resource interface {
	// kind labels the resource in the report.
	kind() string
	// name is the resource's identifier.
	name() string
	// ensure creates the resource if absent and returns what it did. It must be
	// safe to call on an existing resource.
	ensure(ctx context.Context) (Action, error)
	// remove deletes the resource, reporting OpAbsent when there is nothing to
	// delete. It must be safe to call twice.
	remove(ctx context.Context) (Action, error)
}

// Deployer applies and tears down the control plane.
type Deployer struct {
	cfg Config
	// resources in dependency order: Apply walks them forward, Teardown backward.
	resources []resource
	// DryRun reports what Apply would do without calling a mutating API.
	DryRun bool
}

// Config returns the validated configuration.
func (d *Deployer) Config() Config { return d.cfg }

// Apply creates everything that is missing, in dependency order, and returns one
// Action per resource.
//
// It stops at the first failure rather than pressing on: later resources
// generally depend on earlier ones, so continuing would pile a second, confusing
// error on top of the real one. The actions completed so far are returned
// alongside the error, because the caller needs to know what already exists —
// there is no state file to consult.
func (d *Deployer) Apply(ctx context.Context) ([]Action, error) {
	out := make([]Action, 0, len(d.resources))
	for _, r := range d.resources {
		if d.DryRun {
			out = append(out, Action{Kind: r.kind(), Name: r.name(), Op: OpPlan})
			continue
		}
		a, err := r.ensure(ctx)
		if err != nil {
			return out, fmt.Errorf("deploy %s %s: %w", r.kind(), r.name(), err)
		}
		out = append(out, a)
	}
	return out, nil
}

// Teardown removes everything, in reverse dependency order.
//
// Unlike Apply it does *not* stop at the first failure: the requirement is that
// nothing is left billing, so one stubborn resource must not prevent the rest
// from being deleted. Errors are collected and returned together, and
// scripts/teardown-verify.sh is the independent check that the account is
// actually clean.
func (d *Deployer) Teardown(ctx context.Context) ([]Action, error) {
	out := make([]Action, 0, len(d.resources))
	var errs []error
	for i := len(d.resources) - 1; i >= 0; i-- {
		r := d.resources[i]
		if d.DryRun {
			out = append(out, Action{Kind: r.kind(), Name: r.name(), Op: OpPlan})
			continue
		}
		a, err := r.remove(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("teardown %s %s: %w", r.kind(), r.name(), err))
			out = append(out, Action{Kind: r.kind(), Name: r.name(), Op: OpDelete, Detail: "FAILED"})
			continue
		}
		out = append(out, a)
	}
	return out, errors.Join(errs...)
}
