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

	"github.com/kradalby/tasmota-nefit/events"
	"github.com/kradalby/tasmota-nefit/plugs"
	"github.com/stretchr/testify/assert"
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

type mockPlugController struct {
	setPowerFunc func(ctx context.Context, plugID string, on bool) error
	refreshFunc  func(ctx context.Context)
}

func (m *mockPlugController) SetPower(ctx context.Context, plugID string, on bool) error {
	if m.setPowerFunc != nil {
		return m.setPowerFunc(ctx, plugID, on)
	}
	return nil
}

func (m *mockPlugController) RefreshAll(ctx context.Context) {
	if m.refreshFunc != nil {
		m.refreshFunc(ctx)
	}
}

func newTestWebServer(t *testing.T) (*WebServer, *fakePlugProvider, *mockPlugController, *events.Bus) {
	t.Helper()

	bus, err := events.New(testLogger())
	if err != nil {
		t.Fatalf("events.New() error = %v", err)
	}
	provider := newFakePlugProvider()
	controller := &mockPlugController{}

	ws := NewWebServer(
		testLogger(),
		provider,
		controller,
		bus,
		nil,
		"00102003",
		"QR",
		nil,
	)

	t.Cleanup(func() {
		ws.Close()
	})

	return ws, provider, controller, bus
}

func TestHandleIndex(t *testing.T) {
	ws, _, _, _ := newTestWebServer(t)

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

func TestHandleToggleCallsSetPower(t *testing.T) {
	ws, _, controller, _ := newTestWebServer(t)

	called := false
	controller.setPowerFunc = func(ctx context.Context, plugID string, on bool) error {
		called = true
		if plugID != "plug-1" {
			t.Errorf("plugID = %s, want plug-1", plugID)
		}
		if !on {
			t.Errorf("on = %v, want true", on)
		}
		return nil
	}

	req := httptest.NewRequest(http.MethodPost, "/toggle/plug-1", strings.NewReader("action=on"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()

	ws.HandleToggle(rec, req)

	if !called {
		t.Fatal("SetPower was not called")
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
	ws, _, _, bus := newTestWebServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ws.Start(ctx)

	req := httptest.NewRequest(http.MethodGet, "/events", nil).WithContext(ctx)
	rec := &flushRecorder{httptest.NewRecorder()}

	done := make(chan struct{})
	go func() {
		ws.HandleSSE(rec, req)
		close(done)
	}()

	client, err := bus.Client(events.ClientPlugManager)
	if err != nil {
		t.Fatalf("bus.Client() error = %v", err)
	}
	bus.PublishStateUpdate(client, events.StateUpdateEvent{
		PlugID:      "plug-1",
		Name:        "Test Plug",
		On:          true,
		LastUpdated: time.Now(),
		LastSeen:    time.Now(),
	})

	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		if !strings.Contains(rec.Body.String(), "\"plug_id\":\"plug-1\"") {
			assert.Fail(c, "missing SSE event")
		}
	}, time.Second, 20*time.Millisecond)

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SSE handler did not exit")
	}

	var lastData string
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, "data:") {
			lastData = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	if lastData == "" {
		t.Fatalf("expected SSE payload, got %q", rec.Body.String())
	}

	var evt events.StateUpdateEvent
	if err := json.Unmarshal([]byte(lastData), &evt); err != nil {
		t.Fatalf("failed to unmarshal SSE payload: %v", err)
	}
	if evt.PlugID != "plug-1" || !evt.On {
		t.Fatalf("unexpected SSE event: %+v", evt)
	}
}

func TestHandleEventBusDebugShowsStatuses(t *testing.T) {
	ws, _, _, bus := newTestWebServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ws.Start(ctx)

	client, err := bus.Client(events.ClientHAP)
	if err != nil {
		t.Fatalf("Client() error = %v", err)
	}

	bus.PublishConnectionStatus(client, events.ConnectionStatusEvent{
		Timestamp: time.Now(),
		Component: "hap",
		Status:    events.ConnectionStatusConnected,
	})

	time.Sleep(20 * time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/debug/eventbus", nil)
	rec := httptest.NewRecorder()

	ws.HandleEventBusDebug(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "Component Status") {
		t.Fatalf("expected component status table, got %q", body)
	}
	if !strings.Contains(body, "hap") {
		t.Fatalf("expected component entry in debug output, got %q", body)
	}
}

func TestHandleHealth(t *testing.T) {
	ws, _, _, _ := newTestWebServer(t)

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
	ws, _, _, _ := newTestWebServer(t)

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

func TestHandleEventBusDebug(t *testing.T) {
	ws, _, _, bus := newTestWebServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ws.Start(ctx)

	client, err := bus.Client(events.ClientPlugManager)
	if err != nil {
		t.Fatalf("bus.Client() error = %v", err)
	}
	bus.PublishStateUpdate(client, events.StateUpdateEvent{
		PlugID:      "plug-1",
		Name:        "Test Plug",
		On:          true,
		LastUpdated: time.Now(),
		LastSeen:    time.Now(),
	})

	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		ws.stateMu.RLock()
		defer ws.stateMu.RUnlock()
		_, ok := ws.currentState["plug-1"]
		assert.True(c, ok)
	}, time.Second, 20*time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/debug/eventbus", nil)
	rec := httptest.NewRecorder()

	ws.HandleEventBusDebug(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}

	if !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("Content-Type = %s, want text/html", rec.Header().Get("Content-Type"))
	}

	body := rec.Body.String()
	if !strings.Contains(body, "plug-1") {
		t.Fatalf("expected plug info in response: %s", body)
	}
}
