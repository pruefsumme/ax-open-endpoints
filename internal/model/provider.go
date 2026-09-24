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
	"strconv"
	"strings"
	"time"
)

const (
	maxProviderRetries = 2
	maxRetryDelay      = 5 * time.Second
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
	parameters["messages"] = openAIMessages(req)
	if len(req.Tools) > 0 {
		parameters["tools"] = openAITools(req.Tools)
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
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
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
	toolCalls := make([]ToolCall, 0, len(choice.Message.ToolCalls))
	for _, call := range choice.Message.ToolCalls {
		arguments := json.RawMessage(call.Function.Arguments)
		if len(arguments) == 0 {
			arguments = json.RawMessage("{}")
		}
		if !json.Valid(arguments) {
			return nil, fmt.Errorf("%s returned invalid JSON arguments for tool %q", client.cfg.Provider, call.Function.Name)
		}
		toolCalls = append(toolCalls, ToolCall{
			ID:        call.ID,
			Name:      call.Function.Name,
			Arguments: arguments,
		})
	}
	content, err := responseText(choice.Message.Content)
	if err != nil {
		return nil, fmt.Errorf("reading %s response text: %w", client.cfg.Provider, err)
	}
	if content == "" && len(toolCalls) == 0 {
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
		Model:     modelName,
		Content:   content,
		ToolCalls: toolCalls,
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
	system, messages := anthropicMessages(req)
	parameters["messages"] = messages
	if system != "" {
		parameters["system"] = system
	}
	if len(req.Tools) > 0 {
		parameters["tools"] = anthropicTools(req.Tools)
	}
	if _, ok := parameters["max_tokens"]; !ok {
		// Anthropic requires this field even when the caller leaves AX's cap unset.
		parameters["max_tokens"] = 1024
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
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
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
	toolCalls := make([]ToolCall, 0)
	for _, block := range message.Content {
		switch block.Type {
		case "text":
			content.WriteString(block.Text)
		case "tool_use":
			arguments := block.Input
			if len(arguments) == 0 {
				arguments = json.RawMessage("{}")
			}
			if !json.Valid(arguments) {
				return nil, fmt.Errorf("Anthropic returned invalid JSON arguments for tool %q", block.Name)
			}
			toolCalls = append(toolCalls, ToolCall{ID: block.ID, Name: block.Name, Arguments: arguments})
		}
	}
	if content.Len() == 0 && len(toolCalls) == 0 {
		return nil, fmt.Errorf("Anthropic returned no text content (stop reason %q)", message.StopReason)
	}
	modelName := message.Model
	if modelName == "" {
		modelName = req.Model
	}
	return &GenerateResponse{
		Model:     modelName,
		Content:   content.String(),
		ToolCalls: toolCalls,
		Usage: UsageStats{
			PromptTokens:     message.Usage.InputTokens,
			CompletionTokens: message.Usage.OutputTokens,
			TotalTokens:      message.Usage.InputTokens + message.Usage.OutputTokens,
		},
	}, nil
}

func requestMessages(req *GenerateRequest) []Message {
	if len(req.Messages) > 0 {
		messages := append([]Message(nil), req.Messages...)
		if req.SystemInstruction != "" {
			for i := range messages {
				message := &messages[i]
				if message.Role == "system" {
					if message.Content == "" {
						message.Content = req.SystemInstruction
					} else {
						message.Content = req.SystemInstruction + "\n\n" + message.Content
					}
					return messages
				}
			}
			messages = append([]Message{{Role: "system", Content: req.SystemInstruction}}, messages...)
		}
		return messages
	}

	messages := make([]Message, 0, 2)
	if req.SystemInstruction != "" {
		messages = append(messages, Message{Role: "system", Content: req.SystemInstruction})
	}
	if req.Prompt != "" {
		messages = append(messages, Message{Role: "user", Content: req.Prompt})
	}
	return messages
}

func openAIMessages(req *GenerateRequest) []map[string]any {
	messages := requestMessages(req)
	encoded := make([]map[string]any, 0, len(messages))
	for _, message := range messages {
		item := map[string]any{"role": message.Role}
		if message.Content != "" || len(message.ToolCalls) == 0 {
			item["content"] = message.Content
		}
		if message.Role == "tool" {
			item["tool_call_id"] = message.ToolCallID
		}
		if len(message.ToolCalls) > 0 {
			calls := make([]map[string]any, 0, len(message.ToolCalls))
			for _, call := range message.ToolCalls {
				arguments := call.Arguments
				if len(arguments) == 0 {
					arguments = json.RawMessage("{}")
				}
				calls = append(calls, map[string]any{
					"id":   call.ID,
					"type": "function",
					"function": map[string]any{
						"name":      call.Name,
						"arguments": string(arguments),
					},
				})
			}
			item["tool_calls"] = calls
		}
		encoded = append(encoded, item)
	}
	return encoded
}

func openAITools(tools []ToolDefinition) []map[string]any {
	encoded := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		parameters := tool.Parameters
		if len(parameters) == 0 {
			parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		encoded = append(encoded, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        tool.Name,
				"description": tool.Description,
				"parameters":  parameters,
			},
		})
	}
	return encoded
}

func anthropicMessages(req *GenerateRequest) (string, []map[string]any) {
	messages := requestMessages(req)
	var system strings.Builder
	encoded := make([]map[string]any, 0, len(messages))
	for i := 0; i < len(messages); {
		message := messages[i]
		switch message.Role {
		case "system":
			if system.Len() > 0 {
				system.WriteString("\n\n")
			}
			system.WriteString(message.Content)
			i++
		case "tool":
			// Anthropic represents tool output as a user message containing one or
			// more tool_result blocks, rather than as a separate "tool" role.
			blocks := make([]map[string]any, 0, 1)
			for i < len(messages) && messages[i].Role == "tool" {
				toolResult := messages[i]
				blocks = append(blocks, map[string]any{
					"type":        "tool_result",
					"tool_use_id": toolResult.ToolCallID,
					"content":     toolResult.Content,
				})
				i++
			}
			encoded = append(encoded, map[string]any{"role": "user", "content": blocks})
		default:
			blocks := make([]map[string]any, 0, 1+len(message.ToolCalls))
			if message.Content != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": message.Content})
			}
			for _, call := range message.ToolCalls {
				input := call.Arguments
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				blocks = append(blocks, map[string]any{
					"type":  "tool_use",
					"id":    call.ID,
					"name":  call.Name,
					"input": input,
				})
			}
			if len(blocks) == 0 {
				blocks = append(blocks, map[string]any{"type": "text", "text": ""})
			}
			encoded = append(encoded, map[string]any{"role": message.Role, "content": blocks})
			i++
		}
	}
	if system.Len() == 0 {
		system.WriteString(req.SystemInstruction)
	}
	return system.String(), encoded
}

func anthropicTools(tools []ToolDefinition) []map[string]any {
	encoded := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		parameters := tool.Parameters
		if len(parameters) == 0 {
			parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		encoded = append(encoded, map[string]any{
			"name":         tool.Name,
			"description":  tool.Description,
			"input_schema": parameters,
		})
	}
	return encoded
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

// EffectiveProtocol returns the wire protocol selected by this configuration.
func (cfg Config) EffectiveProtocol() string {
	return protocolFor(cfg.Provider, cfg.Protocol)
}

// EffectiveBaseURL returns the configured endpoint or the default for a known provider.
func (cfg Config) EffectiveBaseURL() (string, error) {
	return baseURLFor(cfg, cfg.EffectiveProtocol())
}

func baseURLFor(cfg Config, protocol string) (string, error) {
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
	return strings.TrimRight(parsed.String(), "/"), nil
}

func endpointFor(cfg Config, protocol, path string) (string, error) {
	baseURL, err := baseURLFor(cfg, protocol)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("invalid baseURL: expected an absolute HTTP(S) URL")
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
	for attempt := 0; ; attempt++ {
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

		resp, err := c.httpClient.Do(request)
		if err != nil {
			// A timeout can happen after the provider completed and billed a call.
			// Don't replay requests when the outcome is ambiguous.
			return nil, fmt.Errorf("calling %s at %s: %w", c.cfg.Provider, safeEndpoint(endpoint), err)
		}
		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
			return resp, nil
		}

		statusCode := resp.StatusCode
		retryAfter := resp.Header.Get("Retry-After")
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		if attempt >= maxProviderRetries || !retryableStatus(statusCode) {
			return nil, fmt.Errorf("%s request failed with status %d: %s", c.cfg.Provider, statusCode, strings.TrimSpace(string(responseBody)))
		}
		if err := waitForProviderRetry(ctx, retryAfter, attempt); err != nil {
			return nil, fmt.Errorf("waiting to retry %s request: %w", c.cfg.Provider, err)
		}
	}
}

func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

func waitForProviderRetry(ctx context.Context, retryAfter string, attempt int) error {
	delay := time.Duration(250*(1<<attempt)) * time.Millisecond
	if seconds, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && seconds >= 0 {
		delay = time.Duration(seconds) * time.Second
	} else if when, err := http.ParseTime(retryAfter); err == nil {
		delay = time.Until(when)
	}
	if delay < 0 {
		delay = 0
	}
	if delay > maxRetryDelay {
		delay = maxRetryDelay
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
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
