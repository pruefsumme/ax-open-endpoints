// Copyright 2026 Google LLC
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

package workspace

import (
	"context"
	"fmt"

	"github.com/google/ax/internal/model"
	"github.com/google/ax/internal/store"
	"github.com/google/ax/pkg/apis/v1alpha1"
)

// EnvironmentPlan represents the resolved environment and tool setup for a workspace.
type EnvironmentPlan struct {
	WorkspaceName string                 `json:"workspaceName"`
	Goal          string                 `json:"goal"`
	ModelUsed     string                 `json:"modelUsed"`
	PlanSummary   string                 `json:"planSummary"`
	GitRepos      []*v1alpha1.GitRepo    `json:"gitRepos,omitempty"`
	MCPConfig     *v1alpha1.MCPConfig    `json:"mcpConfig,omitempty"`
	Skills        *v1alpha1.SkillsConfig `json:"skills,omitempty"`
}

// Planner resolves and prepares workspace environments using a model client.
type Planner struct {
	client *model.Client
}

// NewPlanner creates a new Workspace planner backed by a model client.
func NewPlanner(client *model.Client) *Planner {
	if client == nil {
		client = model.NewDefaultClient()
	}
	return &Planner{
		client: client,
	}
}

// NewPlannerWithConfig creates a new Workspace planner using the provided model.Config.
func NewPlannerWithConfig(cfg model.Config, opts ...model.Option) *Planner {
	return NewPlanner(model.NewClient(cfg, opts...))
}

// NewPlannerFromStore creates a Workspace planner by loading the "default-model" configuration from the store.
func NewPlannerFromStore(ctx context.Context, s store.Store, atespace string, opts ...model.Option) (*Planner, error) {
	client, err := model.NewDefaultClientFromStore(ctx, s, atespace, opts...)
	if err != nil {
		return nil, err
	}
	return NewPlanner(client), nil
}

// PlanEnvironment analyzes the declared goal, workspace repositories, MCP servers,
// and skills to synthesize execution setup instructions.
func (p *Planner) PlanEnvironment(ctx context.Context, ws *v1alpha1.Workspace, goal string) (*EnvironmentPlan, error) {
	if ws == nil {
		return nil, fmt.Errorf("workspace cannot be nil")
	}

	wsName := ""
	if ws.Metadata != nil {
		wsName = ws.Metadata.Name
	}
	var gitRepos []*v1alpha1.GitRepo
	var mcpConfig *v1alpha1.MCPConfig
	var skills *v1alpha1.SkillsConfig
	if ws.Spec != nil {
		gitRepos = ws.Spec.Git
		mcpConfig = ws.Spec.Mcp
		skills = ws.Spec.Skills
	}

	prompt := fmt.Sprintf(
		"Synthesize an environment bootstrap and toolchain plan for workspace %q with goal %q. Git repositories: %d, MCP servers: %v",
		wsName,
		goal,
		len(gitRepos),
		mcpConfig != nil,
	)

	// Leave Model empty so Generate uses the model from its configured Model resource.
	resp, err := p.client.Generate(ctx, &model.GenerateRequest{
		Prompt:            prompt,
		SystemInstruction: "You are the AX system workspace environment planner. Output concise bootstrap plans for agent workspaces.",
	})
	if err != nil {
		return nil, fmt.Errorf("generating environment plan for workspace %s: %w", wsName, err)
	}

	return &EnvironmentPlan{
		WorkspaceName: wsName,
		Goal:          goal,
		ModelUsed:     resp.Model,
		PlanSummary:   resp.Content,
		GitRepos:      gitRepos,
		MCPConfig:     mcpConfig,
		Skills:        skills,
	}, nil
}
