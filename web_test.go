package tasmotahomekit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/kradalby/tasmota-homekit/events"
	"github.com/kradalby/tasmota-homekit/plugs"
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
	maps.Copy(out, f.items)
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

func addPlug(f *fakePlugProvider, p plugs.Plug, s plugs.State) {
	s.ID = p.ID
	f.items[p.ID] = struct {
		Plug  plugs.Plug
		State plugs.State
	}{p, s}
}

func TestRoutes(t *testing.T) {
	ws, _, _, _ := newTestWebServer(t)
	mux := http.NewServeMux()
	ws.registerRoutes(mux)

	// /events streams until the request ends.
	ended, cancel := context.WithCancel(t.Context())
	cancel()

	tests := []struct {
		method   string
		path     string
		wantCode int
		wantType string
		wantBody string
	}{
		{http.MethodGet, "/", http.StatusOK, "text/html", "Tasmota HomeKit Bridge"},
		{http.MethodPost, "/toggle/plug-1", http.StatusSeeOther, "", ""},
		{http.MethodGet, "/events", http.StatusOK, "text/event-stream", ""},
		{http.MethodGet, "/health", http.StatusOK, "application/json", `"status":"ok"`},
		{http.MethodGet, "/qrcode", http.StatusOK, "text/plain", "00102003"},
		{http.MethodGet, "/debug/eventbus", http.StatusOK, "text/html", "EventBus Debug"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequestWithContext(ended, tt.method, tt.path, nil))

			assert.Equal(t, tt.wantCode, rec.Code)
			assert.Contains(t, rec.Header().Get("Content-Type"), tt.wantType)
			assert.Contains(t, rec.Body.String(), tt.wantBody)
		})
	}
}

func TestHandleToggle(t *testing.T) {
	tests := []struct {
		name     string
		method   string
		path     string
		form     string
		htmx     bool
		setErr   error
		wantCode int
		wantOn   *bool // nil: SetPower must not be called
	}{
		{"on", http.MethodPost, "/toggle/plug-1", "action=on", true, nil, http.StatusOK, new(true)},
		{"off", http.MethodPost, "/toggle/plug-1", "action=off", true, nil, http.StatusOK, new(false)},
		{"missing action turns off", http.MethodPost, "/toggle/plug-1", "", true, nil, http.StatusOK, new(false)},
		{"non-htmx redirects", http.MethodPost, "/toggle/plug-1", "action=on", false, nil, http.StatusSeeOther, new(true)},
		{"unknown plug", http.MethodPost, "/toggle/nope", "action=on", true, nil, http.StatusNotFound, nil},
		{"web disabled plug", http.MethodPost, "/toggle/hidden", "action=on", true, nil, http.StatusNotFound, nil},
		{"set power fails", http.MethodPost, "/toggle/plug-1", "action=on", true, errors.New("unreachable"), http.StatusInternalServerError, new(true)},
		{"wrong method", http.MethodGet, "/toggle/plug-1", "", true, nil, http.StatusMethodNotAllowed, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ws, provider, controller, _ := newTestWebServer(t)
			addPlug(provider, plugs.Plug{ID: "hidden", Name: "Hidden", Web: new(false)}, plugs.State{})

			var gotOn *bool
			controller.setPowerFunc = func(_ context.Context, plugID string, on bool) error {
				assert.Equal(t, "plug-1", plugID)
				gotOn = &on
				return tt.setErr
			}

			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.form))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if tt.htmx {
				req.Header.Set("HX-Request", "true")
			}
			rec := httptest.NewRecorder()

			ws.HandleToggle(rec, req)

			assert.Equal(t, tt.wantCode, rec.Code)
			assert.Equal(t, tt.wantOn, gotOn)
			switch tt.wantCode {
			case http.StatusOK:
				assert.Contains(t, rec.Body.String(), `id="plug-plug-1"`, "htmx gets the card back")
			case http.StatusSeeOther:
				assert.Equal(t, "/", rec.Header().Get("Location"))
			}
		})
	}
}

func TestHandleIndexHidesWebDisabledPlugs(t *testing.T) {
	ws, provider, _, _ := newTestWebServer(t)
	addPlug(provider, plugs.Plug{ID: "hidden", Name: "Hidden Plug", Web: new(false)}, plugs.State{})
	addPlug(provider, plugs.Plug{ID: "shown", Name: "Shown Plug", Web: new(true)}, plugs.State{})

	rec := httptest.NewRecorder()
	ws.HandleIndex(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rec.Body.String()
	assert.Contains(t, body, "Test Plug", "unset web flag means shown")
	assert.Contains(t, body, "Shown Plug")
	assert.NotContains(t, body, "Hidden Plug")
}

func TestPlugCardPowerStats(t *testing.T) {
	state := plugs.State{ID: "plug-1", Power: 12.34, Voltage: 230, Current: 0.054, Energy: 1.2345}

	metered := new(WebServer).renderPlugCard("plug-1", plugs.Plug{
		ID:       "plug-1",
		Features: &plugs.PlugFeatures{PowerMonitoring: true},
	}, state).Render()
	for _, want := range []string{"12.3 W", "230.0 V", "0.05 A", "1.234 kWh"} {
		assert.Contains(t, metered, want)
	}

	plain := new(WebServer).renderPlugCard("plug-1", plugs.Plug{ID: "plug-1"}, state).Render()
	assert.NotContains(t, plain, "electrical-stats")
}

// The card and plugs.State.Event each derive connection state from LastSeen
// and must agree on the cut-offs. synctest freezes the clock so the
// boundaries are exact.
func TestConnectionThresholds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tests := []struct {
			lastSeen time.Time
			want     string
			wantNote string
		}{
			{time.Time{}, "disconnected", "Never seen"},
			{time.Now().Add(-29 * time.Second), "connected", "Last seen: 29s ago"},
			{time.Now().Add(-30 * time.Second), "stale", "Last seen: 30s ago"},
			{time.Now().Add(-59 * time.Second), "stale", "Last seen: 59s ago"},
			{time.Now().Add(-60 * time.Second), "disconnected", "Last seen: 1m0s ago"},
		}
		for _, tt := range tests {
			state := plugs.State{ID: "plug-1", LastSeen: tt.lastSeen}

			card := new(WebServer).renderPlugCard("plug-1", plugs.Plug{ID: "plug-1"}, state).Render()
			assert.Contains(t, card, `connection-indicator `+tt.want+`"`, tt.wantNote)
			assert.Contains(t, card, tt.wantNote)

			evt := state.Event("test")
			assert.Equal(t, tt.want, evt.ConnectionState, tt.wantNote)
			assert.Equal(t, tt.wantNote, evt.ConnectionNote)
		}
	})
}

// flushRecorder wraps httptest.ResponseRecorder so the SSE handler can call
// Flush. httptest.ResponseRecorder is not safe for concurrent use, so a mutex
// guards writes against the test goroutine reading the body.
type flushRecorder struct {
	*httptest.ResponseRecorder
	mu sync.Mutex
}

func (f *flushRecorder) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ResponseRecorder.Write(p)
}

func (f *flushRecorder) Flush() {
	f.mu.Lock()
	defer f.mu.Unlock()
}

func (f *flushRecorder) body() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Body.String()
}

func TestHandleSSE(t *testing.T) {
	ws, _, _, bus := newTestWebServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ws.Start(ctx)

	req := httptest.NewRequest(http.MethodGet, "/events", nil).WithContext(ctx)
	rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}

	done := make(chan struct{})
	go func() {
		ws.HandleSSE(rec, req)
		close(done)
	}()

	// The snapshot is sent once the client is registered; anything
	// published before that would not reach it.
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.Contains(c, rec.body(), "\"plug_id\":\"plug-1\"", "missing snapshot")
	}, time.Second, 20*time.Millisecond)

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
		assert.Contains(c, rec.body(), "\"on\":true", "missing SSE event")
	}, time.Second, 20*time.Millisecond)

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SSE handler did not exit")
	}

	var lastData string
	for line := range strings.SplitSeq(rec.body(), "\n") {
		if after, ok := strings.CutPrefix(line, "data:"); ok {
			lastData = strings.TrimSpace(after)
		}
	}
	if lastData == "" {
		t.Fatalf("expected SSE payload, got %q", rec.body())
	}

	var evt events.StateUpdateEvent
	if err := json.Unmarshal([]byte(lastData), &evt); err != nil {
		t.Fatalf("failed to unmarshal SSE payload: %v", err)
	}
	if evt.PlugID != "plug-1" || !evt.On {
		t.Fatalf("unexpected SSE event: %+v", evt)
	}
}

// The first frames must carry every plug, not only what fits the client's
// live-update buffer.
func TestHandleSSESnapshotHasEveryPlug(t *testing.T) {
	ws, provider, _, _ := newTestWebServer(t)
	for i := range 20 {
		addPlug(provider, plugs.Plug{ID: fmt.Sprintf("extra-%02d", i)}, plugs.State{})
	}

	ctx, cancel := context.WithCancel(t.Context())
	rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ws.HandleSSE(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/events", nil))
	}()

	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		body := rec.body()
		for id := range provider.items {
			assert.Contains(c, body, `"plug_id":"`+id+`"`)
		}
	}, time.Second, 10*time.Millisecond)

	cancel()
	<-done
}

func TestHandleEventBusDebugShowsStatuses(t *testing.T) {
	ws, _, _, bus := newTestWebServer(t)

	ctx := t.Context()
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

	// Delivery is asynchronous; poll the page until the status lands.
	assert.EventuallyWithT(t, func(c *assert.CollectT) {
		rec := httptest.NewRecorder()
		ws.HandleEventBusDebug(rec, httptest.NewRequest(http.MethodGet, "/debug/eventbus", nil))
		assert.Contains(c, rec.Body.String(), "<td>hap</td><td>connected</td>")
	}, time.Second, 10*time.Millisecond)
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
	ws, _, _, _ := newTestWebServer(t)

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

// TestEventLogConcurrentAccess exercises the event log from two handlers at
// once. HandleToggle appends via LogEvent while HandleIndex reads the slice, so
// an unsynchronised eventLog trips the race detector here.
func TestEventLogConcurrentAccess(t *testing.T) {
	ws, _, _, _ := newTestWebServer(t)

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(2)

		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/toggle/plug-1", strings.NewReader("action=on"))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			ws.HandleToggle(httptest.NewRecorder(), req)
		}()

		go func() {
			defer wg.Done()
			ws.HandleIndex(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		}()
	}
	wg.Wait()
}

// TestCloseWhileSSEConnected shuts the server down with a live SSE client.
// Close must wake the handler and must not close the per-client channel the
// handler already owns, or the handler's own cleanup panics on a double close.
func TestCloseWhileSSEConnected(t *testing.T) {
	ws, _, _, _ := newTestWebServer(t)

	rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequest(http.MethodGet, "/events", nil).WithContext(t.Context())

	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		ws.HandleSSE(rec, req)
		done <- nil
	}()

	// Wait for the handler to register itself as an SSE client.
	for range 200 {
		ws.sseClientsMu.RLock()
		n := len(ws.sseClients)
		ws.sseClientsMu.RUnlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	ws.Close()

	select {
	case r := <-done:
		if r != nil {
			t.Fatalf("HandleSSE panicked on shutdown: %v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HandleSSE did not return after Close")
	}
}
