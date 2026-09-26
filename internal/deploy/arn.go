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

import "fmt"

// ARNs are constructed rather than read back from each created resource.
//
// Terraform could reference `aws_dynamodb_table.sessions.arn` because it holds a
// graph; here, constructing the ARN from account + region + name removes an API
// round-trip *and* a dependency edge — the IAM policies below can be written
// before the resources they point at exist, which is the right order anyway (a
// role has to exist before the Lambda that assumes it).
//
// Partition is hardcoded `aws`, matching deploy/terraform/iam.tf, which does the
// same for the managed-policy and Bedrock ARNs. foray targets standard AWS; a
// GovCloud or China deployment would need the partition threaded through here and
// through the .tf files alike.
const partition = "aws"

func sessionsTableARN(region, accountID, table string) string {
	return fmt.Sprintf("arn:%s:dynamodb:%s:%s:table/%s", partition, region, accountID, table)
}

func bucketARN(bucket string) string {
	// S3 bucket ARNs carry neither region nor account.
	return fmt.Sprintf("arn:%s:s3:::%s", partition, bucket)
}

func roleARN(accountID, role string) string {
	return fmt.Sprintf("arn:%s:iam::%s:role/%s", partition, accountID, role)
}

// inferenceProfileARN is the Bedrock inference profile the brain plans with. The
// region is a wildcard on purpose: a US inference profile routes across regions,
// so pinning one would deny the call the moment Bedrock served it from another.
func inferenceProfileARN(accountID, modelID string) string {
	return fmt.Sprintf("arn:%s:bedrock:*:%s:inference-profile/%s", partition, accountID, modelID)
}

// foundationModelARN matches any foundation model. An inference profile fans out to
// the underlying foundation models, and those ARNs are account-less, so invoking
// through a profile needs both this and the profile ARN.
func foundationModelARN() string {
	return fmt.Sprintf("arn:%s:bedrock:*::foundation-model/*", partition)
}

// instanceARN matches every EC2 instance in the account. The spawn role's
// self-terminate permission is narrowed by a tag condition rather than by ARN,
// because the instance it must terminate is itself and its id is not known when
// the policy is written.
func instanceARN(accountID string) string {
	return fmt.Sprintf("arn:%s:ec2:*:%s:instance/*", partition, accountID)
}

// lambdaBasicExecutionPolicyARN is the AWS-managed policy that grants a Lambda its
// CloudWatch log group. Using the managed policy rather than hand-writing logs:*
// keeps it correct as AWS evolves it.
const lambdaBasicExecutionPolicyARN = "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"

func functionARN(region, accountID, funcName string) string {
	return fmt.Sprintf("arn:%s:lambda:%s:%s:function:%s", partition, region, accountID, funcName)
}

// executionARN is the API's invoke-permission scope. Note the service is
// `execute-api`, not `apigateway` — the latter names the control plane, and a
// permission scoped to it would never match an actual request.
func executionARN(region, accountID, apiID string) string {
	return fmt.Sprintf("arn:%s:execute-api:%s:%s:%s", partition, region, accountID, apiID)
}
