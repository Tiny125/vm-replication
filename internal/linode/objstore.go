package linode

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// Bucket identifies a Linode Object Storage bucket. The Linode API uses the
// cluster/region id as a path segment for object operations; we keep whatever
// the create call returns and fall back to Region.
type Bucket struct {
	Label    string `json:"label"`
	Region   string `json:"region"`
	Cluster  string `json:"cluster"`
	Hostname string `json:"hostname"`
}

func (b Bucket) pathSeg() string {
	if b.Cluster != "" {
		return b.Cluster
	}
	return b.Region
}

// ListBuckets returns every Object Storage bucket in the account (across all
// regions), used to pick a non-colliding audit-bucket name.
func (c *Client) ListBuckets(ctx context.Context) ([]Bucket, error) {
	var resp struct {
		Data []Bucket `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/object-storage/buckets?page_size=500", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// CreateBucket creates an Object Storage bucket in the given region. Requires a
// token with Object Storage read/write and Object Storage enabled on the account.
func (c *Client) CreateBucket(ctx context.Context, label, region string) (Bucket, error) {
	var b Bucket
	err := c.do(ctx, http.MethodPost, "/object-storage/buckets",
		map[string]any{"label": label, "region": region}, &b)
	return b, err
}

// ListObjects returns the names of every object in the bucket, following Linode's
// marker pagination. Used to empty a bucket before deleting it.
func (c *Client) ListObjects(ctx context.Context, b Bucket) ([]string, error) {
	var names []string
	marker := ""
	for {
		path := fmt.Sprintf("/object-storage/buckets/%s/%s/object-list?page_size=1000", b.pathSeg(), b.Label)
		if marker != "" {
			path += "&marker=" + url.QueryEscape(marker)
		}
		var resp struct {
			Data []struct {
				Name string `json:"name"`
			} `json:"data"`
			NextMarker  string `json:"next_marker"`
			IsTruncated bool   `json:"is_truncated"`
		}
		if err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return nil, err
		}
		for _, o := range resp.Data {
			names = append(names, o.Name)
		}
		if !resp.IsTruncated || resp.NextMarker == "" {
			return names, nil
		}
		marker = resp.NextMarker
	}
}

// objectDeleteURL mints a short-lived presigned URL to DELETE an object (same
// approach as objectPutURL: let Linode sign it so we never implement SigV4).
func (c *Client) objectDeleteURL(ctx context.Context, b Bucket, name string) (string, error) {
	var resp struct {
		URL string `json:"url"`
	}
	err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/object-storage/buckets/%s/%s/object-url", b.pathSeg(), b.Label),
		map[string]any{"method": "DELETE", "name": name}, &resp)
	if err != nil {
		return "", err
	}
	if resp.URL == "" {
		return "", fmt.Errorf("linode: empty delete object-url for %s/%s", b.Label, name)
	}
	return resp.URL, nil
}

// DeleteObject removes one object via a presigned DELETE URL. A 404 is treated as
// success (already gone), so emptying a bucket is idempotent.
func (c *Client) DeleteObject(ctx context.Context, b Bucket, name string) error {
	u, err := c.objectDeleteURL(ctx, b, name)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("object storage DELETE %s: %s: %s", name, resp.Status, bytes.TrimSpace(body))
	}
	return nil
}

// EmptyBucket deletes every object in the bucket (a bucket must be empty before
// it can be deleted).
func (c *Client) EmptyBucket(ctx context.Context, b Bucket) error {
	names, err := c.ListObjects(ctx, b)
	if err != nil {
		return err
	}
	for _, n := range names {
		if err := c.DeleteObject(ctx, b, n); err != nil {
			return err
		}
	}
	return nil
}

// DeleteBucket removes the bucket itself. Call EmptyBucket first — Linode refuses
// to delete a non-empty bucket.
func (c *Client) DeleteBucket(ctx context.Context, b Bucket) error {
	return c.do(ctx, http.MethodDelete,
		fmt.Sprintf("/object-storage/buckets/%s/%s", b.pathSeg(), b.Label), nil, nil)
}

// objectPutURL asks Linode for a short-lived presigned URL to PUT an object.
// This avoids implementing S3 SigV4 in the appliance: we mint a URL via the API
// (using the stored token) and then PUT bytes with a plain HTTP request.
func (c *Client) objectPutURL(ctx context.Context, b Bucket, name, contentType string) (string, error) {
	var resp struct {
		URL string `json:"url"`
	}
	err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/object-storage/buckets/%s/%s/object-url", b.pathSeg(), b.Label),
		map[string]any{"method": "PUT", "name": name, "content_type": contentType}, &resp)
	if err != nil {
		return "", err
	}
	if resp.URL == "" {
		return "", fmt.Errorf("linode: empty object-url for %s/%s", b.Label, name)
	}
	return resp.URL, nil
}

// PutObject uploads data to b/name (overwriting). It mints a presigned PUT URL
// then uploads the bytes.
func (c *Client) PutObject(ctx context.Context, b Bucket, name, contentType string, data []byte) error {
	url, err := c.objectPutURL(ctx, b, name, contentType)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("object storage PUT %s: %s: %s", name, resp.Status, bytes.TrimSpace(body))
	}
	return nil
}
