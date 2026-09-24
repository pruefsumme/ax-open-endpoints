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

package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// providerAdapter translates AX's small text-generation request into one
// provider's HTTP protocol. Add an adapter here when a provider does not speak
// one of the supported protocols.
type providerAdapter interface {
	Generate(context.Context, *Client, *GenerateRequest) (*GenerateResponse, error)
}

type openAIAdapter struct{}
type anthropicAdapter struct{}
type googleAdapter struct{}

func adapterFor(protocol string) (providerAdapter, error) {
	switch protocol {
	case ProtocolOpenAI:
		return openAIAdapter{}, nil
	case ProtocolAnthropic:
		return anthropicAdapter{}, nil
	case ProtocolGoogle:
		return googleAdapter{}, nil
	default:
		return nil, fmt.Errorf("unsupported protocol %q", protocol)
	}
}

func (googleAdapter) Generate(ctx context.Context, client *Client, req *GenerateRequest) (*GenerateResponse, error) {
	return client.generateGoogle(ctx, req)
}

func (openAIAdapter) Generate(ctx context.Context, client *Client, req *GenerateRequest) (*GenerateResponse, error) {
	endpoint, err := endpointFor(client.cfg, ProtocolOpenAI, "/chat/completions")
	if err != nil {
		return nil, err
	}

	parameters := client.requestParameters()
	moveParameter(parameters, "maxTokens", "max_tokens")
	moveParameter(parameters, "maxOutputTokens", "max_tokens")
	parameters["model"] = req.Model
	parameters["messages"] = promptMessages(req)
	if req.Temperature > 0 {
		parameters["temperature"] = req.Temperature
	}
	if req.MaxTokens > 0 {
		parameters["max_tokens"] = req.MaxTokens
	}
	if err := rejectStreaming(parameters); err != nil {
		return nil, err
	}

	resp, err := client.postJSON(ctx, ProtocolOpenAI, endpoint, parameters)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var completion struct {
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content   json.RawMessage `json:"content"`
				ToolCalls json.RawMessage `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&completion); err != nil {
		return nil, fmt.Errorf("decoding %s response: %w", client.cfg.Provider, err)
	}
	if len(completion.Choices) == 0 {
		return nil, fmt.Errorf("%s returned no completion choices", client.cfg.Provider)
	}

	choice := completion.Choices[0]
	if hasJSONValue(choice.Message.ToolCalls) || choice.FinishReason == "tool_calls" {
		return nil, fmt.Errorf("%s returned tool calls, but AX Generate only supports text responses", client.cfg.Provider)
	}
	content, err := responseText(choice.Message.Content)
	if err != nil {
		return nil, fmt.Errorf("reading %s response text: %w", client.cfg.Provider, err)
	}
	if content == "" {
		return nil, fmt.Errorf("%s returned no text content (finish reason %q)", client.cfg.Provider, choice.FinishReason)
	}

	modelName := completion.Model
	if modelName == "" {
		modelName = req.Model
	}
	totalTokens := completion.Usage.TotalTokens
	if totalTokens == 0 {
		totalTokens = completion.Usage.PromptTokens + completion.Usage.CompletionTokens
	}
	return &GenerateResponse{
		Model:   modelName,
		Content: content,
		Usage: UsageStats{
			PromptTokens:     completion.Usage.PromptTokens,
			CompletionTokens: completion.Usage.CompletionTokens,
			TotalTokens:      totalTokens,
		},
	}, nil
}

func (anthropicAdapter) Generate(ctx context.Context, client *Client, req *GenerateRequest) (*GenerateResponse, error) {
	if client.cfg.APIKey == "" {
		return nil, fmt.Errorf("provider %q requires an API key", client.cfg.Provider)
	}
	endpoint, err := endpointFor(client.cfg, ProtocolAnthropic, "/v1/messages")
	if err != nil {
		return nil, err
	}

	parameters := client.requestParameters()
	moveParameter(parameters, "maxTokens", "max_tokens")
	moveParameter(parameters, "maxOutputTokens", "max_tokens")
	parameters["model"] = req.Model
	parameters["messages"] = []map[string]string{{"role": "user", "content": req.Prompt}}
	if _, ok := parameters["max_tokens"]; !ok {
		// Anthropic requires this field even when the caller leaves AX's cap unset.
		parameters["max_tokens"] = 1024
	}
	if req.SystemInstruction != "" {
		parameters["system"] = req.SystemInstruction
	}
	if req.Temperature > 0 {
		parameters["temperature"] = req.Temperature
	}
	if req.MaxTokens > 0 {
		parameters["max_tokens"] = req.MaxTokens
	}
	if err := rejectStreaming(parameters); err != nil {
		return nil, err
	}

	resp, err := client.postJSON(ctx, ProtocolAnthropic, endpoint, parameters)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var message struct {
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&message); err != nil {
		return nil, fmt.Errorf("decoding Anthropic response: %w", err)
	}

	var content strings.Builder
	for _, block := range message.Content {
		if block.Type == "text" {
			content.WriteString(block.Text)
		}
	}
	if content.Len() == 0 {
		if message.StopReason == "tool_use" {
			return nil, fmt.Errorf("Anthropic returned tool use, but AX Generate only supports text responses")
		}
		return nil, fmt.Errorf("Anthropic returned no text content (stop reason %q)", message.StopReason)
	}
	modelName := message.Model
	if modelName == "" {
		modelName = req.Model
	}
	return &GenerateResponse{
		Model:   modelName,
		Content: content.String(),
		Usage: UsageStats{
			PromptTokens:     message.Usage.InputTokens,
			CompletionTokens: message.Usage.OutputTokens,
			TotalTokens:      message.Usage.InputTokens + message.Usage.OutputTokens,
		},
	}, nil
}

func promptMessages(req *GenerateRequest) []map[string]string {
	messages := make([]map[string]string, 0, 2)
	if req.SystemInstruction != "" {
		messages = append(messages, map[string]string{"role": "system", "content": req.SystemInstruction})
	}
	messages = append(messages, map[string]string{"role": "user", "content": req.Prompt})
	return messages
}

func (c *Client) requestParameters() map[string]any {
	parameters := make(map[string]any, len(c.cfg.Parameters)+4)
	for name, value := range c.cfg.Parameters {
		if name != systemInstructionParam {
			parameters[name] = value
		}
	}
	return parameters
}

func rejectStreaming(parameters map[string]any) error {
	if streaming, ok := parameters["stream"].(bool); ok && streaming {
		return fmt.Errorf("streaming is not supported by AX Generate")
	}
	return nil
}

func moveParameter(parameters map[string]any, from, to string) {
	if _, exists := parameters[to]; exists {
		delete(parameters, from)
		return
	}
	if value, exists := parameters[from]; exists {
		parameters[to] = value
		delete(parameters, from)
	}
}

func responseText(raw json.RawMessage) (string, error) {
	if !hasJSONValue(raw) {
		return "", nil
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}

	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var content strings.Builder
		for _, part := range parts {
			if part.Type == "" || part.Type == "text" {
				content.WriteString(part.Text)
			}
		}
		return content.String(), nil
	}

	return "", fmt.Errorf("content must be a string or text parts")
}

func hasJSONValue(raw json.RawMessage) bool {
	return len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func endpointFor(cfg Config, protocol, path string) (string, error) {
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		switch protocol {
		case ProtocolGoogle:
			baseURL = "https://generativelanguage.googleapis.com"
		case ProtocolAnthropic:
			baseURL = "https://api.anthropic.com"
		case ProtocolOpenAI:
			switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
			case "", "openai":
				baseURL = "https://api.openai.com/v1"
			case "openrouter":
				baseURL = "https://openrouter.ai/api/v1"
			default:
				return "", fmt.Errorf("baseURL is required for OpenAI-compatible provider %q", cfg.Provider)
			}
		default:
			return "", fmt.Errorf("unsupported protocol %q", protocol)
		}
	}

	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("invalid baseURL: expected an absolute HTTP(S) URL")
	}
	if parsed.User != nil {
		return "", fmt.Errorf("baseURL must not contain embedded credentials; use secretKey")
	}
	if !strings.HasSuffix(strings.TrimRight(parsed.Path, "/"), path) {
		parsed.Path = strings.TrimRight(parsed.Path, "/") + path
		parsed.RawPath = ""
	}
	return parsed.String(), nil
}

func (c *Client) postJSON(ctx context.Context, protocol, endpoint string, payload any) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encoding %s request: %w", c.cfg.Provider, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating %s request for %s: %w", c.cfg.Provider, safeEndpoint(endpoint), err)
	}
	request.Header.Set("Content-Type", "application/json")
	for name, value := range c.cfg.Headers {
		request.Header.Set(name, value)
	}
	if protocol == ProtocolAnthropic && request.Header.Get("anthropic-version") == "" {
		request.Header.Set("anthropic-version", "2023-06-01")
	}
	if c.cfg.APIKey != "" {
		header, prefix := defaultAPIKeyAuth(protocol)
		if c.cfg.APIKeyHeader != "" {
			header = c.cfg.APIKeyHeader
			prefix = ""
		}
		if c.cfg.APIKeyPrefix != "" {
			prefix = c.cfg.APIKeyPrefix
		}
		request.Header.Set(header, prefix+c.cfg.APIKey)
	}

	// A network timeout can happen after a provider has already completed and
	// billed the generation, so retries are left to the caller.
	resp, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("calling %s at %s: %w", c.cfg.Provider, safeEndpoint(endpoint), err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%s request failed with status %d: %s", c.cfg.Provider, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp, nil
}

func defaultAPIKeyAuth(protocol string) (header, prefix string) {
	switch protocol {
	case ProtocolAnthropic:
		return "x-api-key", ""
	default:
		return "Authorization", "Bearer "
	}
}

func safeEndpoint(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "configured endpoint"
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.Redacted()
}
