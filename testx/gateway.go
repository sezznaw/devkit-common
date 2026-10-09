package testx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
)

// Response is what a gateway route answered: the HTTP status and the
// envelope {code, msg, data}.
type Response struct {
	Status int
	Code   int             `json:"code"`
	Msg    string          `json:"msg"`
	Data   json.RawMessage `json:"data"`
	Body   string
}

// Decode reads data into v.
func (r Response) Decode(t testing.TB, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Data, v); err != nil {
		t.Fatalf("testx: decode data %s: %v", r.Data, err)
	}
}

// Header is a request header for Post / Get.
type Header = ut.Header

// Bearer is the Authorization header of an access token.
func Bearer(token string) Header { return Header{Key: "Authorization", Value: "Bearer " + token} }

// From is the client IP the request appears to come from (the guard's
// rate limits count per IP).
func From(ip string) Header { return Header{Key: "X-Forwarded-For", Value: ip} }

// Post sends body (JSON-encoded) to path of the server in-process, no
// network, and returns the parsed response. A body that is not JSON
// (a 413, a 429 from the guard) leaves Code -1 and the text in Body.
func Post(t testing.TB, h *server.Hertz, path string, body any, headers ...Header) Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("testx: encode body: %v", err)
	}
	hs := append([]Header{{Key: "Content-Type", Value: "application/json"}}, headers...)
	w := ut.PerformRequest(h.Engine, "POST", path, &ut.Body{Body: bytes.NewReader(b), Len: len(b)}, hs...)
	return parse(w)
}

// Get requests path.
func Get(t testing.TB, h *server.Hertz, path string, headers ...Header) Response {
	t.Helper()
	w := ut.PerformRequest(h.Engine, "GET", path, nil, headers...)
	return parse(w)
}

func parse(w *ut.ResponseRecorder) Response {
	r := Response{Status: w.Code, Code: -1, Body: w.Body.String()}
	_ = json.Unmarshal(w.Body.Bytes(), &r)
	return r
}

// MustCode fails the test unless the response has HTTP status and code.
func (r Response) MustCode(t testing.TB, status, code int) Response {
	t.Helper()
	if r.Status != status || r.Code != code {
		t.Fatalf("testx: want HTTP %d code %d, got HTTP %d code %d: %s", status, code, r.Status, r.Code, r.Body)
	}
	return r
}

// String is the response in one line, for a failure message.
func (r Response) String() string { return fmt.Sprintf("HTTP %d %s", r.Status, r.Body) }
