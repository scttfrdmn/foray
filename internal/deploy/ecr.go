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
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
)

// ecrAPI is the slice of ECR this package uses.
type ecrAPI interface {
	DescribeRepositories(ctx context.Context, in *ecr.DescribeRepositoriesInput, opts ...func(*ecr.Options)) (*ecr.DescribeRepositoriesOutput, error)
	CreateRepository(ctx context.Context, in *ecr.CreateRepositoryInput, opts ...func(*ecr.Options)) (*ecr.CreateRepositoryOutput, error)
	DeleteRepository(ctx context.Context, in *ecr.DeleteRepositoryInput, opts ...func(*ecr.Options)) (*ecr.DeleteRepositoryOutput, error)
	PutLifecyclePolicy(ctx context.Context, in *ecr.PutLifecyclePolicyInput, opts ...func(*ecr.Options)) (*ecr.PutLifecyclePolicyOutput, error)
}

// workerRepo holds the nnsight worker image the session's GPU instance runs.
//
// It exists because nothing else put the worker on the instance: foray launched a bare
// AL2023 box and ran `python3 -m worker.batch` on it, which has no torch, no nnsight and
// no `worker` package (issue #103). `spawn task run` pulls a container image and
// auto-selects the GPU driver AMI, so the missing piece was somewhere to keep the image.
//
// Of everything this package creates, this is the one resource that grows without bound:
// every `make worker-push` adds a few GB of layers, and nothing removes the old ones. So a
// lifecycle policy expires what is no longer referenced — a repository left to accumulate
// is the "control plane rests at ~$0" invariant leaking, slowly.
type workerRepo struct {
	api  ecrAPI
	repo string
}

// DefaultWorkerRepo is the repository name, matching the Makefile's WORKER_REPO.
const DefaultWorkerRepo = "foray-worker"

// untaggedExpiryDays bounds how long an unreferenced layer set lingers. A day is enough
// to notice a bad push and roll back to it, and short enough that the repository does not
// quietly become the largest line on the bill.
const untaggedExpiryDays = 1

// keepRecentImages is how many tagged images to retain. The instance pulls one tag, so
// history here is only for rolling back; a handful is plenty.
const keepRecentImages = 5

func (r *workerRepo) kind() string { return "ecr repository" }
func (r *workerRepo) name() string { return r.repo }

func (r *workerRepo) ensure(ctx context.Context) (Action, error) {
	act := Action{Kind: r.kind(), Name: r.repo}

	existing, err := r.describe(ctx)
	if err != nil {
		return act, err
	}
	if existing == nil {
		_, err := r.api.CreateRepository(ctx, &ecr.CreateRepositoryInput{
			RepositoryName: aws.String(r.repo),
			// Mutable so `:dev` can be re-pushed during development. The alternative
			// (immutable tags) would make every iteration invent a new tag, and the
			// instance is launched with whatever tag the control plane was told about.
			ImageTagMutability: ecrtypes.ImageTagMutabilityMutable,
			// Basic scanning is free and on-push, so there is no reason to decline it.
			ImageScanningConfiguration: &ecrtypes.ImageScanningConfiguration{ScanOnPush: true},
			Tags:                       ecrTags(Tags(r.repo)),
		})
		if err != nil {
			return act, fmt.Errorf("create repository: %w", err)
		}
		act.Op = OpCreate
		act.Detail = fmt.Sprintf("scan on push, untagged expire after %dd", untaggedExpiryDays)
	} else {
		act.Op = OpExists
	}

	// Converge the lifecycle policy either way: a repository created by an older deploy
	// (or by hand) would otherwise keep every layer forever.
	if err := r.ensureLifecycle(ctx); err != nil {
		return act, err
	}
	return act, nil
}

func (r *workerRepo) remove(ctx context.Context) (Action, error) {
	act := Action{Kind: r.kind(), Name: r.repo}
	existing, err := r.describe(ctx)
	if err != nil {
		return act, err
	}
	if existing == nil {
		act.Op = OpAbsent
		return act, nil
	}
	// Force, because a repository with images in it cannot be deleted otherwise — and
	// leaving one behind is exactly the storage charge teardown exists to end. The image
	// is a build artifact `make worker-push` recreates, not the user's data.
	if _, err := r.api.DeleteRepository(ctx, &ecr.DeleteRepositoryInput{
		RepositoryName: aws.String(r.repo),
		Force:          true,
	}); err != nil {
		return act, fmt.Errorf("delete repository: %w", err)
	}
	act.Op = OpDelete
	act.Detail = "including its images"
	return act, nil
}

// describe returns the repository, or nil when it does not exist.
func (r *workerRepo) describe(ctx context.Context) (*ecrtypes.Repository, error) {
	out, err := r.api.DescribeRepositories(ctx, &ecr.DescribeRepositoriesInput{
		RepositoryNames: []string{r.repo},
	})
	if err != nil {
		var notFound *ecrtypes.RepositoryNotFoundException
		if errors.As(err, &notFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("describe repository: %w", err)
	}
	if len(out.Repositories) == 0 {
		return nil, nil
	}
	return &out.Repositories[0], nil
}

// ensureLifecycle installs the expiry rules. PutLifecyclePolicy replaces wholesale, so it
// is idempotent by construction.
func (r *workerRepo) ensureLifecycle(ctx context.Context) error {
	if _, err := r.api.PutLifecyclePolicy(ctx, &ecr.PutLifecyclePolicyInput{
		RepositoryName:      aws.String(r.repo),
		LifecyclePolicyText: aws.String(workerRepoLifecyclePolicy()),
	}); err != nil {
		return fmt.Errorf("put lifecycle policy: %w", err)
	}
	return nil
}

// workerRepoLifecyclePolicy bounds the repository's size. Rule order matters to ECR: it
// applies rules by ascending priority, so untagged images are swept first and the
// keep-recent rule then trims history.
func workerRepoLifecyclePolicy() string {
	return fmt.Sprintf(`{
  "rules": [
    {
      "rulePriority": 1,
      "description": "expire untagged layers",
      "selection": {
        "tagStatus": "untagged",
        "countType": "sinceImagePushed",
        "countUnit": "days",
        "countNumber": %d
      },
      "action": {"type": "expire"}
    },
    {
      "rulePriority": 2,
      "description": "keep only recent tagged images",
      "selection": {
        "tagStatus": "any",
        "countType": "imageCountMoreThan",
        "countNumber": %d
      },
      "action": {"type": "expire"}
    }
  ]
}`, untaggedExpiryDays, keepRecentImages)
}

// WorkerImageURI is the image reference the control plane launches with and
// `make worker-push` pushes to. One function so the two cannot disagree — a mismatch is
// an instance that pulls nothing and a rung that fails after the GPU is already billing.
func (c Config) WorkerImageURI(tag string) string {
	if tag == "" {
		tag = DefaultWorkerTag
	}
	return fmt.Sprintf("%s.dkr.ecr.%s.amazonaws.com/%s:%s", c.AccountID, c.Region, c.WorkerRepo, tag)
}

// DefaultWorkerTag matches the Makefile's WORKER_TAG.
const DefaultWorkerTag = "dev"

// ecrTags converts the common tag map to ECR's tag shape.
func ecrTags(m map[string]string) []ecrtypes.Tag {
	out := make([]ecrtypes.Tag, 0, len(m))
	for k, v := range m {
		out = append(out, ecrtypes.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out
}
