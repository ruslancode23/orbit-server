// Package objectstore stores attachment bytes in an S3-compatible store
// (SeaweedFS locally, S3 elsewhere) and signs the links people download by.
package objectstore

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type Client struct {
	s3      *s3.Client
	presign *s3.PresignClient
	bucket  string
}

// New connects to the store. The endpoint is given explicitly and paths are
// kept in the URL, because SeaweedFS has no per-bucket hostnames.
func New(ctx context.Context, endpoint, bucket, accessKey, secretKey string) (*Client, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"), // SeaweedFS ignores it; the SDK insists on one
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	return &Client{s3: client, presign: s3.NewPresignClient(client), bucket: bucket}, nil
}

func (c *Client) Put(ctx context.Context, key, mimeType string, body io.Reader, size int64) error {
	_, err := c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(key),
		Body:          body,
		ContentType:   aws.String(mimeType),
		ContentLength: aws.Int64(size),
	})
	if err != nil {
		return fmt.Errorf("put object: %w", err)
	}
	return nil
}

// SignedURL is a link that works for ttl and no longer. The download carries
// the original file name and is never rendered in the browser: an attachment
// is a file people save, and an inline render is where XSS starts.
func (c *Client) SignedURL(ctx context.Context, key, fileName string, ttl time.Duration) (string, error) {
	request, err := c.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket:                     aws.String(c.bucket),
		Key:                        aws.String(key),
		ResponseContentDisposition: aws.String(contentDisposition(fileName)),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("presign get object: %w", err)
	}
	return request.URL, nil
}

// SignedInlineURL is a link meant to be rendered, not downloaded: the browser
// shows the image in place. It is for content this server produced itself — a
// re-encoded avatar, never an upload passed through unchanged — and it asks the
// browser to keep it for a year, which is safe because every avatar lives under
// a key that changes on every upload. S3 caps a presigned link at seven days.
func (c *Client) SignedInlineURL(ctx context.Context, key, mimeType string, ttl time.Duration) (string, error) {
	request, err := c.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket:                     aws.String(c.bucket),
		Key:                        aws.String(key),
		ResponseContentDisposition: aws.String("inline"),
		ResponseContentType:        aws.String(mimeType),
		ResponseCacheControl:       aws.String("public, max-age=31536000, immutable"),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("presign inline object: %w", err)
	}
	return request.URL, nil
}

func (c *Client) Delete(ctx context.Context, key string) error {
	_, err := c.s3.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("delete object: %w", err)
	}
	return nil
}

// contentDisposition keeps the name readable and quotes it, so a name with a
// space or a comma cannot break the header.
func contentDisposition(fileName string) string {
	escaped := ""
	for _, r := range fileName {
		if r == '"' || r == '\\' || r < 0x20 {
			continue
		}
		escaped += string(r)
	}
	if escaped == "" {
		escaped = "file"
	}
	return fmt.Sprintf("attachment; filename=%q", escaped)
}
