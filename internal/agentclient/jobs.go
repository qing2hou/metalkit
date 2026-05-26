package agentclient

import (
	"context"
	"errors"
	"net/http"

	"metalkit/internal/jobs"
)

// CurrentJob hits GET /api/v1/agent/jobs/current?machine_uuid=<uuid>.
// Returns ErrNoCurrentJob on 404 (use errors.Is); the *jobs.Job on 200.
func (c *Client) CurrentJob(ctx context.Context) (*jobs.Job, error) {
	path := c.queryWith("/api/v1/agent/jobs/current")
	var j jobs.Job
	if err := c.get(ctx, path, &j); err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Code == http.StatusNotFound {
			return nil, ErrNoCurrentJob
		}
		return nil, err
	}
	return &j, nil
}

// Claim hits POST /api/v1/agent/jobs/{id}/claim with body {"machine_uuid":<uuid>}.
func (c *Client) Claim(ctx context.Context, jobID string) (*jobs.Job, error) {
	body := map[string]string{"machine_uuid": c.UUID}
	var j jobs.Job
	if err := c.post(ctx, "/api/v1/agent/jobs/"+jobID+"/claim", body, &j); err != nil {
		return nil, err
	}
	return &j, nil
}

// Stage hits POST /api/v1/agent/jobs/{id}/stage. Returns nil on 204.
func (c *Client) Stage(ctx context.Context, jobID, stage string) error {
	body := map[string]string{
		"machine_uuid": c.UUID,
		"stage":        stage,
	}
	return c.post(ctx, "/api/v1/agent/jobs/"+jobID+"/stage", body, nil)
}

// Log hits POST /api/v1/agent/jobs/{id}/logs.
func (c *Client) Log(ctx context.Context, jobID, level, message string) error {
	body := map[string]string{
		"machine_uuid": c.UUID,
		"level":        level,
		"message":      message,
	}
	return c.post(ctx, "/api/v1/agent/jobs/"+jobID+"/logs", body, nil)
}

// Succeed hits POST /api/v1/agent/jobs/{id}/succeed.
func (c *Client) Succeed(ctx context.Context, jobID string) error {
	body := map[string]string{"machine_uuid": c.UUID}
	return c.post(ctx, "/api/v1/agent/jobs/"+jobID+"/succeed", body, nil)
}

// Fail hits POST /api/v1/agent/jobs/{id}/fail with body
// {"machine_uuid":<uuid>, "error":<errMsg>}.
func (c *Client) Fail(ctx context.Context, jobID, errMsg string) error {
	body := map[string]string{
		"machine_uuid": c.UUID,
		"error":        errMsg,
	}
	return c.post(ctx, "/api/v1/agent/jobs/"+jobID+"/fail", body, nil)
}

// Spec hits GET /api/v1/agent/jobs/{id}/spec?machine_uuid=<uuid>.
// Returns the InstallSpec on 200; 403 → ownership mismatch, 404 → unknown job
// (both surface as *APIError so callers can branch on Code).
func (c *Client) Spec(ctx context.Context, jobID string) (*jobs.InstallSpec, error) {
	path := c.queryWith("/api/v1/agent/jobs/" + jobID + "/spec")
	var s jobs.InstallSpec
	if err := c.get(ctx, path, &s); err != nil {
		return nil, err
	}
	return &s, nil
}
