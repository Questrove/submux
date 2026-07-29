package runtimeipc

import (
	"context"
	"errors"
	"testing"
	"time"

	"submux/internal/runtimeapi"
	"submux/internal/runtimestate"
)

func TestClientWatchesPersistedEventsOverLocalIPC(t *testing.T) {
	service := eventObserver{
		observerFunc: func(context.Context, runtimeapi.PeerIdentity) (runtimeapi.Snapshot, error) {
			return runtimeapi.Snapshot{
				ProtocolVersion:   runtimeapi.ProtocolVersion,
				Revision:          2,
				LatestEventCursor: 1,
			}, nil
		},
		events: func(ctx context.Context, _ runtimeapi.PeerIdentity, after uint64, _ int) ([]runtimeapi.Event, uint64, error) {
			if after == 0 {
				return []runtimeapi.Event{{
					Cursor:           1,
					Type:             "operation.queued",
					At:               time.Unix(1, 0).UTC(),
					OperationID:      "op_shared",
					SnapshotRevision: 2,
				}}, 1, nil
			}
			<-ctx.Done()
			return nil, 1, ctx.Err()
		},
	}
	client, stop := startEventTestRuntime(t, service)
	defer stop()

	stopWatching := errors.New("event observed")
	err := client.WatchEvents(context.Background(), 0, func(event runtimeapi.Event) error {
		if event.Cursor != 1 || event.OperationID != "op_shared" {
			t.Fatalf("Runtime event=%#v", event)
		}
		return stopWatching
	})
	if !errors.Is(err, stopWatching) {
		t.Fatalf("watch Runtime events error=%v", err)
	}
}

func TestClientReadsAuthoritativeSnapshotWhenEventCursorExpires(t *testing.T) {
	service := eventObserver{
		observerFunc: func(context.Context, runtimeapi.PeerIdentity) (runtimeapi.Snapshot, error) {
			return runtimeapi.Snapshot{
				ProtocolVersion:   runtimeapi.ProtocolVersion,
				Revision:          51,
				LatestEventCursor: 50,
			}, nil
		},
		events: func(ctx context.Context, _ runtimeapi.PeerIdentity, after uint64, _ int) ([]runtimeapi.Event, uint64, error) {
			if after == 0 {
				return nil, 40, &runtimestate.CursorExpiredError{Earliest: 40}
			}
			<-ctx.Done()
			return nil, 40, ctx.Err()
		},
	}
	client, stop := startEventTestRuntime(t, service)
	defer stop()

	ctx, cancel := context.WithCancel(context.Background())
	var update RuntimeUpdate
	err := client.Watch(ctx, 0, func(value RuntimeUpdate) error {
		update = value
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("watch Runtime after expired cursor error=%v", err)
	}
	if update.Snapshot == nil || update.Event != nil ||
		update.Snapshot.Revision != 51 ||
		update.Snapshot.LatestEventCursor != 50 {
		t.Fatalf("resynchronized Runtime update=%#v", update)
	}
}

func TestClientResynchronizesWhenCursorExpiresAfterStreamStarts(t *testing.T) {
	service := eventObserver{
		observerFunc: func(context.Context, runtimeapi.PeerIdentity) (runtimeapi.Snapshot, error) {
			return runtimeapi.Snapshot{
				ProtocolVersion:   runtimeapi.ProtocolVersion,
				Revision:          22,
				LatestEventCursor: 21,
			}, nil
		},
		events: func(_ context.Context, _ runtimeapi.PeerIdentity, after uint64, _ int) ([]runtimeapi.Event, uint64, error) {
			if after == 0 {
				return []runtimeapi.Event{{
					Cursor:           1,
					Type:             "operation.queued",
					At:               time.Unix(1, 0).UTC(),
					OperationID:      "op_before_gap",
					SnapshotRevision: 2,
				}}, 1, nil
			}
			return nil, 20, &runtimestate.CursorExpiredError{Earliest: 20}
		},
	}
	client, stop := startEventTestRuntime(t, service)
	defer stop()

	ctx, cancel := context.WithCancel(context.Background())
	var updates []RuntimeUpdate
	err := client.Watch(ctx, 0, func(value RuntimeUpdate) error {
		updates = append(updates, value)
		if value.Snapshot != nil {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("watch Runtime across retained-history gap error=%v", err)
	}
	if len(updates) != 2 ||
		updates[0].Event == nil ||
		updates[0].Event.Cursor != 1 ||
		updates[1].Snapshot == nil ||
		updates[1].Snapshot.LatestEventCursor != 21 {
		t.Fatalf("Runtime updates across retained-history gap=%#v", updates)
	}
}

func startEventTestRuntime(t *testing.T, observer Observer) (*Client, func()) {
	t.Helper()
	endpoint := testEndpoint(t)
	listener, err := Listen(endpoint)
	if err != nil {
		t.Fatalf("listen on Runtime IPC: %v", err)
	}
	authorizer, err := CurrentUserAuthorizer()
	if err != nil {
		_ = listener.Close()
		t.Fatalf("create Runtime authorizer: %v", err)
	}
	server, err := NewServer(observer, authorizer)
	if err != nil {
		_ = listener.Close()
		t.Fatalf("create Runtime IPC server: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- server.Serve(ctx, listener) }()
	client, err := NewClient(endpoint, "test")
	if err != nil {
		cancel()
		_ = <-result
		t.Fatalf("create Runtime IPC client: %v", err)
	}
	stop := func() {
		client.CloseIdleConnections()
		cancel()
		if err := <-result; err != nil {
			t.Errorf("stop Runtime IPC server: %v", err)
		}
	}
	return client, stop
}
