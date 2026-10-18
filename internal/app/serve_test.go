package app

import (
	"context"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Zuful/rikugan/internal/watch"
	"github.com/Zuful/rikugan/internal/watch/journal"
	"github.com/Zuful/rikugan/pkg/provider"
)

func TestServeNamespaces(t *testing.T) {
	t.Setenv("RIKUGAN_SERVE_NAMESPACES", " shop, payments ,,")
	got, err := serveNamespaces()
	if err != nil || !reflect.DeepEqual(got, []string{"shop", "payments"}) {
		t.Errorf("got %v, %v", got, err)
	}

	// Outside a pod, with nothing configured: refuse rather than watch
	// every namespace.
	t.Setenv("RIKUGAN_SERVE_NAMESPACES", "")
	if _, err := serveNamespaces(); err == nil || !strings.Contains(err.Error(), "RIKUGAN_SERVE_NAMESPACES") {
		t.Errorf("unconfigured outside a pod: %v", err)
	}
}

type idleSource struct{ provider.DeploymentSource }

func (idleSource) WatchRollouts(context.Context) (<-chan provider.Rollout, error) {
	return make(chan provider.Rollout), nil
}

func TestRunWithAPIStopsWhenTheAPICannotListen(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()

	j, err := journal.Open(t.TempDir(), journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	daemon := &watch.Daemon{Source: idleSource{}, Journal: j}
	err = runWithAPI(ctx, daemon, &http.Server{Addr: busy.Addr().String(), ReadHeaderTimeout: time.Second})
	if err == nil || !strings.Contains(err.Error(), "verdicts API") {
		t.Errorf("got %v, want the listen error", err)
	}
	if ctx.Err() != nil {
		t.Error("runWithAPI waited for the context instead of stopping")
	}
}
