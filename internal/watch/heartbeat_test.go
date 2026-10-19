package watch

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zuful/rikugan/internal/watch/journal"
	"github.com/Zuful/rikugan/pkg/provider"
)

func TestHeartbeatFromTheLoop(t *testing.T) {
	var beats atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ping/s3cret" {
			beats.Add(1)
		}
	}))
	defer srv.Close()

	j, err := journal.Open(t.TempDir(), journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	d := &Daemon{
		Source: &fakeSource{events: make(chan provider.Rollout)}, Checker: &fakeChecker{}, Journal: j,
		Heartbeat: &Heartbeat{URL: srv.URL + "/ping/s3cret", Interval: 20 * time.Millisecond},
	}
	go func() { _ = d.Run(ctx) }()
	for beats.Load() < 3 {
		select {
		case <-ctx.Done():
			t.Fatalf("%d beats in 2 s, want one at start and one every 20 ms", beats.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestHeartbeatErrorsDoNotLeakTheURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	var logs bytes.Buffer
	hb := &Heartbeat{URL: srv.URL + "/ping/s3cret", Logger: slog.New(slog.NewTextHandler(&logs, nil))}
	hb.ping(context.Background())
	srv.Close()
	hb.ping(context.Background()) // unreachable now
	out := logs.String()
	if !strings.Contains(out, "HTTP 404") || !strings.Contains(out, "unreachable") {
		t.Errorf("logs:\n%s", out)
	}
	if strings.Contains(out, "s3cret") {
		t.Errorf("the URL leaked into the logs:\n%s", out)
	}
}
