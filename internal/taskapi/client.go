package taskapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/zachlatta/tasks/internal/task"
)

type Client struct {
	baseURL string
	secret  string
	http    *http.Client
}

type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s (http %d): %s", e.Code, e.Status, e.Message)
}

func NewClient(apiURL, secret string) (*Client, error) {
	if strings.TrimSpace(apiURL) == "" {
		return nil, fmt.Errorf("API URL is required")
	}
	parsed, err := url.Parse(apiURL)
	if err != nil {
		return nil, fmt.Errorf("API URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("API URL scheme must be http or https")
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.Path != "" && parsed.Path != "/" {
		return nil, fmt.Errorf("API URL must be an absolute origin without a path, query, fragment, or user information")
	}
	if parsed.Scheme == "http" && !isLoopbackHost(parsed.Hostname()) {
		return nil, fmt.Errorf("API URL must use HTTPS except on a loopback address")
	}
	if strings.TrimSpace(secret) == "" {
		return nil, fmt.Errorf("shared secret is required")
	}
	if strings.ContainsAny(secret, "\r\n") {
		return nil, fmt.Errorf("shared secret must not contain line breaks")
	}
	return &Client{
		baseURL: strings.TrimRight(apiURL, "/"),
		secret:  secret,
		http:    &http.Client{},
	}, nil
}

func (c *Client) CreateTask(ctx context.Context, input CreateTaskInput) (task.Task, error) {
	return call[task.Task](ctx, c, CreateTaskTool, input)
}

func (c *Client) EditTaskText(ctx context.Context, input EditTaskTextInput) (task.Task, error) {
	return call[task.Task](ctx, c, EditTaskTextTool, input)
}

func (c *Client) UpdateTask(ctx context.Context, input UpdateTaskInput) (task.Task, error) {
	return call[task.Task](ctx, c, UpdateTaskTool, input)
}

func (c *Client) QueryTasksSQL(ctx context.Context, input SQLQueryInput) (SQLQueryOutput, error) {
	return call[SQLQueryOutput](ctx, c, QueryTasksSQLTool, input)
}

func (c *Client) CompleteTask(ctx context.Context, input CompleteTaskInput) (task.Task, error) {
	return call[task.Task](ctx, c, CompleteTaskTool, input)
}

func (c *Client) DeleteTask(ctx context.Context, input DeleteTaskInput) (task.Task, error) {
	return call[task.Task](ctx, c, DeleteTaskTool, input)
}

func (c *Client) RestoreTask(ctx context.Context, input RestoreTaskInput) (task.Task, error) {
	return call[task.Task](ctx, c, RestoreTaskTool, input)
}

func call[Output, Input any](ctx context.Context, client *Client, name string, input Input) (Output, error) {
	var zero Output
	body, err := json.Marshal(input)
	if err != nil {
		return zero, fmt.Errorf("encode %s input: %w", name, err)
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		client.baseURL+"/api/tools/"+name,
		bytes.NewReader(body),
	)
	if err != nil {
		return zero, fmt.Errorf("create %s request: %w", name, err)
	}
	request.Header.Set("Authorization", "Bearer "+client.secret)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.http.Do(request)
	if err != nil {
		return zero, fmt.Errorf("call %s: %w", name, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return zero, fmt.Errorf("read %s response: %w", name, err)
	}
	if response.StatusCode != http.StatusOK {
		return zero, decodeAPIError(response.StatusCode, raw)
	}
	var envelope struct {
		Data Output `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return zero, fmt.Errorf("decode %s response: %w", name, err)
	}
	return envelope.Data, nil
}

func decodeAPIError(status int, body []byte) error {
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error.Code != "" {
		return &APIError{Status: status, Code: envelope.Error.Code, Message: envelope.Error.Message}
	}
	message := strings.TrimSpace(string(body))
	if message == "" {
		message = http.StatusText(status)
	}
	return &APIError{Status: status, Code: "http_error", Message: message}
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}
