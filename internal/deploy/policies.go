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

// The three roles, least-privilege, mirroring deploy/terraform/iam.tf statement
// for statement (issue #54, documented in deploy/terraform/README.md):
//
//	foray-gateway-lambda  forayd: the sessions table, nothing else. It routes
//	                      graphs and stamps last_request_time; it never plans,
//	                      prices, launches or reads saves.
//	foray-webapi-lambda   the page's API: the table, Bedrock (to plan), read-only
//	                      Spot pricing (truffle), the data bucket's sessions/
//	                      prefix (to presign exports), and PassRole for the spawn
//	                      role (to launch a session's GPU).
//	foray-spawn-instance  the GPU itself: write its saves under sessions/, and
//	                      terminate/stop *itself*.
//
// Each grant below is scoped as tightly as the API allows, and where it cannot be
// scoped by ARN it is scoped by condition. The comments record why, because "why is
// this Resource *" is the question a reader will have.

// sessionsTablePolicy is the only data access forayd gets: the four operations
// gateway.DynamoStore actually performs on one table.
//
// No Scan and no DeleteItem: Touch is an UpdateItem, receipts are a Query, and
// rows expire by TTL rather than being deleted. /healthz deliberately does not
// enumerate (internal/gateway/dynamo.go omits the enumerator capability precisely
// so no Scan is needed).
func sessionsTablePolicy(tableARN string) inlinePolicy {
	return inlinePolicy{
		name: "sessions-table",
		doc: mustJSON(policyDoc{
			Version: policyVersion,
			Statement: []statement{{
				Effect: "Allow",
				Action: []string{
					"dynamodb:GetItem",
					"dynamodb:PutItem",
					"dynamodb:UpdateItem",
					"dynamodb:Query",
				},
				Resource: tableARN,
			}},
		}),
	}
}

// bedrockInvokePolicy lets the web API's brain plan and interpret.
//
// Two resources are needed because invoking *through* an inference profile fans out
// to the underlying foundation models: the profile ARN alone is denied at the
// model, and the model ARN alone is denied at the profile. Both regions are
// wildcards because a US inference profile routes across them — pinning one would
// deny the call the moment Bedrock served it elsewhere.
func bedrockInvokePolicy(accountID, planModelID string) inlinePolicy {
	return inlinePolicy{
		name: "bedrock-invoke",
		doc: mustJSON(policyDoc{
			Version: policyVersion,
			Statement: []statement{{
				Effect: "Allow",
				Action: []string{
					"bedrock:InvokeModel",
					"bedrock:InvokeModelWithResponseStream",
				},
				Resource: []string{
					inferenceProfileARN(accountID, planModelID),
					foundationModelARN(),
				},
			}},
		}),
	}
}

// truffleSpotPricingPolicy backs every cost number foray shows.
//
// Resource is "*" because these are account-wide read-only describes with no ARN to
// scope to — there is no such thing as "describe the price of one instance type"
// as a resource. All five are read-only: nothing here can launch, modify or spend.
func truffleSpotPricingPolicy() inlinePolicy {
	return inlinePolicy{
		name: "truffle-spot-pricing",
		doc: mustJSON(policyDoc{
			Version: policyVersion,
			Statement: []statement{{
				Effect: "Allow",
				Action: []string{
					"ec2:DescribeSpotPriceHistory",
					"ec2:DescribeInstanceTypes",
					"ec2:DescribeInstanceTypeOfferings",
					"ec2:DescribeRegions",
					"pricing:GetProducts",
				},
				Resource: "*",
			}},
		}),
	}
}

// dataBucketSessionsPolicy scopes the web API to the session prefix only.
//
// The ListBucket statement is separate and condition-scoped because ListBucket is a
// *bucket* action — its resource is the bucket, not the objects — so without the
// `s3:prefix` condition it would grant a listing of everything in the bucket. The
// object statement is prefix-scoped by ARN. Together: read and write under
// sessions/, list under sessions/, nothing else.
func dataBucketSessionsPolicy(dataBucket string) inlinePolicy {
	arn := bucketARN(dataBucket)
	return inlinePolicy{
		name: "data-bucket-sessions",
		doc: mustJSON(policyDoc{
			Version: policyVersion,
			Statement: []statement{
				{
					Effect:   "Allow",
					Action:   []string{"s3:GetObject", "s3:PutObject"},
					Resource: arn + "/sessions/*",
				},
				{
					Effect:   "Allow",
					Action:   []string{"s3:ListBucket"},
					Resource: arn,
					Condition: map[string]any{
						"StringLike": map[string]any{"s3:prefix": []string{"sessions/*"}},
					},
				},
			},
		}),
	}
}

// passSpawnRolePolicy lets the web API launch a session's GPU with the spawn role
// attached.
//
// PassRole is the privilege-escalation-shaped permission in this set: whoever can
// pass a role can give it to a service. It is narrowed twice — to exactly the spawn
// role's ARN, and by condition to EC2 — so it cannot be used to hand that role, or
// any other, to anything else.
func passSpawnRolePolicy(accountID string) inlinePolicy {
	return inlinePolicy{
		name: "pass-spawn-role",
		doc: mustJSON(policyDoc{
			Version: policyVersion,
			Statement: []statement{{
				Effect:   "Allow",
				Action:   []string{"iam:PassRole"},
				Resource: roleARN(accountID, RoleSpawnInstance),
				Condition: map[string]any{
					"StringEquals": map[string]any{"iam:PassedToService": "ec2.amazonaws.com"},
				},
			}},
		}),
	}
}

// spawnSessionSavesPolicy is what the worker on the GPU gets: write its
// activations under the session prefix, and read them back. No ListBucket — the
// worker writes keys it already knows, so it never needs to enumerate, and not
// granting it means a compromised worker cannot inventory other sessions.
func spawnSessionSavesPolicy(dataBucket string) inlinePolicy {
	return inlinePolicy{
		name: "session-saves",
		doc: mustJSON(policyDoc{
			Version: policyVersion,
			Statement: []statement{{
				Effect:   "Allow",
				Action:   []string{"s3:GetObject", "s3:PutObject"},
				Resource: bucketARN(dataBucket) + "/sessions/*",
			}},
		}),
	}
}

// spawnSelfTerminatePolicy lets the instance end its own life — the in-instance
// idle/TTL daemon is what enforces ephemerality, so it must be able to act.
//
// It cannot be scoped by instance ARN, because the instance does not know its own
// id when the policy is written. So it is scoped by **tag**: only instances tagged
// Project=foray can be stopped or terminated. That is the same tag teardown-verify
// asserts on, which makes the blast radius exactly "foray's own instances".
func spawnSelfTerminatePolicy(accountID string) inlinePolicy {
	return inlinePolicy{
		name: "self-terminate-foray-tagged",
		doc: mustJSON(policyDoc{
			Version: policyVersion,
			Statement: []statement{{
				Effect:   "Allow",
				Action:   []string{"ec2:TerminateInstances", "ec2:StopInstances"},
				Resource: instanceARN(accountID),
				Condition: map[string]any{
					"StringEquals": map[string]any{
						"aws:ResourceTag/" + TagProject: TagProjectValue,
					},
				},
			}},
		}),
	}
}

// --- role definitions -------------------------------------------------------

// gatewayRole is forayd's execution role: logs, and one table.
func gatewayRole(api iamAPI, tableARN string) *role {
	return &role{
		api:         api,
		roleName:    RoleGatewayLambda,
		assumeDoc:   assumeRolePolicy("lambda.amazonaws.com"),
		managed:     []string{lambdaBasicExecutionPolicyARN},
		inline:      []inlinePolicy{sessionsTablePolicy(tableARN)},
		description: "foray gateway (forayd) Lambda: routes intervention graphs and stamps last_request_time",
	}
}

// webAPIRole is the page's API role: it plans, prices, presigns and launches, so it
// is the widest of the three — and every grant is scoped in the policy above.
func webAPIRole(api iamAPI, tableARN, dataBucket, accountID, planModelID string) *role {
	return &role{
		api:       api,
		roleName:  RoleWebAPILambda,
		assumeDoc: assumeRolePolicy("lambda.amazonaws.com"),
		managed:   []string{lambdaBasicExecutionPolicyARN},
		inline: []inlinePolicy{
			sessionsTablePolicy(tableARN),
			bedrockInvokePolicy(accountID, planModelID),
			truffleSpotPricingPolicy(),
			dataBucketSessionsPolicy(dataBucket),
			passSpawnRolePolicy(accountID),
		},
		description: "foray web API Lambda: plan (Bedrock), price (truffle), presign exports, launch sessions",
	}
}

// spawnRole is attached to the session's GPU instance.
func spawnRole(api iamAPI, dataBucket, accountID string) *role {
	return &role{
		api:       api,
		roleName:  RoleSpawnInstance,
		assumeDoc: assumeRolePolicy("ec2.amazonaws.com"),
		inline: []inlinePolicy{
			spawnSessionSavesPolicy(dataBucket),
			spawnSelfTerminatePolicy(accountID),
		},
		description: "foray session GPU instance: write saves under sessions/, self-terminate",
	}
}
