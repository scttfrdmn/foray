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
	"os"
	"strings"
	"testing"
)

// The two deployment paths must stay equivalent.
//
// `foray deploy` is primary and deploy/terraform/ is the documented alternate
// (issue #85), which means the same account can be built either way — and a
// permission granted by one path but not the other is a bug that only shows up as
// "it works when I deploy with Terraform". They are maintained in parallel today,
// so this is the guard against that drift.
//
// Deliberately compares the *permission surface* (IAM actions, role names) rather
// than doing a structural diff: the two express the same intent in different
// syntaxes, so matching text would be brittle, while an action granted in one and
// not the other is unambiguously wrong.

const iamTerraformPath = "../../deploy/terraform/iam.tf"

func readIAMTerraform(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(iamTerraformPath)
	if err != nil {
		t.Fatalf("read %s: %v", iamTerraformPath, err)
	}
	return string(b)
}

// Every IAM action this package grants must also be granted by the Terraform path.
// A new action here that is missing there means the alternate path deploys a
// control plane that cannot do its job.
func TestNoActionGrantedOnlyByTheGoPath(t *testing.T) {
	tf := readIAMTerraform(t)
	f := newFakeIAM()
	tableARN := sessionsTableARN("us-west-2", testAccount, "foray-sessions")
	roles := []*role{
		gatewayRole(f, tableARN),
		webAPIRole(f, tableARN, "foray-data", testAccount, DefaultPlanModelID),
		spawnRole(f, "foray-data", testAccount),
	}

	for _, r := range roles {
		for _, p := range r.inline {
			doc := mustParse(t, p.doc)
			for _, st := range doc.Statement {
				for _, action := range stringsOf(t, st.Action) {
					if !strings.Contains(tf, `"`+action+`"`) {
						t.Errorf("%s/%s grants %q, which deploy/terraform/iam.tf does not — the two paths have drifted",
							r.roleName, p.name, action)
					}
				}
			}
		}
	}
}

// And the reverse: an action the Terraform grants but this package does not means
// `foray deploy` produces an under-permissioned control plane — the failure that
// surfaces as a mysterious AccessDenied at run time.
func TestNoActionGrantedOnlyByTerraform(t *testing.T) {
	tf := readIAMTerraform(t)
	f := newFakeIAM()
	tableARN := sessionsTableARN("us-west-2", testAccount, "foray-sessions")

	var granted strings.Builder
	for _, r := range []*role{
		gatewayRole(f, tableARN),
		webAPIRole(f, tableARN, "foray-data", testAccount, DefaultPlanModelID),
		spawnRole(f, "foray-data", testAccount),
	} {
		granted.WriteString(r.assumeDoc)
		for _, p := range r.inline {
			granted.WriteString(p.doc)
		}
		for _, m := range r.managed {
			granted.WriteString(m)
		}
	}
	goDoc := granted.String()

	// Pull every "service:Action" literal out of the .tf. The prefixes are the
	// services foray's roles actually touch; a new service in the .tf would need
	// adding here, which is itself a useful prompt to check both paths.
	for _, action := range extractTerraformActions(tf) {
		if !strings.Contains(goDoc, action) {
			t.Errorf("deploy/terraform/iam.tf grants %q, which internal/deploy does not — `foray deploy` would be under-permissioned",
				action)
		}
	}
}

// The role names must match, or the two paths create two different sets of roles and
// a teardown by one leaves the other's behind.
func TestRoleNamesMatchTerraform(t *testing.T) {
	tf := readIAMTerraform(t)
	for _, name := range []string{RoleGatewayLambda, RoleWebAPILambda, RoleSpawnInstance} {
		if !strings.Contains(tf, `"`+name+`"`) {
			t.Errorf("role %q is not in deploy/terraform/iam.tf — the paths would create different roles", name)
		}
	}
}

// extractTerraformActions finds quoted "service:Action" strings in the .tf.
func extractTerraformActions(tf string) []string {
	services := []string{"dynamodb:", "bedrock:", "ec2:", "pricing:", "s3:", "iam:", "sts:"}
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(tf, "\n") {
		trimmed := strings.TrimSpace(line)
		// Skip comments so prose mentioning an action is not read as a grant.
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		for _, svc := range services {
			idx := 0
			for {
				i := strings.Index(trimmed[idx:], `"`+svc)
				if i < 0 {
					break
				}
				start := idx + i + 1
				end := strings.Index(trimmed[start:], `"`)
				if end < 0 {
					break
				}
				action := trimmed[start : start+end]
				if !seen[action] && strings.Count(action, ":") == 1 && !strings.Contains(action, " ") {
					seen[action] = true
					out = append(out, action)
				}
				idx = start + end
			}
		}
	}
	return out
}
