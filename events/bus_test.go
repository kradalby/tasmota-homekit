package events

import (
	"io"
	"log/slog"
	"testing"
	"testing/synctest"
)

// Close must stop the underlying bus too, or its router goroutine outlives it.
// synctest fails the test if any goroutine started inside the bubble remains.
func TestCloseStopsBus(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bus, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		if err := bus.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

// Network handlers can outlive their server's Close; a publish that comes
// after the bus closes must be dropped rather than panic.
func TestPublishAfterClose(t *testing.T) {
	bus, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	client, err := bus.Client(ClientWeb)
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Close(); err != nil {
		t.Fatal(err)
	}

	bus.PublishStateUpdate(client, StateUpdateEvent{})
	bus.PublishCommand(client, CommandEvent{})
	bus.PublishConnectionStatus(client, ConnectionStatusEvent{})
}
