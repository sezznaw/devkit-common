package nacosx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The SDK carries on with an account Nacos rejects, and reports a registration
// that Nacos answered with 403 as a success. So the account is checked here.
func TestCheckAccount(t *testing.T) {
	nacos := func(auth string) string {
		mux := http.NewServeMux()
		mux.HandleFunc("/nacos/v1/console/server/state", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"version":"2.4.3","auth_enabled":"` + auth + `"}`))
		})
		mux.HandleFunc("/nacos/v1/auth/users/login", func(w http.ResponseWriter, r *http.Request) {
			if r.FormValue("username") == "nacos" && r.FormValue("password") == "right" {
				w.Write([]byte(`{"accessToken":"t"}`))
				return
			}
			http.Error(w, "unknown user!", http.StatusForbidden)
		})
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		return strings.TrimPrefix(srv.URL, "http://")
	}
	open, closed := nacos("false"), nacos("true")

	for _, c := range []struct {
		name, addr, user, pass string
		want                   []string // nil: accepted
	}{
		{"no authentication, no account", open, "", "", nil},
		{"no authentication, an account nobody checks", open, "nacos", "whatever", nil},
		{"the right account", closed, "nacos", "right", nil},
		{"no account", closed, "", "", []string{"requires an account", "NACOS_USERNAME"}},
		{"the wrong password", closed, "nacos", "wrong", []string{`does not accept the account "nacos"`, "unknown user!", "NACOS_PASSWORD"}},
		{"not a Nacos that says what it wants", "127.0.0.1:1", "", "", nil},
	} {
		err := checkAccount(c.addr, Config{Username: c.user, Password: c.pass}, time.Second)
		if c.want == nil {
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: accepted", c.name)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: %q is missing in: %v", c.name, w, err)
			}
		}
		if strings.Contains(err.Error(), "wrong") {
			t.Errorf("%s: the password must not be in the error: %v", c.name, err)
		}
	}
}
