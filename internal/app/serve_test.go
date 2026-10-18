package app

import (
	"reflect"
	"strings"
	"testing"
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
