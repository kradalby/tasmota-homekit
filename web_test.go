package tasmotahomekit

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kradalby/tasmota-nefit/plugs"
	"tailscale.com/util/eventbus"
)

type fakePlugProvider struct {
	items map[string]struct {
		Plug  plugs.Plug
		State plugs.State
	}
}

func newFakePlugProvider() *fakePlugProvider {
	return &fakePlugProvider{
		items: map[string]struct {
			Plug  plugs.Plug
			State plugs.State
		}{
			"plug-1": {
				Plug: plugs.Plug{
					ID:      "plug-1",
					Name:    "Test Plug",
					Address: "1.2.3.4",
				},
				State: plugs.State{
					ID:          "plug-1",
					Name:        "Test Plug",
					On:          false,
					LastUpdated: time.Now(),
				},
			},
		},
	}
}

func (f *fakePlugProvider) Snapshot() map[string]struct {
	Plug  plugs.Plug
	State plugs.State
} {
	out := make(map[string]struct {
		Plug  plugs.Plug
		State plugs.State
	}, len(f.items))
	for id, item := range f.items {
		out[id] = item
	}
	return out
}

func (f *fakePlugProvider) Plug(id string) (plugs.Plug, plugs.State, bool) {
	item, ok := f.items[id]
	return item.Plug, item.State, ok
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestWebServer(t *testing.T) (*WebServer, *fakePlugProvider, chan plugs.CommandEvent) {
	t.Helper()

	bus := eventbus.New()
	provider := newFakePlugProvider()
	cmds := make(chan plugs.CommandEvent, 1)

	ws := NewWebServer(
		testLogger(),
		provider,
		cmds,
		bus,
		nil,
		"00102003",
		"QR",
	)

	t.Cleanup(func() {
		ws.Close()
	})

	return ws, provider, cmds
}

func TestHandleIndex(t *testing.T) {
	ws, _, _ := newTestWebServer(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	ws.HandleIndex(rec, req)

	res := rec.Result()
	defer func() {
		if err := res.Body.Close(); err != nil {
			t.Fatalf("closing response body: %v", err)
		}
	}()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusOK)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "Tasmota HomeKit Bridge") {
		t.Fatalf("response missing heading: %s", body)
	}
	if !strings.Contains(body, "Test Plug") {
		t.Fatalf("response missing plug name: %s", body)
	}
}

func TestHandleTogglePublishesCommand(t *testing.T) {
	ws, _, cmds := newTestWebServer(t)

	req := httptest.NewRequest(http.MethodPost, "/toggle/plug-1", strings.NewReader("action=on"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()

	ws.HandleToggle(rec, req)

	select {
	case cmd := <-cmds:
		if cmd.PlugID != "plug-1" || !cmd.On {
			t.Fatalf("unexpected command: %+v", cmd)
		}
	case <-time.After(time.Second):
		t.Fatal("expected command event")
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
}

type flushRecorder struct {
	*httptest.ResponseRecorder
}

func (f *flushRecorder) Flush() {}

func TestHandleSSE(t *testing.T) {
	ws, _, _ := newTestWebServer(t)

	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req = req.WithContext(ctx)
	rec := &flushRecorder{httptest.NewRecorder()}

	done := make(chan struct{})
	go func() {
		ws.HandleSSE(rec, req)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	ws.broadcastSSE("plug-1")
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SSE handler did not exit")
	}

	body := rec.Body.String()
	if !strings.Contains(body, "plug-1") {
		t.Fatalf("expected SSE payload, got %q", body)
	}
}

func TestHandleHealth(t *testing.T) {
	ws, _, _ := newTestWebServer(t)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	ws.HandleHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}

	var payload struct {
		Status string `json:"status"`
		Plugs  int    `json:"plugs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("health response invalid json: %v", err)
	}
	if payload.Status != "ok" || payload.Plugs != 1 {
		t.Fatalf("unexpected payload: %+v", payload)
	}
}

func TestHandleQRCode(t *testing.T) {
	ws, _, _ := newTestWebServer(t)

	req := httptest.NewRequest(http.MethodGet, "/qrcode", nil)
	rec := httptest.NewRecorder()

	ws.HandleQRCode(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "00102003") {
		t.Fatalf("expected PIN in response: %s", body)
	}
}
