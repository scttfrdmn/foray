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

package brain

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// converseAPI is the slice of the Bedrock Runtime client the invoker needs.
// Narrowing it to one method keeps the SDK surface small and the invoker
// trivially fakeable in tests without standing up a real client.
type converseAPI interface {
	Converse(ctx context.Context, in *bedrockruntime.ConverseInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error)
}

// BedrockInvoker calls Bedrock Runtime's Converse against a US inference profile
// (e.g. "us.anthropic.claude-..."). The Converse API has one request/response
// shape across model families, which keeps swapping the planning model a config
// change. Temperature is intentionally omitted: recent Claude models reject it
// in the request body, and maxTokens alone is sufficient for planning.
type BedrockInvoker struct {
	client    converseAPI
	modelID   string
	maxTokens int32
}

// DefaultPlanMaxTokens bounds the planning response. A ladder of two or three
// rungs with nnsight fits comfortably; this caps a runaway generation.
const DefaultPlanMaxTokens = 2048

// NewBedrockInvoker builds an invoker over a configured Bedrock Runtime client.
// modelID is a US inference profile id for the planning model.
func NewBedrockInvoker(client *bedrockruntime.Client, modelID string) *BedrockInvoker {
	return &BedrockInvoker{client: client, modelID: modelID, maxTokens: DefaultPlanMaxTokens}
}

// Converse implements Invoker: one Converse round-trip returning the model's
// text. System and prompt are sent as the system block and a single user turn.
func (b *BedrockInvoker) Converse(ctx context.Context, system, prompt string) (string, error) {
	out, err := b.client.Converse(ctx, &bedrockruntime.ConverseInput{
		ModelId: aws.String(b.modelID),
		System:  []types.SystemContentBlock{&types.SystemContentBlockMemberText{Value: system}},
		Messages: []types.Message{{
			Role:    types.ConversationRoleUser,
			Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: prompt}},
		}},
		// temperature deliberately absent — recent Claude models reject it.
		InferenceConfig: &types.InferenceConfiguration{MaxTokens: aws.Int32(b.maxTokens)},
	})
	if err != nil {
		return "", fmt.Errorf("bedrock converse %s: %w", b.modelID, err)
	}
	return converseText(out)
}

// converseText extracts the assistant's text from a Converse response. The
// output is a tagged union; the planner needs the text member.
func converseText(out *bedrockruntime.ConverseOutput) (string, error) {
	msg, ok := out.Output.(*types.ConverseOutputMemberMessage)
	if !ok {
		return "", fmt.Errorf("bedrock converse: unexpected output shape %T", out.Output)
	}
	for _, block := range msg.Value.Content {
		if t, ok := block.(*types.ContentBlockMemberText); ok {
			return t.Value, nil
		}
	}
	return "", fmt.Errorf("bedrock converse: response carried no text block")
}
