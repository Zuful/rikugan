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

func TestServeSweep(t *testing.T) {
	t.Setenv("RIKUGAN_SWEEP_INTERVAL", "")
	t.Setenv("RIKUGAN_SWEEP_NAMESPACES", "")
	t.Setenv("RIKUGAN_SWEEP_EXCLUDE", "batch, shop/legacy")
	t.Setenv("RIKUGAN_SWEEP_NOTIFY_WARNING", "true")
	t.Setenv("RIKUGAN_SWEEP_WARNING_PERSISTENCE", "2")
	cfg, err := serveSweep([]string{"shop"}, nil, nil)
	if err != nil || cfg == nil {
		t.Fatalf("%v %v", cfg, err)
	}
	if cfg.Interval != 0 || !reflect.DeepEqual(cfg.Namespaces, []string{"shop"}) ||
		!reflect.DeepEqual(cfg.Exclude, []string{"batch", "shop/legacy"}) ||
		!cfg.State.NotifyWarning || cfg.State.WarningPersistence != 2 {
		t.Errorf("config %+v", cfg)
	}

	t.Setenv("RIKUGAN_SWEEP_NAMESPACES", "a,b")
	t.Setenv("RIKUGAN_SWEEP_INTERVAL", "2h")
	if cfg, _ := serveSweep([]string{"shop"}, nil, nil); cfg.Interval != 2*time.Hour || len(cfg.Namespaces) != 2 {
		t.Errorf("overrides: %+v", cfg)
	}
	t.Setenv("RIKUGAN_SWEEP_INTERVAL", "off")
	if cfg, err := serveSweep(nil, nil, nil); cfg != nil || err != nil {
		t.Errorf("off: %v %v", cfg, err)
	}
	for _, bad := range []string{"4", "10s"} {
		t.Setenv("RIKUGAN_SWEEP_INTERVAL", bad)
		if _, err := serveSweep(nil, nil, nil); err == nil {
			t.Errorf("interval %q accepted", bad)
		}
	}
	t.Setenv("RIKUGAN_SWEEP_INTERVAL", "")
	t.Setenv("RIKUGAN_SWEEP_WARNING_PERSISTENCE", "0")
	if _, err := serveSweep(nil, nil, nil); err == nil {
		t.Error("persistence 0 accepted")
	}
}
