package s3x

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/sezznaw/devkit-common/metricsx"
	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

func fake(t *testing.T) string {
	backend := s3mem.New()
	backend.CreateBucket("assets")
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestPutGetStatListDeleteUnderThePrefix(t *testing.T) {
	zlogtest.Capture(t)
	ctx := context.Background()
	c, err := Open(ctx, Target{Endpoint: fake(t), Bucket: "assets", AccessKey: "k", SecretKey: "s", PathStyle: true}, Config{}, "ser-user")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.PutBytes(ctx, "avatars/42.png", []byte("png-bytes"), "image/png"); err != nil {
		t.Fatal(err)
	}
	b, err := c.GetBytes(ctx, "avatars/42.png")
	if err != nil || string(b) != "png-bytes" {
		t.Fatalf("get: %q %v", b, err)
	}
	o, err := c.Stat(ctx, "avatars/42.png")
	if err != nil || o.Size != 9 || o.ContentType != "image/png" {
		t.Fatalf("stat: %+v %v", o, err)
	}
	list, err := c.List(ctx, "avatars", 0)
	if err != nil || len(list) != 1 || list[0].Key != "avatars/42.png" {
		t.Fatalf("list: %+v %v", list, err)
	}
	if _, err := c.Stat(ctx, "avatars/none.png"); !errors.Is(err, ErrNotFound) {
		t.Errorf("stat missing: %v", err)
	}
	if _, err := c.GetBytes(ctx, "avatars/none.png"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get missing: %v", err)
	}
	u, err := c.PresignGet(ctx, "avatars/42.png", 10*time.Minute)
	if err != nil || !strings.Contains(u, "/assets/ser-user/avatars/42.png") || !strings.Contains(u, "X-Amz-Signature") {
		t.Fatalf("presign: %q %v", u, err)
	}
	resp, err := http.Get(u)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("download through the presigned url: %v %v", err, resp)
	}
	if err := c.Delete(ctx, "avatars/42.png"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Stat(ctx, "avatars/42.png"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: %v", err)
	}
	if got := testutil.ToFloat64(metricsx.S3Requests.WithLabelValues("put", "ok")); got < 1 {
		t.Errorf("put counted %v", got)
	}
	if got := testutil.ToFloat64(metricsx.S3Requests.WithLabelValues("stat", "not_found")); got < 1 {
		t.Errorf("not_found counted %v", got)
	}
}

func TestOpenChecksTheBucket(t *testing.T) {
	zlogtest.Capture(t)
	if _, err := Open(context.Background(), Target{Endpoint: fake(t), Bucket: "nope", AccessKey: "k", SecretKey: "s", PathStyle: true}, Config{}, "x"); err == nil || !strings.Contains(err.Error(), `bucket "nope"`) {
		t.Errorf("missing bucket: %v", err)
	}
}

func TestTargetFromRow(t *testing.T) {
	env := map[string]string{"S3_SECRET_ACCESS_KEY": "sek"}
	tg, err := TargetFromRow("seaweedfs:8333", "sportsbook-dev-assets", "ak", "path_style=true&insecure=true&secret_env=S3_SECRET_ACCESS_KEY", func(k string) string { return env[k] })
	if err != nil || tg.Endpoint != "http://seaweedfs:8333" || tg.Bucket != "sportsbook-dev-assets" || tg.AccessKey != "ak" || tg.SecretKey != "sek" || !tg.PathStyle {
		t.Fatalf("%+v %v", tg, err)
	}
	if _, err := TargetFromRow("h:1", "b", "ak", "secret_env=MISSING", func(string) string { return "" }); err == nil {
		t.Error("missing secret env accepted")
	}
	tg, _ = TargetFromRow("s3.amazonaws.com", "b", "ak", "path_style=false&region=ap-east-1", func(string) string { return "" })
	if tg.Endpoint != "https://s3.amazonaws.com" || tg.PathStyle || tg.Region != "ap-east-1" {
		t.Errorf("aws style: %+v", tg)
	}
}

func fakeTwo(t *testing.T) string {
	backend := s3mem.New()
	backend.CreateBucket("assets")
	backend.CreateBucket("public")
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestPublicBucketAndURLs(t *testing.T) {
	zlogtest.Capture(t)
	ctx := context.Background()
	ep := fakeTwo(t)
	c, err := Open(ctx, Target{Endpoint: ep, Bucket: "assets", PublicBucket: "public", PublicEndpoint: "http://s3.example.com", AccessKey: "k", SecretKey: "s", PathStyle: true}, Config{}, "ser-member")
	if err != nil {
		t.Fatal(err)
	}
	pub := c.Public()
	if pub == nil || !pub.IsPublic() || c.IsPublic() {
		t.Fatalf("public client: %v", pub)
	}
	if err := pub.PutBytes(ctx, "avatars/42.png", []byte("png"), "image/png"); err != nil {
		t.Fatal(err)
	}
	// The object went to the public bucket, under the same prefix; the private bucket does not have it.
	if b, err := pub.GetBytes(ctx, "avatars/42.png"); err != nil || string(b) != "png" {
		t.Fatalf("public get: %q %v", b, err)
	}
	if _, err := c.Stat(ctx, "avatars/42.png"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("private bucket should not have it: %v", err)
	}
	// URL is built on the public endpoint, bucket and prefix; the private bucket has no URL.
	if got := pub.URL("avatars/42 b.png"); got != "http://s3.example.com/public/ser-member/avatars/42%20b.png" {
		t.Fatalf("url: %s", got)
	}
	if c.URL("x") != "" {
		t.Fatal("private bucket must not give a plain URL")
	}
	// Presigned URLs are signed for the public endpoint, on both buckets.
	for _, cl := range []*Client{c, pub} {
		u, err := cl.PresignGet(ctx, "avatars/42.png", time.Minute)
		if err != nil || !strings.HasPrefix(u, "http://s3.example.com/"+cl.Bucket()+"/ser-member/avatars/42.png?") {
			t.Fatalf("presign: %s %v", u, err)
		}
	}
}

func TestPublicURLOverridesAndVirtualHost(t *testing.T) {
	zlogtest.Capture(t)
	ctx := context.Background()
	ep := fakeTwo(t)
	c, err := Open(ctx, Target{Endpoint: ep, Bucket: "assets", PublicBucket: "public", PublicURL: "https://cdn.example.com/", AccessKey: "k", SecretKey: "s", PathStyle: true}, Config{}, "ser-member")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Public().URL("/banners/1.png"); got != "https://cdn.example.com/ser-member/banners/1.png" {
		t.Fatalf("cdn url: %s", got)
	}
	if got := publicBase("https://s3.ap-east-1.amazonaws.com", "assets", false); got != "https://assets.s3.ap-east-1.amazonaws.com" {
		t.Fatalf("virtual host: %s", got)
	}
}

func TestOpenWithoutPublicBucketAndWithTheSameBucket(t *testing.T) {
	zlogtest.Capture(t)
	ctx := context.Background()
	ep := fakeTwo(t)
	c, err := Open(ctx, Target{Endpoint: ep, Bucket: "assets", AccessKey: "k", SecretKey: "s", PathStyle: true}, Config{}, "ser-member")
	if err != nil || c.Public() != nil {
		t.Fatalf("no public bucket: %v %v", c.Public(), err)
	}
	if _, err := Open(ctx, Target{Endpoint: ep, Bucket: "assets", PublicBucket: "assets", AccessKey: "k", SecretKey: "s", PathStyle: true}, Config{}, "ser-member"); err == nil || !strings.Contains(err.Error(), "must not be the private one") {
		t.Fatalf("same bucket must be refused: %v", err)
	}
	if _, err := Open(ctx, Target{Endpoint: ep, Bucket: "assets", PublicBucket: "missing", AccessKey: "k", SecretKey: "s", PathStyle: true}, Config{}, "ser-member"); err == nil || !strings.Contains(err.Error(), "public bucket") {
		t.Fatalf("missing public bucket must fail at start: %v", err)
	}
}

func TestTargetFromRowPublicParams(t *testing.T) {
	tg, err := TargetFromRow("s3.internal:8333", "assets", "ak", "path_style=true&insecure=true&secret_env=SK&public_bucket=public&public_endpoint=http://s3.example.com&public_url=https://cdn.example.com", func(string) string { return "sk" })
	if err != nil {
		t.Fatal(err)
	}
	if tg.Endpoint != "http://s3.internal:8333" || tg.PublicBucket != "public" || tg.PublicEndpoint != "http://s3.example.com" || tg.PublicURL != "https://cdn.example.com" {
		t.Fatalf("%+v", tg)
	}
	if st := (Config{Endpoint: "e", Bucket: "b", PublicBucket: "p", PublicEndpoint: "pe", PublicURL: "pu"}).Static(); st.PublicBucket != "p" || st.PublicEndpoint != "pe" || st.PublicURL != "pu" {
		t.Fatalf("%+v", st)
	}
}
