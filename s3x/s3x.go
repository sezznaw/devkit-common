// Package s3x is object storage for a service (SeaweedFS on dev, any S3 in
// production): files a service keeps (avatars, documents, exports) go into
// one bucket under a prefix of the service's own. The configuration decides
// whether a service has it (`s3.enabled`) and where (static, or the
// deployment's datasource table); the Runtime opens it; `rt.S3` is the
// client. Every call is a span, a log record at debug and a metric.
package s3x

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"go.opentelemetry.io/contrib/instrumentation/github.com/aws/aws-sdk-go-v2/otelaws"

	"github.com/sezznaw/devkit-common/config"
	"github.com/sezznaw/devkit-common/metricsx"
	"github.com/sezznaw/devkit-common/zlog"
)

// Config is the `s3:` section of a service configuration.
type Config struct {
	// Enabled opens the client at start; rt.S3 is nil otherwise.
	Enabled bool `yaml:"enabled"`
	// Source: "static" (the fields below) or "platform" (the datasource
	// row of kind s3 for this deployment: endpoint, bucket, and the names
	// of the environment variables that hold the keys).
	Source string `yaml:"source"`
	// Endpoint, Bucket, AccessKey, SecretKey, Region: with source static.
	// Endpoint "http://127.0.0.1:8333" for a local SeaweedFS
	// (docker run -d -p 8333:8333 chrislusf/seaweedfs server -s3).
	Endpoint  string `yaml:"endpoint"`
	Bucket    string `yaml:"bucket"`
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
	Region    string `yaml:"region"`
	// PathStyle addresses the bucket in the path (SeaweedFS, MinIO: true,
	// the default) instead of as a subdomain (AWS: false).
	PathStyle *bool `yaml:"path_style"`
	// Prefix is the folder of this service inside the bucket. Default: the
	// service name. Every key a service uses is under it, so services do
	// not step on each other's files.
	Prefix string `yaml:"prefix"`
	// Timeout of one request. Default 30s (uploads can be large).
	Timeout config.Duration `yaml:"timeout"`
}

const (
	SourceStatic   = "static"
	SourcePlatform = "platform"
	defaultTimeout = 30 * time.Second
	defaultRegion  = "us-east-1"
)

func (c Config) SourceName() string {
	if c.Source == "" {
		return SourceStatic
	}
	return c.Source
}

// Validate checks the section.
func (c Config) Validate() error {
	switch c.SourceName() {
	case SourceStatic:
		if c.Endpoint == "" || c.Bucket == "" {
			return fmt.Errorf("s3x: s3.source static needs s3.endpoint and s3.bucket")
		}
	case SourcePlatform:
	default:
		return fmt.Errorf("s3x: s3.source %q: static or platform", c.Source)
	}
	return nil
}

// Target is where the client connects, however the configuration said it.
type Target struct {
	Endpoint  string // scheme://host:port
	Bucket    string
	AccessKey string
	SecretKey string
	Region    string
	PathStyle bool
}

// Static is the Target of a static configuration.
func (c Config) Static() Target {
	return Target{Endpoint: c.Endpoint, Bucket: c.Bucket, AccessKey: c.AccessKey, SecretKey: c.SecretKey, Region: c.Region, PathStyle: c.PathStyle == nil || *c.PathStyle}
}

// TargetFromRow builds the Target from a datasource row of kind s3: host and
// port, db_name as the bucket, the access key from the variable named by
// password_env (already read: accessKey), and params
// "path_style=true&insecure=true&secret_env=S3_SECRET_ACCESS_KEY".
func TargetFromRow(addr, bucket, accessKey, params string, getenv func(string) string) (Target, error) {
	q, err := url.ParseQuery(params)
	if err != nil {
		return Target{}, fmt.Errorf("s3x: datasource params %q: %w", params, err)
	}
	t := Target{Bucket: bucket, AccessKey: accessKey, PathStyle: q.Get("path_style") != "false", Region: q.Get("region")}
	scheme := "https"
	if q.Get("insecure") == "true" {
		scheme = "http"
	}
	t.Endpoint = scheme + "://" + addr
	if env := q.Get("secret_env"); env != "" {
		if t.SecretKey = getenv(env); t.SecretKey == "" {
			return Target{}, fmt.Errorf("s3x: the S3 secret key is to come from the environment variable %q, which is not set", env)
		}
	}
	if t.Bucket == "" {
		return Target{}, fmt.Errorf("s3x: the datasource row of kind s3 has no db_name (the bucket)")
	}
	return t, nil
}

// Client is one bucket seen through one prefix.
type Client struct {
	api     *s3.Client
	presign *s3.PresignClient
	bucket  string
	prefix  string
	timeout time.Duration
}

// Object is what Stat and Get say about an object.
type Object struct {
	Key          string
	Size         int64
	ContentType  string
	LastModified time.Time
	ETag         string
}

// ErrNotFound: no object under that key.
var ErrNotFound = errors.New("s3x: object not found")

// Open connects, checks the bucket exists (HeadBucket, which also verifies
// the credentials) and returns the client.
func Open(ctx context.Context, t Target, cfg Config, service string) (*Client, error) {
	if t.Endpoint == "" || t.Bucket == "" {
		return nil, fmt.Errorf("s3x: target is incomplete: endpoint=%q bucket=%q", t.Endpoint, t.Bucket)
	}
	region := t.Region
	if region == "" {
		region = defaultRegion
	}
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
	if t.AccessKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(t.AccessKey, t.SecretKey, "")))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("s3x: aws config: %w", err)
	}
	otelaws.AppendMiddlewares(&awsCfg.APIOptions)
	api := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(t.Endpoint)
		o.UsePathStyle = t.PathStyle
	})
	prefix := strings.Trim(cfg.Prefix, "/")
	if prefix == "" {
		prefix = service
	}
	c := &Client{api: api, presign: s3.NewPresignClient(api), bucket: t.Bucket, prefix: prefix, timeout: cfg.Timeout.Or(defaultTimeout)}
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := api.HeadBucket(hctx, &s3.HeadBucketInput{Bucket: aws.String(t.Bucket)}); err != nil {
		return nil, fmt.Errorf("s3x: bucket %q at %s: %w (does it exist, and are the keys right?)", t.Bucket, t.Endpoint, err)
	}
	return c, nil
}

// Bucket and Prefix say where the files are.
func (c *Client) Bucket() string { return c.bucket }
func (c *Client) Prefix() string { return c.prefix }

// Raw is the SDK client, for anything the methods below do not cover.
func (c *Client) Raw() *s3.Client { return c.api }

func (c *Client) full(key string) string { return c.prefix + "/" + strings.TrimLeft(key, "/") }

func (c *Client) do(ctx context.Context, op, key string, fn func(ctx context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	start := time.Now()
	err := fn(ctx)
	status := "ok"
	if err != nil {
		status = "error"
		if errors.Is(err, ErrNotFound) {
			status = "not_found"
		}
	}
	metricsx.S3Requests.WithLabelValues(op, status).Inc()
	metricsx.S3Duration.WithLabelValues(op).Observe(time.Since(start).Seconds())
	log := zlog.Ctx(ctx)
	if err != nil && status == "error" {
		log.Error("s3 "+op+" failed", zlog.Str("bucket", c.bucket), zlog.Str("key", c.full(key)), zlog.Dur("latency", time.Since(start)), zlog.Err(err))
	} else {
		log.Debug("s3 "+op, zlog.Str("bucket", c.bucket), zlog.Str("key", c.full(key)), zlog.Str("status", status), zlog.Dur("latency", time.Since(start)))
	}
	return err
}

// Put stores r under key with the content type. Keys are paths like
// "avatars/42.png", under the service's prefix.
func (c *Client) Put(ctx context.Context, key string, r io.Reader, contentType string) error {
	return c.do(ctx, "put", key, func(ctx context.Context) error {
		in := &s3.PutObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(c.full(key)), Body: r}
		if contentType != "" {
			in.ContentType = aws.String(contentType)
		}
		_, err := c.api.PutObject(ctx, in)
		return err
	})
}

// PutBytes is Put for a byte slice.
func (c *Client) PutBytes(ctx context.Context, key string, data []byte, contentType string) error {
	return c.Put(ctx, key, bytes.NewReader(data), contentType)
}

// Get returns the object's content (close it) and its metadata.
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, *Object, error) {
	var body io.ReadCloser
	var obj *Object
	err := c.do(ctx, "get", key, func(ctx context.Context) error {
		out, err := c.api.GetObject(context.WithoutCancel(ctx), &s3.GetObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(c.full(key))})
		if err != nil {
			return mapErr(err)
		}
		body = out.Body
		obj = &Object{Key: key, Size: aws.ToInt64(out.ContentLength), ContentType: aws.ToString(out.ContentType), LastModified: aws.ToTime(out.LastModified), ETag: aws.ToString(out.ETag)}
		return nil
	})
	return body, obj, err
}

// GetBytes is Get for small objects.
func (c *Client) GetBytes(ctx context.Context, key string) ([]byte, error) {
	body, _, err := c.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(body)
}

// Stat returns the metadata without the content; ErrNotFound when absent.
func (c *Client) Stat(ctx context.Context, key string) (*Object, error) {
	var obj *Object
	err := c.do(ctx, "stat", key, func(ctx context.Context) error {
		out, err := c.api.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(c.full(key))})
		if err != nil {
			return mapErr(err)
		}
		obj = &Object{Key: key, Size: aws.ToInt64(out.ContentLength), ContentType: aws.ToString(out.ContentType), LastModified: aws.ToTime(out.LastModified), ETag: aws.ToString(out.ETag)}
		return nil
	})
	return obj, err
}

// Delete removes the object; deleting what is not there is not an error.
func (c *Client) Delete(ctx context.Context, key string) error {
	return c.do(ctx, "delete", key, func(ctx context.Context) error {
		_, err := c.api.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(c.full(key))})
		return err
	})
}

// List returns the keys under dir (relative to the prefix), at most limit
// (0 = 1000).
func (c *Client) List(ctx context.Context, dir string, limit int) ([]Object, error) {
	var out []Object
	err := c.do(ctx, "list", dir, func(ctx context.Context) error {
		max := int32(1000)
		if limit > 0 && limit < 1000 {
			max = int32(limit)
		}
		p := c.full(strings.TrimRight(dir, "/")) + "/"
		res, err := c.api.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(c.bucket), Prefix: aws.String(p), MaxKeys: aws.Int32(max)})
		if err != nil {
			return err
		}
		for _, o := range res.Contents {
			out = append(out, Object{Key: strings.TrimPrefix(aws.ToString(o.Key), c.prefix+"/"), Size: aws.ToInt64(o.Size), LastModified: aws.ToTime(o.LastModified), ETag: aws.ToString(o.ETag)})
		}
		return nil
	})
	return out, err
}

// PresignGet returns a URL from which anyone can download the object for
// ttl: what a client gets instead of the file going through the service.
func (c *Client) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	var u string
	err := c.do(ctx, "presign_get", key, func(ctx context.Context) error {
		req, err := c.presign.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(c.full(key))}, s3.WithPresignExpires(ttl))
		if err != nil {
			return err
		}
		u = req.URL
		return nil
	})
	return u, err
}

// PresignPut returns a URL a client can PUT the file to directly for ttl,
// with the content type it must send.
func (c *Client) PresignPut(ctx context.Context, key, contentType string, ttl time.Duration) (string, error) {
	var u string
	err := c.do(ctx, "presign_put", key, func(ctx context.Context) error {
		in := &s3.PutObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(c.full(key))}
		if contentType != "" {
			in.ContentType = aws.String(contentType)
		}
		req, err := c.presign.PresignPutObject(ctx, in, s3.WithPresignExpires(ttl))
		if err != nil {
			return err
		}
		u = req.URL
		return nil
	})
	return u, err
}

func mapErr(err error) error {
	var nf *types.NotFound
	var nk *types.NoSuchKey
	if errors.As(err, &nf) || errors.As(err, &nk) {
		return ErrNotFound
	}
	var re interface{ HTTPStatusCode() int }
	if errors.As(err, &re) && re.HTTPStatusCode() == 404 {
		return ErrNotFound
	}
	return err
}
