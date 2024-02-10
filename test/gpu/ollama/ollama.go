// Copyright 2023 The gVisor Authors.
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

// Package ollama provides an Ollama API client.
package ollama

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gvisor.dev/gvisor/pkg/test/dockerutil"
	"gvisor.dev/gvisor/pkg/test/testutil"
)

const (
	// Port is the port used by the ollama server.
	Port = 11434

	// curtQuery is a query that should result in a very curt response.
	curtQuery = `Please reply with the single word: "Hello". Do not reply with any other word.`
)

// Ollama is an ollama client.
type Ollama struct {
	// server is used to perform requests against the server.
	server Server

	// logger is used to log.
	logger testutil.Logger

	// ModelNames is the list of available model names.
	ModelNames []string

	// HasGPU is set depending on whether the LLM has GPU access.
	// ollama supports running both on CPU and GPU, and detects this
	// by spawning nvidia-smi.
	HasGPU bool
}

// Server performs requests against an ollama server.
type Server interface {
	// HTTPRequest performs an HTTP request against the ollama server.
	// The request is a GET request if postData == nil, otherwise POST.
	HTTPRequest(ctx context.Context, endpoint string, postData []byte) ([]byte, error)

	// Logs retrieves logs from the server.
	Logs(ctx context.Context) (string, error)
}

// New starts a new Ollama server in the given container,
// then waits for it to serve and returns the client.
func New(ctx context.Context, server Server, logger testutil.Logger) (*Ollama, error) {
	started := time.Now()
	llm := &Ollama{
		logger: logger,
		server: server,
	}

	// Wait until serving.
	if err := llm.WaitUntilServing(ctx); err != nil {
		return nil, fmt.Errorf("ollama did not come up for serving: %w", err)
	}
	logger.Logf("Ollama serving API requests after %v", time.Since(started))

	// Get list of model names.
	modelNames, err := llm.listModelNames(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not list model names: %w", err)
	}
	if len(modelNames) == 0 {
		return nil, errors.New("no models available")
	}
	llm.ModelNames = modelNames
	logger.Logf("Available ollama model names: %v (loaded %v since container start)", modelNames, time.Since(started))

	// Load the first model.
	// This is necessary to force ollama to load a model, without which
	// we cannot detect if it is using the GPU or not.
	// This may fail during the process of loading the first model, so we keep
	// iterating for a while.
	_, err = llm.Prompt(ctx, &Prompt{
		Model:     &Model{Name: llm.ModelNames[0]},
		WarmFirst: false,
		Query:     curtQuery,
	})
	if err != nil {
		return nil, fmt.Errorf("could not load first model %q: %w", llm.ModelNames[0], err)
	}
	logger.Logf("Loaded first ollama model %q (%v since container start)", llm.ModelNames[0], time.Since(started))

	// Now go over the logs and check if the GPU was used.
	logs, err := llm.server.Logs(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not get logs: %w", err)
	}
	switch {
	case strings.Contains(logs, "no GPU detected"):
		llm.HasGPU = false
	case strings.Contains(logs, "Nvidia GPU detected"):
		llm.HasGPU = true
	default:
		return nil, fmt.Errorf("cannot determine whether ollama is using GPU from logs:\n%s", logs)
	}
	logger.Logf("Ollama successfully initialized in a total of %v", time.Since(started))
	return llm, nil
}

// dockerServer implements `Server`. It interfaces with an ollama server
// running in a local Docker container.
type dockerServer struct {
	container *dockerutil.Container
	logger    testutil.Logger
}

// NewDocker returns a new Ollama client talking to an Ollama server that runs
// in a local Docker container.
func NewDocker(ctx context.Context, cont *dockerutil.Container, logger testutil.Logger) (*Ollama, error) {
	opts := dockerutil.GPURunOpts()
	opts.Image = "gpu/ollama"
	started := time.Now()
	if err := cont.Spawn(ctx, opts); err != nil {
		return nil, fmt.Errorf("could not start ollama: %v", err)
	}
	logger.Logf("Ollama container started after %v", time.Since(started))
	ds := &dockerServer{
		container: cont,
		logger:    logger,
	}
	return New(ctx, ds, logger)
}

// HTTPRequest implements `Server.HTTPRequest`.
func (ds *dockerServer) HTTPRequest(ctx context.Context, endpoint string, data []byte) ([]byte, error) {
	cmd := []string{"wget", "-qO-"}
	if data != nil {
		cmd = append(cmd, "--post-data", string(data))
	}
	cmd = append(cmd, fmt.Sprintf("http://llm:%d%s", Port, endpoint))
	out, err := dockerutil.MakeContainer(ctx, ds.logger).Run(ctx, dockerutil.RunOpts{
		Image: "basic/busybox",
		Links: []string{ds.container.MakeLink("llm")},
	}, cmd...)
	if err != nil {
		if out != "" {
			return []byte(out), fmt.Errorf("command %q failed (%w): %v", strings.Join(cmd, " "), err, out)
		}
		return nil, fmt.Errorf("could not run command %q: %w", strings.Join(cmd, " "), err)
	}
	return []byte(out), nil
}

// Logs implements `Server.Logs`.
func (ds *dockerServer) Logs(ctx context.Context) (string, error) {
	return ds.container.Logs(ctx)
}

// request makes an HTTP request to the ollama API.
func (llm *Ollama) request(ctx context.Context, endpoint string, data []byte) ([]byte, error) {
	if endpoint != "" && !strings.HasPrefix(endpoint, "/") {
		return nil, fmt.Errorf("endpoint must be empty or start with '/', got %q", endpoint)
	}
	return llm.server.HTTPRequest(ctx, endpoint, data)
}

// jsonGet performs a JSON HTTP GET request.
func jsonGet[Out any](ctx context.Context, llm *Ollama, endpoint string) (Out, error) {
	var resp Out
	out, err := llm.request(ctx, endpoint, nil)
	if err != nil {
		return resp, fmt.Errorf("GET %q failed: %w", endpoint, err)
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return resp, fmt.Errorf("malformed JSON response %q: %w", string(out), err)
	}
	return resp, nil
}

// jsonPost performs a JSON HTTP POST request.
func jsonPost[In, Out any](ctx context.Context, llm *Ollama, endpoint string, input In) (Out, error) {
	var resp Out
	query, err := json.Marshal(input)
	if err != nil {
		return resp, fmt.Errorf("could not marshal input %v: %w", input, err)
	}
	out, err := llm.request(ctx, endpoint, query)
	if err != nil {
		return resp, fmt.Errorf("POST %q %v failed: %w", endpoint, string(query), err)
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return resp, fmt.Errorf("malformed JSON response %q: %w", string(out), err)
	}
	return resp, nil
}

// listModelNames lists the available model names.
func (llm *Ollama) listModelNames(ctx context.Context) ([]string, error) {
	type model struct {
		Name       string `json:"name"`
		ModifiedAt string `json:"modified_at"`
		Size       int    `json:"size"`
	}
	type modelsList struct {
		Models []model `json:"models"`
	}
	models, err := jsonGet[modelsList](ctx, llm, "/api/tags")
	if err != nil {
		return nil, err
	}
	modelNames := make([]string, len(models.Models))
	for i, m := range models.Models {
		modelNames[i] = m.Name
	}
	return modelNames, nil
}

// WaitUntilServing waits until ollama is serving, or the context expires.
func (llm *Ollama) WaitUntilServing(ctx context.Context) error {
	for ctx.Err() == nil {
		out, err := llm.request(ctx, "/", nil)
		if err != nil {
			continue
		}
		if string(out) == "Ollama is running" {
			return nil
		}
	}
	return fmt.Errorf("ollama did not respond: %w", ctx.Err())
}

// Model encodes a model and options for it.
type Model struct {
	// Name is the name of the ollama model, e.g. "codellama:7b".
	Name string

	// Options maps parameter names to JSON-compatible values.
	Options map[string]any
}

// String returns the model's name.
func (m *Model) String() string {
	return m.Name
}

// modelTemperatureOption is the temperature option that most models have
// which controls how free they are from deviating from their most-likely
// token chain.
const modelTemperatureOption = "temperature"

// RaiseTemperature increases the "temperature" option of the model,
// if any.
func (m *Model) RaiseTemperature() {
	temp, ok := m.Options[modelTemperatureOption]
	if !ok {
		temp = float64(0.0)
	}
	if m.Options == nil {
		m.Options = map[string]any{}
	}
	m.Options[modelTemperatureOption] = min(1.0, temp.(float64)*2+.025)
}

// ZeroTemperatureModel returns a Model with the given name and an initial
// temperature setting of zero. This setting allows for consistent settings.
func ZeroTemperatureModel(name string) *Model {
	return &Model{
		Name: name,
		Options: map[string]any{
			modelTemperatureOption: 0.0,
		},
	}
}

// Prompt is an ollama prompt.
type Prompt struct {
	// Model is the model to query.
	Model *Model

	// Query is the prompt string.
	// Common leading whitespace will be removed.
	Query string

	// images is a set of attached images.
	// Use AddImage to add an image.
	images [][]byte

	// Context is the conversational context to follow up on, if any.
	// This is returned from `Response`.
	Context ConversationContext

	// WarmFirst ensures the model is already loaded by issuing a small query
	// beforehand. This is necessary for benchmarks to be accurate, but is
	// unnecessary when just testing.
	WarmFirst bool
}

// AddImage attaches an image to the prompt.
// Returns itself for chainability.
func (p *Prompt) AddImage(data []byte) *Prompt {
	p.images = append(p.images, data)
	return p
}

// CleanQuery removes common whitespace from query lines, and all
// leading/ending whitespace-only lines.
// It is useful to be able to specify query string as indented strings
// without breaking visual continuity in Go code.
// For example (where dots are spaces):
//
// """\n
// ..The Quick Brown Fox\n
// ..Jumps Over\n
// ....The Lazy Dog\n
// ."""
//
// becomes:
//
// ""The Quick Brown Fox\n
// Jumps Over\n
// ..The Lazy Dog"""
func (p *Prompt) CleanQuery() string {
	lines := strings.Split(p.Query, "\n")

	// Trim lines at the beginning and end that are only whitespace.
	trimmedLines := make([]string, 0, len(lines))
	startedNonWhitespace := false
	var block []string
	for _, line := range lines {
		trimmedLine := strings.TrimSpace(line)
		if !startedNonWhitespace && trimmedLine != "" {
			startedNonWhitespace = true
		}
		if startedNonWhitespace {
			block = append(block, line)
		}
		if trimmedLine != "" {
			trimmedLines = append(trimmedLines, block...)
			block = block[:0]
		}
	}

	// Find longest common whitespace prefix.
	if len(trimmedLines) == 0 {
		return ""
	}
	trimmedFirstLine := strings.TrimSpace(trimmedLines[0])
	common := []rune(trimmedLines[0][:strings.Index(trimmedLines[0], trimmedFirstLine)])
	for ; len(common) > 0; common = common[:len(common)-1] {
		allMatch := true
		for _, line := range trimmedLines[1:] {
			if strings.TrimSpace(line) == "" {
				continue // Ignore whitespace-only or empty lines.
			}
			if !strings.HasPrefix(line, string(common)) {
				allMatch = false
				break
			}
		}
		if allMatch {
			break
		}
	}

	// Remove it.
	if len(common) > 0 {
		for i, line := range trimmedLines {
			trimmedLines[i] = strings.TrimPrefix(line, string(common))
		}
	}

	return strings.Join(trimmedLines, "\n")
}

// String returns a human-friendly string representing this prompt.
func (p *Prompt) String() string {
	return fmt.Sprintf("[%v] %s", p.Model, p.CleanQuery())
}

// PromptJSON encodes the JSON data for a query.
type PromptJSON struct {
	Model   string              `json:"model"`
	Prompt  string              `json:"prompt"`
	Images  []string            `json:"images"`
	Stream  bool                `json:"stream"`
	Context ConversationContext `json:"context"`
	Options map[string]any      `json:"options"`
}

// json encodes this prompt to the JSON format expected by Ollama.
func (p *Prompt) json() PromptJSON {
	images := make([]string, len(p.images))
	for i, image := range p.images {
		images[i] = base64.StdEncoding.EncodeToString(image)
	}
	return PromptJSON{
		Model:   p.Model.Name,
		Prompt:  p.CleanQuery(),
		Images:  images,
		Stream:  false,
		Context: p.Context,
		Options: p.Model.Options,
	}
}

// ResponseJSON is the JSON-format response from ollama about a prompt in
// non-streamed mode.
type ResponseJSON struct {
	Model           string              `json:"model"`
	Response        string              `json:"response"`
	Done            bool                `json:"done"`
	TotalNanos      int                 `json:"total_duration"`
	LoadNanos       int                 `json:"load_duration"`
	EvalCount       int                 `json:"eval_count"`
	EvalNanos       int                 `json:"eval_duration"`
	PromptEvalCount int                 `json:"prompt_eval_count"`
	PromptEvalNanos int                 `json:"prompt_eval_duration"`
	Context         ConversationContext `json:"context"`
}

// Response represents a response to a query from Ollama.
type Response struct {
	data ResponseJSON
}

// Done returns whether the response was completely generated.
func (r *Response) Done() bool {
	return r.data.Done
}

// String returns the response text, if it is done.
func (r *Response) String() string {
	if !r.data.Done {
		if r.data.Response != "" {
			return fmt.Sprintf("%s <NOT DONE>", r.data.Response)
		}
		return "<NOT DONE>"
	}
	return r.data.Response
}

// Text returns the body of the response, if it is done.
func (r *Response) Text() string {
	return r.data.Response
}

// TotalDuration returns the total response generation time.
func (r *Response) TotalDuration() time.Duration {
	return time.Duration(r.data.TotalNanos) * time.Nanosecond
}

// LoadDuration returns the load response generation time.
func (r *Response) LoadDuration() time.Duration {
	return time.Duration(r.data.LoadNanos) * time.Nanosecond
}

// EvalDuration returns the response evaluation time.
func (r *Response) EvalDuration() time.Duration {
	return time.Duration(r.data.EvalNanos) * time.Nanosecond
}

// PromptEvalDuration returns the prompt evaluation time.
func (r *Response) PromptEvalDuration() time.Duration {
	return time.Duration(r.data.PromptEvalNanos) * time.Nanosecond
}

// TokensPerSecond computes the number of tokens generated per second.
func (r *Response) TokensPerSecond() float64 {
	if !r.data.Done || r.EvalDuration() == 0 {
		return 0
	}
	return float64(r.data.EvalCount) / float64(r.EvalDuration().Seconds())
}

// ConversationContext represents a conversational context.
// It is returned by a response and may be passed to a follow-up prompt.
type ConversationContext []int

// withServerLogsErr adds server logs to `err` if possible.
func (llm *Ollama) withServerLogsErr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%w (+ context err: %v)", err, ctx.Err())
	}
	serverLogs, logsErr := llm.server.Logs(ctx)
	if logsErr != nil {
		return fmt.Errorf("%w (could not get server logs: %v)", err, logsErr)
	}
	if serverLogs != "" {
		return fmt.Errorf("%w; ollama server logs:\n%v\n(end of ollama server logs)", err, serverLogs)
	}
	return fmt.Errorf("%w (server logs are empty)", err)
}

// Prompt returns the result of prompting the given `model` with `prompt`.
func (llm *Ollama) Prompt(ctx context.Context, prompt *Prompt) (*Response, error) {
	if prompt.WarmFirst {
		warmCtx, warmCancel := context.WithTimeout(ctx, 3*time.Minute)
		_, err := jsonPost[PromptJSON, ResponseJSON](warmCtx, llm, "/api/generate", (&Prompt{
			Model: prompt.Model,
			Query: curtQuery,
		}).json())
		warmCancel()
		if err != nil {
			return nil, llm.withServerLogsErr(ctx, fmt.Errorf("warmup prompt for model %s failed: %w", prompt.Model.Name, err))
		}
	}
	resp, err := jsonPost[PromptJSON, ResponseJSON](ctx, llm, "/api/generate", prompt.json())
	if err != nil {
		return nil, llm.withServerLogsErr(ctx, fmt.Errorf("prompt (%s %q) request failed: %w", prompt.Model.Name, prompt.CleanQuery(), err))
	}
	return &Response{data: resp}, nil
}

// PromptUntil repeatedly issues a prompt until `iterate` returns a nil error.
// `iterate` may optionally return an updated `Prompt` which will be used to
// follow up.
// This is useful to work around the flakiness of LLMs in tests.
func (llm *Ollama) PromptUntil(ctx context.Context, prompt *Prompt, iterate func(*Prompt, *Response) (*Prompt, error)) (*Response, error) {
	var lastResponse *Response
	var lastError error
	attempts := 0
	warmed := false
	for ctx.Err() == nil {
		response, err := llm.Prompt(ctx, prompt)
		if err != nil {
			return nil, fmt.Errorf("prompt request failed: %w", err)
		}
		if prompt.WarmFirst && !warmed {
			// Future prompts do not need to specify the WarmFirst option.
			promptCopy := *prompt
			promptCopy.WarmFirst = false
			prompt = &promptCopy
			warmed = true
		}
		attempts++
		newPrompt, err := iterate(prompt, response)
		if err == nil {
			return response, nil
		}
		if newPrompt != nil {
			prompt = newPrompt
		}
		lastResponse = response
		lastError = err
	}
	return nil, fmt.Errorf("response %q (attempt #%d with prompt %v) did not match predicate: %v", lastResponse, attempts, prompt, lastError)
}
