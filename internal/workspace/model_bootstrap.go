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
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/google/ax/internal/model"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"gopkg.in/yaml.v3"
)

const (
	maxBootstrapTurns       = 24
	maxBootstrapToolCalls   = 48
	bootstrapCommandTimeout = 2 * time.Minute
	maxBootstrapOutput      = 32 * 1024
	maxBootstrapTranscript  = 128 * 1024
	maxBootstrapCommandSize = 16 * 1024
)

// modelBootstrapSelected keeps the default Google workspace setup on its
// existing Antigravity path. Other providers use the configured Model client.
func modelBootstrapSelected() bool {
	raw := os.Getenv(modelConfigEnv)
	if raw == "" {
		return false
	}

	var configured v1alpha1.Model
	if err := yaml.Unmarshal([]byte(raw), &configured); err != nil {
		return true
	}
	return model.ConfigFromCRD(&configured).EffectiveProtocol() != model.ProtocolGoogle
}

func runModelBootstrap(parent context.Context, goal, workspacePath string) bool {
	var configured v1alpha1.Model
	if err := yaml.Unmarshal([]byte(os.Getenv(modelConfigEnv)), &configured); err != nil {
		slog.Error("reading configured model for workspace bootstrap", "error", err)
		return false
	}

	cfg := model.ConfigFromCRD(&configured)
	cfg.APIKey = os.Getenv(modelAPIKeyEnv)
	// The controller resolves the secret before launching the task. The runner
	// should use that value directly instead of trying Kubernetes credentials.
	cfg.SecretKey = nil
	client := model.NewClient(cfg)

	timeout := bootstrapTimeout()
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	slog.Info("starting model-backed workspace bootstrap", "provider", cfg.Provider, "protocol", cfg.EffectiveProtocol(), "model", cfg.Model, "workspace", workspacePath, "timeout", timeout)

	messages := []model.Message{
		{
			Role: "system",
			Content: fmt.Sprintf(
				"You prepare an AX workspace for a task. The workspace directory is %q. "+
					"Use the run_command tool to inspect the project and make only the changes needed for the goal. "+
					"Run commands from the workspace directory, keep all project files there, and stop when setup is complete.",
				workspacePath,
			),
		},
		{Role: "user", Content: "Prepare this workspace for the following goal: " + goal},
	}
	tools := []model.ToolDefinition{{
		Name:        "run_command",
		Description: "Run one shell command with the workspace as its working directory. Use this to inspect files, install dependencies, or prepare the project. The command runs inside the task container and its output is returned to you.",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{"command":{"type":"string","description":"A shell command to run from the workspace directory."}},
			"required":["command"],
			"additionalProperties":false
		}`),
	}}

	toolCalls := 0
	transcriptBytes := 0
	for turn := 0; turn < maxBootstrapTurns; turn++ {
		response, err := client.Generate(ctx, &model.GenerateRequest{
			Messages:  messages,
			Tools:     tools,
			MaxTokens: 4096,
		})
		if err != nil {
			slog.Warn("model-backed workspace bootstrap request failed", "provider", cfg.Provider, "turn", turn+1, "error", err)
			return false
		}

		if response.Content != "" {
			slog.Info("workspace bootstrap response", "provider", cfg.Provider, "turn", turn+1, "content", response.Content)
		}
		if len(response.ToolCalls) == 0 {
			return strings.TrimSpace(response.Content) != ""
		}

		messages = append(messages, model.Message{
			Role:      "assistant",
			Content:   response.Content,
			ToolCalls: response.ToolCalls,
		})
		for _, call := range response.ToolCalls {
			toolCalls++
			if toolCalls > maxBootstrapToolCalls {
				slog.Warn("model-backed workspace bootstrap exceeded tool-call limit", "limit", maxBootstrapToolCalls)
				return false
			}
			if call.ID == "" {
				call.ID = fmt.Sprintf("workspace-setup-%d", toolCalls)
			}
			remaining := maxBootstrapTranscript - transcriptBytes
			if remaining <= 0 {
				slog.Warn("model-backed workspace bootstrap exceeded transcript limit", "limit", maxBootstrapTranscript)
				return false
			}
			outputLimit := maxBootstrapOutput
			if remaining < outputLimit {
				outputLimit = remaining
			}
			result := executeWorkspaceCommand(ctx, workspacePath, call, outputLimit)
			transcriptBytes += len(result)
			messages = append(messages, model.Message{
				Role:       "tool",
				ToolCallID: call.ID,
				ToolName:   call.Name,
				Content:    result,
			})
		}
	}

	slog.Warn("model-backed workspace bootstrap exceeded turn limit", "limit", maxBootstrapTurns)
	return false
}

func executeWorkspaceCommand(ctx context.Context, workspacePath string, call model.ToolCall, outputLimit int) string {
	if call.Name != "run_command" {
		return fmt.Sprintf("unknown tool %q; use run_command", call.Name)
	}
	var input struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(call.Arguments, &input); err != nil {
		return fmt.Sprintf("invalid run_command arguments: %v", err)
	}
	if input.Command == "" {
		return "run_command requires a non-empty command"
	}
	if len(input.Command) > maxBootstrapCommandSize {
		return fmt.Sprintf("run_command is limited to %d bytes", maxBootstrapCommandSize)
	}

	commandCtx, cancel := context.WithTimeout(ctx, bootstrapCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, "/bin/sh", "-c", input.Command)
	// This sets the working directory; it is not a filesystem jail. The task
	// container remains the isolation boundary for commands requested by a model.
	cmd.Dir = workspacePath
	output := &limitedOutput{limit: outputLimit}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Run(); err != nil {
		return fmt.Sprintf("command failed: %v\n%s", err, output.String())
	}
	return output.String()
}

type limitedOutput struct {
	mu        sync.Mutex
	output    strings.Builder
	limit     int
	truncated bool
}

func (w *limitedOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	remaining := w.limit - w.output.Len()
	if remaining > 0 {
		if len(p) > remaining {
			_, _ = w.output.Write(p[:remaining])
			w.truncated = true
		} else {
			_, _ = w.output.Write(p)
		}
	} else if len(p) > 0 {
		w.truncated = true
	}
	return len(p), nil
}

func (w *limitedOutput) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	result := w.output.String()
	if w.truncated {
		result += "\n[command output truncated]"
	}
	return result
}
