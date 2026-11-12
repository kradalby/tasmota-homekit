package tasmotahomekit

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brutella/hap"
	"github.com/brutella/hap/chacha20poly1305"
	"github.com/brutella/hap/curve25519"
	"github.com/brutella/hap/hkdf"
	"github.com/brutella/hap/tlv8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kradalby/tasmota-homekit/plugs"
)

// newSceneBridge wires the path a HomeKit write takes in production: a paired
// hap.Server over HAPManager, plugs.Manager and tasmota-go clients, with each
// plug a fake Tasmota HTTP server running handlers[i].
func newSceneBridge(t *testing.T, handlers []http.HandlerFunc) (*HAPManager, http.Handler) {
	t.Helper()

	// Created before the plugs so it closes after them: cleanups run last
	// first, and a plug reply landing on a closed bus panics.
	bus := newTestEventsBus(t)

	cfg := make([]plugs.Plug, len(handlers))
	for i, h := range handlers {
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		cfg[i] = plugs.Plug{ID: fmt.Sprintf("plug-%d", i), Name: fmt.Sprintf("Plug %d", i), Address: srv.URL}
	}

	pm, err := plugs.NewManager(cfg, bus)
	require.NoError(t, err)
	hm := NewHAPManager(cfg, "Test Bridge", pm, bus)
	t.Cleanup(hm.Close)

	store := hap.NewMemStore()
	accs := hm.GetAccessories()
	srv, err := hap.NewServer(store, accs[0], accs[1:]...)
	require.NoError(t, err)

	return hm, pairVerify(t, srv, store)
}

// pairVerify runs pair-verify through srv's handler as a controller already
// paired in store, so later requests from httptest's fixed RemoteAddr pass
// hap's session check. hap registers no conn for that address, so it skips
// the encryption upgrade and requests stay plain JSON.
func pairVerify(t *testing.T, srv *hap.Server, store hap.Store) http.Handler {
	t.Helper()
	h := srv.ServeMux().(http.Handler)

	const controller = "test-controller"
	ctrlPub, ctrlKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pairing, err := json.Marshal(hap.Pairing{Name: controller, PublicKey: ctrlPub, Permission: 1})
	require.NoError(t, err)
	require.NoError(t, store.Set(hex.EncodeToString([]byte(controller))+".pairing", pairing))

	accPub, accKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	srv.Key = hap.KeyPair{Public: accPub, Private: accKey}

	// tlv8.Marshal misreads ",optional" tags, so requests use plain ones.
	type reply struct {
		PublicKey []byte `tlv8:"3,optional"`
		State     byte   `tlv8:"6,optional"`
		Error     byte   `tlv8:"7,optional"`
	}
	post := func(in any) reply {
		b, err := tlv8.Marshal(in)
		require.NoError(t, err)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/pair-verify", bytes.NewReader(b)))
		var out reply
		require.NoError(t, tlv8.Unmarshal(w.Body.Bytes(), &out))
		require.Zero(t, out.Error, "pair-verify M%d", out.State)
		return out
	}

	pub, priv := curve25519.GenerateKeyPair()
	m2 := post(struct {
		PublicKey []byte `tlv8:"3"`
		State     byte   `tlv8:"6"`
	}{pub[:], 1})

	var accEphemeral [32]byte
	copy(accEphemeral[:], m2.PublicKey)
	shared := curve25519.SharedSecret(priv, accEphemeral)
	key, err := hkdf.Sha512(shared[:], []byte("Pair-Verify-Encrypt-Salt"), []byte("Pair-Verify-Encrypt-Info"))
	require.NoError(t, err)

	signed := append(append(pub[:], controller...), accEphemeral[:]...)
	inner, err := tlv8.Marshal(struct {
		Identifier string `tlv8:"1"`
		Signature  []byte `tlv8:"10"`
	}{controller, ed25519.Sign(ctrlKey, signed)})
	require.NoError(t, err)
	enc, mac, err := chacha20poly1305.EncryptAndSeal(key[:], []byte("PV-Msg03"), inner, nil)
	require.NoError(t, err)

	m4 := post(struct {
		EncryptedData []byte `tlv8:"5"`
		State         byte   `tlv8:"6"`
	}{append(enc, mac[:]...), 3})
	require.Equal(t, byte(4), m4.State)

	return h
}

// applyScene sends what HomeKit sends a bridge for a scene: one PUT
// /characteristics turning every outlet on.
func applyScene(h http.Handler, hm *HAPManager) *httptest.ResponseRecorder {
	type write struct {
		Aid   uint64 `json:"aid"`
		Iid   uint64 `json:"iid"`
		Value bool   `json:"value"`
	}
	var cs []write
	for _, id := range hm.accessoryOrder {
		o := hm.accessories[id].(*OutletWrapper)
		cs = append(cs, write{Aid: o.Id, Iid: o.Outlet.Outlet.On.Id, Value: true})
	}
	body, _ := json.Marshal(map[string]any{"characteristics": cs})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/characteristics", bytes.NewReader(body)))
	return w
}

// sceneRun is what a set of fake plugs saw while a scene was applied.
type sceneRun struct {
	mu          sync.Mutex
	powerAt     []time.Time // when plug i received its Power command
	statusAt    []time.Time
	inFlight    int // Power commands received and not yet answered
	maxInFlight int
}

// plug serves Tasmota's /cm endpoint as plug i, answering Power after power
// and Status 0 after status.
func (r *sceneRun) plug(i int, power, status time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		cmd := req.URL.Query().Get("cmnd")
		switch {
		case strings.HasPrefix(cmd, "Power"):
			r.mu.Lock()
			r.powerAt[i] = time.Now()
			r.inFlight++
			r.maxInFlight = max(r.maxInFlight, r.inFlight)
			r.mu.Unlock()

			time.Sleep(power)

			r.mu.Lock()
			r.inFlight--
			r.mu.Unlock()
			_, _ = io.WriteString(w, `{"POWER":"ON"}`)
		case cmd == "Status 0":
			r.mu.Lock()
			r.statusAt[i] = time.Now()
			r.mu.Unlock()

			time.Sleep(status)
			_, _ = io.WriteString(w, `{"StatusSTS":{"POWER":"ON"}}`)
		default:
			http.Error(w, "unexpected command "+cmd, http.StatusBadRequest)
		}
	}
}

// A scene's plugs must switch together. hap's PUT /characteristics calls each
// write's callback in turn, so a callback that waited on its plug would tell
// plug i to switch only once every plug before it had answered.
//
// Each plug holds its Power reply for powerDelay; Power commands sent
// together are therefore all in flight at once, while one-at-a-time sending
// never has more than one. The logs show when each plug got its command, and
// statusDelay shows that a slow Status 0 no longer holds anything back.
func TestSceneSwitchesPlugsTogether(t *testing.T) {
	const (
		plugCount  = 4
		powerDelay = 100 * time.Millisecond
	)

	for _, tc := range []struct {
		name        string
		statusDelay time.Duration
	}{
		{"status-instant", 0},
		{"status-as-slow-as-power", powerDelay},
		{"status-4x-power", 4 * powerDelay},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			run := &sceneRun{
				powerAt:  make([]time.Time, plugCount),
				statusAt: make([]time.Time, plugCount),
			}
			handlers := make([]http.HandlerFunc, plugCount)
			for i := range handlers {
				handlers[i] = run.plug(i, powerDelay, tc.statusDelay)
			}
			hm, h := newSceneBridge(t, handlers)

			start := time.Now()
			w := applyScene(h, hm)
			took := time.Since(start)
			require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
			hm.wg.Wait()

			run.mu.Lock()
			defer run.mu.Unlock()
			var rows []string
			for i := range plugCount {
				row := fmt.Sprintf("plug %d: Power +%v", i, run.powerAt[i].Sub(start).Round(time.Millisecond))
				if !run.statusAt[i].IsZero() {
					row += fmt.Sprintf(", Status 0 +%v", run.statusAt[i].Sub(start).Round(time.Millisecond))
				}
				rows = append(rows, row)
			}
			t.Logf("power=%v status=%v, HomeKit reply after %v\n%s",
				powerDelay, tc.statusDelay, took.Round(time.Millisecond), strings.Join(rows, "\n"))

			assert.Equal(t, plugCount, run.maxInFlight,
				"plugs received Power one after another (at most %d of %d in flight at once)", run.maxInFlight, plugCount)
		})
	}
}

// HomeKit's scene request succeeds without waiting on the plugs. A plug that
// fails is logged and returns to its real state once its command is done;
// the other plugs still switch.
func TestSceneFailedPlugReturnsToPlugState(t *testing.T) {
	errs := captureErrors(t)
	ok := func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"POWER":"ON"}`)
	}
	down := func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unreachable", http.StatusServiceUnavailable)
	}
	hm, h := newSceneBridge(t, []http.HandlerFunc{ok, down, ok})

	w := applyScene(h, hm)
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
	hm.wg.Wait()

	assert.True(t, hm.accessories["plug-0"].OnValue())
	assert.False(t, hm.accessories["plug-1"].OnValue())
	assert.True(t, hm.accessories["plug-2"].OnValue())
	assert.Equal(t, []string{"plug-1"}, errs.plugIDs())
}
