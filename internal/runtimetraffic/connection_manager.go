package runtimetraffic

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"submux/internal/runtimeapi"
)

var (
	ErrConnectionSnapshotUnavailable = errors.New("Runtime connection snapshot is unavailable")
	ErrConnectionScopeChanged        = errors.New("Runtime connection scope changed after confirmation")
)

type ConnectionCloseResult struct {
	Matched       int
	Closed        int
	AlreadyClosed int
}

type ConnectionManager struct {
	Collector  *Collector
	Controller MihomoController
}

func (manager *ConnectionManager) CloseOne(ctx context.Context, connectionID string) (ConnectionCloseResult, error) {
	if manager == nil {
		return ConnectionCloseResult{}, errors.New("Runtime connection manager is unavailable")
	}
	alreadyClosed, err := manager.Controller.CloseConnection(ctx, connectionID)
	if err != nil {
		return ConnectionCloseResult{}, err
	}
	result := ConnectionCloseResult{Matched: 1}
	if alreadyClosed {
		result.AlreadyClosed = 1
	} else {
		result.Closed = 1
	}
	return result, nil
}

func (manager *ConnectionManager) CloseScope(
	ctx context.Context,
	query runtimeapi.ConnectionQuery,
	confirmedCount int,
	confirmedScopeToken string,
) (ConnectionCloseResult, error) {
	if ctx == nil {
		return ConnectionCloseResult{}, errors.New("Runtime connection close context is required")
	}
	if manager == nil || manager.Collector == nil {
		return ConnectionCloseResult{}, ErrConnectionSnapshotUnavailable
	}
	connections, scopeToken, available := manager.matchingConnections(query)
	if !available {
		return ConnectionCloseResult{}, ErrConnectionSnapshotUnavailable
	}
	if len(connections) > confirmedCount || scopeToken != confirmedScopeToken {
		return ConnectionCloseResult{}, ErrConnectionScopeChanged
	}
	result := ConnectionCloseResult{Matched: len(connections)}
	if len(connections) == 0 {
		return result, nil
	}
	for _, connection := range connections {
		if connection.ID == "" {
			return ConnectionCloseResult{}, fmt.Errorf("%w: connection has no stable ID", ErrConnectionSnapshotUnavailable)
		}
		alreadyClosed, err := manager.Controller.CloseConnection(ctx, connection.ID)
		if err != nil {
			return result, err
		}
		if alreadyClosed {
			result.AlreadyClosed++
		} else {
			result.Closed++
		}
	}
	return result, nil
}

func (manager *ConnectionManager) matchingConnections(query runtimeapi.ConnectionQuery) ([]runtimeapi.Connection, string, bool) {
	manager.Collector.mu.RLock()
	defer manager.Collector.mu.RUnlock()
	if !manager.Collector.status.Available {
		return nil, "", false
	}
	connections := make([]runtimeapi.Connection, 0, len(manager.Collector.connections))
	for _, connection := range manager.Collector.connections {
		if !connectionMatches(connection, query) {
			continue
		}
		clone := connection
		clone.OutboundChain = append([]string(nil), connection.OutboundChain...)
		connections = append(connections, clone)
	}
	return connections, connectionScopeToken(connections), true
}

func connectionScopeToken(connections []runtimeapi.Connection) string {
	ids := make([]string, len(connections))
	for index, connection := range connections {
		ids[index] = connection.ID
	}
	sort.Strings(ids)
	digest := sha256.New()
	var length [8]byte
	for _, id := range ids {
		binary.BigEndian.PutUint64(length[:], uint64(len(id)))
		_, _ = digest.Write(length[:])
		_, _ = digest.Write([]byte(id))
	}
	return hex.EncodeToString(digest.Sum(nil))
}
