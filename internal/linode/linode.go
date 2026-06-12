// Package linode is a small client for the parts of the Linode (Akamai Cloud)
// API the appliance needs: provisioning a Block Storage volume per migration,
// attaching it to the replication server, and finalizing a migration into a
// reusable artifact (a cloned volume) plus an optional launched instance.
//
// It depends only on the stdlib. Network calls require a valid API token, so
// these paths are exercised against the real API, not in a sandbox.
package linode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const apiBase = "https://api.linode.com/v4"

// Client talks to the Linode API with a personal access token.
type Client struct {
	token string
	http  *http.Client
}

// New returns a client for the given token.
func New(token string) *Client {
	return &Client{token: token, http: &http.Client{Timeout: 60 * time.Second}}
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("linode %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Volume is a Block Storage volume.
type Volume struct {
	ID             int64  `json:"id"`
	Label          string `json:"label"`
	Status         string `json:"status"`
	Size           int    `json:"size"` // GiB
	LinodeID       int64  `json:"linode_id"`
	FilesystemPath string `json:"filesystem_path"`
}

// CreateVolume creates a Block Storage volume of sizeGiB. When linodeID is
// non-zero the volume is created attached to that Linode and region is OMITTED:
// the API requires the volume's region to match the Linode's, and rejects the
// request if a different region is supplied ("The Linode's region does not
// match the requested region for creation"). region is only sent for
// unattached volumes.
func (c *Client) CreateVolume(ctx context.Context, label, region string, sizeGiB int, linodeID int64) (Volume, error) {
	req := map[string]any{"label": label, "size": sizeGiB}
	if linodeID != 0 {
		req["linode_id"] = linodeID
	} else {
		req["region"] = region
	}
	var v Volume
	err := c.do(ctx, http.MethodPost, "/volumes", req, &v)
	return v, err
}

// GetVolume fetches a volume by id.
func (c *Client) GetVolume(ctx context.Context, id int64) (Volume, error) {
	var v Volume
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/volumes/%d", id), nil, &v)
	return v, err
}

// AttachVolume attaches a volume to a Linode.
func (c *Client) AttachVolume(ctx context.Context, volumeID, linodeID int64) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/volumes/%d/attach", volumeID),
		map[string]any{"linode_id": linodeID}, nil)
}

// DetachVolume detaches a volume from any Linode.
func (c *Client) DetachVolume(ctx context.Context, volumeID int64) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/volumes/%d/detach", volumeID), map[string]any{}, nil)
}

// DeleteVolume permanently deletes a volume.
func (c *Client) DeleteVolume(ctx context.Context, volumeID int64) error {
	return c.do(ctx, http.MethodDelete, fmt.Sprintf("/volumes/%d", volumeID), nil, nil)
}

// GetInstance fetches a Linode instance (used to learn the appliance's actual
// region so launches and volumes default to it instead of a configured guess).
func (c *Client) GetInstance(ctx context.Context, id int64) (Instance, error) {
	var inst Instance
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/linode/instances/%d", id), nil, &inst)
	return inst, err
}

// DeleteInstance permanently deletes a Linode instance (used to clean up a
// previous cutover attempt before retrying, and on migration delete).
func (c *Client) DeleteInstance(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodDelete, fmt.Sprintf("/linode/instances/%d", id), nil, nil)
}

// CloneVolume clones a volume into a new immutable volume (the migration's
// "snapshot" artifact) and returns it.
func (c *Client) CloneVolume(ctx context.Context, volumeID int64, label string) (Volume, error) {
	var v Volume
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/volumes/%d/clone", volumeID),
		map[string]any{"label": label}, &v)
	return v, err
}

// WaitVolumeActive polls until the volume is "active" or ctx/timeout expires.
func (c *Client) WaitVolumeActive(ctx context.Context, id int64, timeout time.Duration) (Volume, error) {
	deadline := time.Now().Add(timeout)
	for {
		v, err := c.GetVolume(ctx, id)
		if err != nil {
			return v, err
		}
		if v.Status == "active" {
			return v, nil
		}
		if time.Now().After(deadline) {
			return v, fmt.Errorf("linode: volume %d not active after %s (status %q)", id, timeout, v.Status)
		}
		select {
		case <-ctx.Done():
			return v, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// Instance is a Linode compute instance.
type Instance struct {
	ID     int64    `json:"id"`
	Label  string   `json:"label"`
	Status string   `json:"status"`
	IPv4   []string `json:"ipv4"`
	Region string   `json:"region"`
}

// CreateInstance creates a bare (unbooted, no distribution) Linode that a
// migrated volume can be attached to and booted from.
func (c *Client) CreateInstance(ctx context.Context, label, region, typ string) (Instance, error) {
	var inst Instance
	err := c.do(ctx, http.MethodPost, "/linode/instances",
		map[string]any{"label": label, "region": region, "type": typ, "booted": false}, &inst)
	return inst, err
}

// CreateConfigBootingVolume creates a config profile that boots from an attached
// volume (GRUB 2) and returns the config id.
func (c *Client) CreateConfigBootingVolume(ctx context.Context, linodeID, volumeID int64, label string) (int64, error) {
	var cfg struct {
		ID int64 `json:"id"`
	}
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/linode/instances/%d/configs", linodeID),
		map[string]any{
			"label":       label,
			"kernel":      "linode/grub2",
			"devices":     map[string]any{"sda": map[string]any{"volume_id": volumeID}},
			"root_device": "/dev/sda",
			"helpers":     map[string]any{"network": true, "distro": false},
		}, &cfg)
	return cfg.ID, err
}

// CreateConfigBootingVolumes creates a config profile that boots from the first
// volume and attaches the rest as additional disks (sda, sdb, … up to sdh).
// Returns the config id. The OS on the boot volume mounts the data volumes via
// its fstab (typically by UUID, preserved in the clones).
//
// kernel selects how Linode boots: "linode/grub2" for a partitioned disk with a
// reinstalled bootloader, or a Linode-supplied kernel (e.g. "linode/latest-64bit")
// for a partitionless whole-disk root filesystem that has no on-disk bootloader.
// rootDevice is the device the kernel mounts as / (e.g. "/dev/sda").
func (c *Client) CreateConfigBootingVolumes(ctx context.Context, linodeID int64, volumeIDs []int64, label, kernel, rootDevice string) (int64, error) {
	if kernel == "" {
		kernel = "linode/grub2"
	}
	if rootDevice == "" {
		rootDevice = "/dev/sda"
	}
	slots := []string{"sda", "sdb", "sdc", "sdd", "sde", "sdf", "sdg", "sdh"}
	devices := map[string]any{}
	for i, vid := range volumeIDs {
		if i >= len(slots) {
			break
		}
		devices[slots[i]] = map[string]any{"volume_id": vid}
	}
	var cfg struct {
		ID int64 `json:"id"`
	}
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/linode/instances/%d/configs", linodeID),
		map[string]any{
			"label":       label,
			"kernel":      kernel,
			"devices":     devices,
			"root_device": rootDevice,
			"helpers":     map[string]any{"network": true, "distro": false},
		}, &cfg)
	return cfg.ID, err
}

// Boot boots a Linode into the given config.
func (c *Client) Boot(ctx context.Context, linodeID, configID int64) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/linode/instances/%d/boot", linodeID),
		map[string]any{"config_id": configID}, nil)
}

// ApplianceLinodeID asks the Linode Metadata Service for the id of the instance
// this process runs on (so the appliance can attach volumes to itself). Returns
// 0 if the metadata service is unavailable (e.g. running off-Linode).
func ApplianceLinodeID(ctx context.Context) (int64, error) {
	// Allow an explicit override for testing / non-metadata environments.
	if v := os.Getenv("APPLIANCE_LINODE_ID"); v != "" {
		var id int64
		_, err := fmt.Sscan(v, &id)
		return id, err
	}
	// Metadata service: token then instance lookup.
	hc := &http.Client{Timeout: 5 * time.Second}
	tokReq, _ := http.NewRequestWithContext(ctx, http.MethodPut, "http://169.254.169.254/v1/token", nil)
	tokReq.Header.Set("Metadata-Token-Expiry-Seconds", "60")
	tokResp, err := hc.Do(tokReq)
	if err != nil {
		return 0, fmt.Errorf("metadata token: %w", err)
	}
	tok, _ := io.ReadAll(tokResp.Body)
	tokResp.Body.Close()

	instReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://169.254.169.254/v1/instance", nil)
	instReq.Header.Set("Metadata-Token", strings.TrimSpace(string(tok)))
	instReq.Header.Set("Accept", "application/json")
	instResp, err := hc.Do(instReq)
	if err != nil {
		return 0, fmt.Errorf("metadata instance: %w", err)
	}
	defer instResp.Body.Close()
	var meta struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(instResp.Body).Decode(&meta); err != nil {
		return 0, err
	}
	return meta.ID, nil
}
