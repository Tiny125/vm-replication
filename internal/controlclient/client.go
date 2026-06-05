// Package controlclient is a tiny REST client the agent uses to register itself
// and report each sync to the control plane. It depends only on the stdlib and
// the shared api types, so importing it does not pull the storage engine into
// the agent binary.
package controlclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tiny125/vm-replication/internal/api"
)

// Client talks to a control plane instance.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// New returns a client for baseURL (e.g. http://controld:8088) using a bearer
// token. A zero-value Client (nil) is a no-op; callers can guard with Enabled.
func New(baseURL, token string) *Client {
	if baseURL == "" {
		return nil
	}
	return &Client{
		base:  strings.TrimRight(baseURL, "/"),
		token: token,
		http:  &http.Client{Timeout: 10 * time.Second},
	}
}

// Enabled reports whether the client is configured.
func (c *Client) Enabled() bool { return c != nil }

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("control plane %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// RegisterServer registers/updates this host in the inventory.
func (c *Client) RegisterServer(ctx context.Context, req api.RegisterServerRequest) (api.Server, error) {
	var sv api.Server
	err := c.do(ctx, http.MethodPost, "/api/v1/servers", req, &sv)
	return sv, err
}

// ReportSync posts the result of one replication pass for a job.
func (c *Client) ReportSync(ctx context.Context, jobID int64, req api.ReportSyncRequest) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/api/v1/jobs/%d/syncs", jobID), req, nil)
}

// ListServers returns the server inventory.
func (c *Client) ListServers(ctx context.Context) ([]api.Server, error) {
	var out []api.Server
	err := c.do(ctx, http.MethodGet, "/api/v1/servers", nil, &out)
	return out, err
}

// CreateJob creates a replication job.
func (c *Client) CreateJob(ctx context.Context, req api.CreateJobRequest) (api.Job, error) {
	var job api.Job
	err := c.do(ctx, http.MethodPost, "/api/v1/jobs", req, &job)
	return job, err
}

// Status returns the health/RPO view for all jobs.
func (c *Client) Status(ctx context.Context) ([]api.JobStatus, error) {
	var out []api.JobStatus
	err := c.do(ctx, http.MethodGet, "/api/v1/status", nil, &out)
	return out, err
}

// SetState changes a job's lifecycle state.
func (c *Client) SetState(ctx context.Context, jobID int64, state api.JobState) (api.Job, error) {
	var job api.Job
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/api/v1/jobs/%d/state", jobID), api.SetStateRequest{State: state}, &job)
	return job, err
}
