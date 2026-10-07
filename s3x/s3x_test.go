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
