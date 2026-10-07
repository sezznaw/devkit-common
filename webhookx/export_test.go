package webhookx

import (
	"net"
	"strconv"
	"testing"

	"github.com/sezznaw/devkit-common/kitexx"
)

func freePort(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
}

func startAll(rt *kitexx.Runtime) error { return kitexx.RunStarters(rt) }
