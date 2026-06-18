package linode

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
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
