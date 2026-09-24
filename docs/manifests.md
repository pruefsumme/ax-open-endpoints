# Writing manifests

All four kinds can live in one multi-document YAML file. See [`examples/task.yaml`](../examples/task.yaml) for a complete, working set.

## Task

```yaml
apiVersion: ax.io/v1alpha1
kind: Task
metadata:
  name: task123
  atespace: default
spec:
  image: "ghcr.io/my-org/my-agent-image"
  command: ["python", "agent.py"]
  env:
    - name: ENVIRONMENT
      value: "production"

  resources:
    requests:
      cpu: "500m"
      memory: "1Gi"
    limits:
      cpu: "2"
      memory: "4Gi"

  workspaces:
    - name: default-workspace
      path: "/workspace"
      goal: "Install dependencies and run the test suite"   # Antigravity prepares the workspace to this goal on first run

  gateway:
    name: default-gateway

  debug: true   # serve guest services inside the sandbox so `ax ssh` works; off by default
```

### Binding several workspaces

`spec.workspaces` takes as many entries as you like, so a task can compose reusable `Workspace` resources, for example the code to work on plus a shared set of tools:

```yaml
spec:
  workspaces:
    - name: my-service          # mounted at /workspace/my-service, the command's working directory
      goal: "Install dependencies and run the test suite"
    - name: team-tools
      path: "/workspace/tools"  # explicit mount path
```

Each entry is set up independently at its own path, in order. Every entry needs a `name`; without a `path` it lands at `/workspace/<name>`, and paths must be unique. The first entry is the working directory of `spec.command`, and the task reports `WorkspaceReady` only once all of them are prepared. See [`examples/multi-workspace.yaml`](../examples/multi-workspace.yaml) for a complete set.

## Workspace

```yaml
apiVersion: ax.io/v1alpha1
kind: Workspace
metadata:
  name: default-workspace
  atespace: default
spec:
  git:
    - name: origin
      repo: "https://github.com/chalk/chalk.git"
      branch: "main"
  mcp:
    registries:
      - provider: google
        query: "mcp.tags:build"
    servers:
      - name: git-tools
        endpoint: "http://git-mcp.default.svc.cluster.local:8080"
  skills:
    registries:
      - provider: google
        query: "skills.tags:nodejs"
    path: "/.agents/skills"
```

## Gateway

```yaml
apiVersion: ax.io/v1alpha1
kind: Gateway
metadata:
  name: default-gateway
  atespace: default
spec:
  listeners:
    - name: grpc
      port: 8494
      protocol: gRPC
    - name: http
      port: 8080
      protocol: HTTP
  egress:
    allowlist:
      hosts:
        - host: "*"      # allow everything on 443; tighten this in production
          port: 443
```

## Model

`provider` names the service; `protocol` selects its HTTP request format. AX
supports `openai` (Chat Completions), `anthropic` (Messages), and `google`
(Gemini generateContent). When `protocol` is omitted, `google` and `anthropic`
use their native APIs and every other provider name uses OpenAI-compatible Chat
Completions. This makes OpenRouter, hosted gateways, and local servers work
without a provider-specific code change.

Official protocol references:

- [OpenRouter Chat Completions](https://openrouter.ai/docs/api/api-reference/chat/create-a-chat-completion)
- [Gemini OpenAI compatibility](https://ai.google.dev/gemini-api/docs/openai)
- [Anthropic Messages API](https://docs.anthropic.com/en/api/messages)

`baseURL` is the API base URL, including a version path such as `/v1`. AX adds
the protocol endpoint path, so an OpenAI-compatible server usually ends at
`/v1`, while Anthropic uses the service root. The built-in defaults are
OpenAI's endpoint for `provider: openai`, OpenRouter's endpoint for
`provider: openrouter`, and the official endpoints for the native Google and
Anthropic protocols. Other provider names need an explicit `baseURL`.

### OpenAI-compatible provider

Store the key in a Kubernetes Secret and refer to it by name and key. For
example, this manifest selects Space Bunny Alpha through OpenRouter; replace
the model slug with any model available to your OpenRouter account.

```bash
kubectl create secret generic openrouter-api-secret \
  --from-literal=OPENROUTER_API_KEY='your-key'
```

```yaml
apiVersion: ax.io/v1alpha1
kind: Model
metadata:
  name: default-model
  atespace: default
spec:
  provider: openrouter
  protocol: openai
  model: stealth/space-bunny-alpha
  baseURL: https://openrouter.ai/api/v1
  secretKey:
    name: openrouter-api-secret
    key: OPENROUTER_API_KEY
  headers:
    HTTP-Referer: https://example.com
    X-OpenRouter-Title: AX
  parameters:
    temperature: 0.2
```

`Authorization: Bearer <key>` is the default authentication for this protocol.
Set `apiKeyHeader` and `apiKeyPrefix` when a compatible gateway expects a
different key header. Use `headers` for additional non-secret headers; keep
credentials in the referenced Secret.

```yaml
spec:
  provider: my-llm-gateway
  protocol: openai
  model: qwen-model-name
  baseURL: https://llm.example.com/v1
  apiKeyHeader: X-API-Key
  secretKey:
    name: llm-gateway-secret
    key: API_KEY
```

Providers that use a different HTTP protocol need a protocol adapter in
`internal/model/provider.go`. AX supports text and function-call responses for
OpenAI-compatible and Anthropic protocols. The built-in workspace goal agent
uses one bounded `run_command` tool loop with those protocols. It does not
stream tokens or accept multimodal input.

The OpenAI-compatible and Anthropic clients retry explicit `429`, `502`, `503`,
and `504` responses up to two times, honoring `Retry-After` up to five seconds.
They do not retry network timeouts because a provider may have completed the
request before the connection failed.

The atespace's `default-model` is supplied to every task container as
`AX_MODEL_YAML` and `AX_MODEL_API_KEY`; OpenAI and Anthropic protocol settings
also populate the matching standard environment variables. The task's own
command can use these values with its agent library. A command running in that
container can read the configured API key, so use a separate atespace for
workloads that should not share model credentials.

For a `Workspace` goal, OpenAI-compatible and Anthropic Models use AX's generic
bootstrap. A native Google Model keeps Antigravity. To use the configured Gemini
model with the generic bootstrap, set `protocol: openai` and use Google's
OpenAI-compatible base URL, `https://generativelanguage.googleapis.com/v1beta/openai/`.
The `run_command` tool starts in the workspace directory, but shell commands are
not restricted to that path; the task container is the isolation boundary. The
task Gateway must allow egress to the selected provider host on port 443.

### Google Gemini

For Google models, store the API key and set `provider: google`.

```bash
kubectl create secret generic gemini-api-secret --from-literal=GEMINI_API_KEY="AIzaSy..."
```

Then reference it from the `Model`:

```yaml
apiVersion: ax.io/v1alpha1
kind: Model
metadata:
  name: default-model
  atespace: default
spec:
  provider: google
  model: gemini-3.8-flash
  secretKey:
    name: gemini-api-secret
    key: GEMINI_API_KEY
  parameters:
    temperature: 0.9
```

### Anthropic Claude

Store the key the same way and set `provider: anthropic`.

```bash
kubectl create secret generic anthropic-api-secret --from-literal=ANTHROPIC_API_KEY="sk-ant-..."
```

```yaml
apiVersion: ax.io/v1alpha1
kind: Model
metadata:
  name: claude-model
  atespace: default
spec:
  provider: anthropic
  model: claude-opus-5
  secretKey:
    name: anthropic-api-secret
    key: ANTHROPIC_API_KEY
  parameters:
    maxTokens: 16000
    temperature: 0.9
```
