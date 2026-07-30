package runtimenet

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"submux/internal/runtimeapi"
)

const maxIPCRequestBytes = 64 << 10

type LocalListener interface {
	net.Listener
	PeerUID(net.Conn) (uint32, error)
}

type Server struct {
	manager *Manager

	mu          sync.Mutex
	connections map[string]sessionConnection
}

type sessionConnection struct {
	id        string
	expiresAt time.Time
}

type connectionIdentity struct {
	id  string
	uid uint32
	err error
}

type connectionContextKey struct{}

type sessionEnvelope struct {
	Request SessionRequest `json:"request"`
}

type previewEnvelope struct {
	SessionID string                           `json:"session_id"`
	Request   runtimeapi.NetworkPreviewRequest `json:"request"`
}

type prepareEnvelope struct {
	Meta   RequestMeta `json:"meta"`
	PlanID string      `json:"plan_id"`
}

type ownershipEnvelope struct {
	Meta        RequestMeta `json:"meta"`
	OwnershipID string      `json:"ownership_id"`
}

type releaseEnvelope struct {
	Meta        RequestMeta `json:"meta"`
	OwnershipID string      `json:"ownership_id"`
	Reason      string      `json:"reason"`
}

type observeEnvelope struct {
	SessionID string `json:"session_id"`
}

type resultEnvelope struct {
	SessionID   string `json:"session_id"`
	OperationID string `json:"operation_id"`
	Operation   string `json:"operation"`
}

type coreStageEnvelope struct {
	Meta  RequestMeta        `json:"meta"`
	Stage PrivilegedCoreStage `json:"stage"`
}

type coreMutationEnvelope struct {
	Meta     RequestMeta `json:"meta"`
	ObjectID string      `json:"object_id"`
}

type coreObserveEnvelope struct {
	SessionID string `json:"session_id"`
	ObjectID  string `json:"object_id"`
}

type errorEnvelope struct {
	Error string `json:"error"`
}

func NewServer(manager *Manager) (*Server, error) {
	if manager == nil {
		return nil, errors.New("privileged Runtime network manager is required")
	}
	return &Server{
		manager:     manager,
		connections: make(map[string]sessionConnection),
	}, nil
}

func (s *Server) Serve(ctx context.Context, listener LocalListener) error {
	if ctx == nil {
		return errors.New("privileged Runtime network server context is required")
	}
	if listener == nil {
		return errors.New("privileged Runtime network listener is required")
	}
	httpServer := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    8 << 10,
		ConnContext: func(parent context.Context, connection net.Conn) context.Context {
			id, idErr := randomIdentifier("conn_")
			uid, uidErr := listener.PeerUID(connection)
			return context.WithValue(parent, connectionContextKey{}, connectionIdentity{
				id:  id,
				uid: uid,
				err: errors.Join(idErr, uidErr),
			})
		},
	}
	result := make(chan error, 1)
	go func() {
		err := httpServer.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
			err = nil
		}
		result <- err
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return errors.Join(httpServer.Shutdown(shutdownContext), <-result)
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/session", s.handleSession)
	mux.HandleFunc("/v1/preview", s.handlePreview)
	mux.HandleFunc("/v1/prepare", s.handlePrepare)
	mux.HandleFunc("/v1/commit", s.handleCommit)
	mux.HandleFunc("/v1/renew", s.handleRenew)
	mux.HandleFunc("/v1/release", s.handleRelease)
	mux.HandleFunc("/v1/observe", s.handleObserve)
	mux.HandleFunc("/v1/core/stage", s.handleCoreStage)
	mux.HandleFunc("/v1/core/start", s.handleCoreStart)
	mux.HandleFunc("/v1/core/stop", s.handleCoreStop)
	mux.HandleFunc("/v1/core/observe", s.handleCoreObserve)
	mux.HandleFunc("/v1/results/query", s.handleResult)
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		writeIPCError(writer, http.StatusNotFound, errors.New("unknown privileged Runtime network endpoint"))
	})
	return mux
}

func (s *Server) handleSession(writer http.ResponseWriter, request *http.Request) {
	if !validatePOST(writer, request) {
		return
	}
	connection, ok := request.Context().Value(connectionContextKey{}).(connectionIdentity)
	if !ok || connection.err != nil || connection.id == "" {
		writeIPCError(writer, http.StatusForbidden, errors.New("privileged Runtime network peer identity is unavailable"))
		return
	}
	var envelope sessionEnvelope
	if err := decodeIPCRequest(request, &envelope); err != nil {
		writeIPCError(writer, http.StatusBadRequest, err)
		return
	}
	session, err := s.manager.OpenSession(connection.uid, envelope.Request)
	if err != nil {
		writeIPCError(writer, http.StatusForbidden, err)
		return
	}
	s.mu.Lock()
	for id, binding := range s.connections {
		if !binding.expiresAt.After(time.Now()) {
			delete(s.connections, id)
		}
	}
	s.connections[session.ID] = sessionConnection{id: connection.id, expiresAt: session.ExpiresAt}
	s.mu.Unlock()
	writeIPCJSON(writer, http.StatusCreated, session)
}

func (s *Server) handlePreview(writer http.ResponseWriter, request *http.Request) {
	var envelope previewEnvelope
	if !s.decodeSessionRequest(writer, request, &envelope, func() string { return envelope.SessionID }) {
		return
	}
	preview, err := s.manager.Preview(request.Context(), envelope.SessionID, envelope.Request)
	writeIPCResult(writer, preview, err)
}

func (s *Server) handlePrepare(writer http.ResponseWriter, request *http.Request) {
	var envelope prepareEnvelope
	if !s.decodeSessionRequest(writer, request, &envelope, func() string { return envelope.Meta.SessionID }) {
		return
	}
	prepared, err := s.manager.Prepare(request.Context(), envelope.Meta, envelope.PlanID)
	writeIPCResult(writer, prepared, err)
}

func (s *Server) handleCommit(writer http.ResponseWriter, request *http.Request) {
	var envelope ownershipEnvelope
	if !s.decodeSessionRequest(writer, request, &envelope, func() string { return envelope.Meta.SessionID }) {
		return
	}
	status, err := s.manager.Commit(request.Context(), envelope.Meta, envelope.OwnershipID)
	writeIPCResult(writer, status, err)
}

func (s *Server) handleRenew(writer http.ResponseWriter, request *http.Request) {
	var envelope ownershipEnvelope
	if !s.decodeSessionRequest(writer, request, &envelope, func() string { return envelope.Meta.SessionID }) {
		return
	}
	status, err := s.manager.Renew(request.Context(), envelope.Meta, envelope.OwnershipID)
	writeIPCResult(writer, status, err)
}

func (s *Server) handleRelease(writer http.ResponseWriter, request *http.Request) {
	var envelope releaseEnvelope
	if !s.decodeSessionRequest(writer, request, &envelope, func() string { return envelope.Meta.SessionID }) {
		return
	}
	status, err := s.manager.Release(
		request.Context(),
		envelope.Meta,
		envelope.OwnershipID,
		envelope.Reason,
	)
	writeIPCResult(writer, status, err)
}

func (s *Server) handleObserve(writer http.ResponseWriter, request *http.Request) {
	var envelope observeEnvelope
	if !s.decodeSessionRequest(writer, request, &envelope, func() string { return envelope.SessionID }) {
		return
	}
	status, err := s.manager.Observe(request.Context(), envelope.SessionID)
	writeIPCResult(writer, status, err)
}

func (s *Server) handleResult(writer http.ResponseWriter, request *http.Request) {
	var envelope resultEnvelope
	if !s.decodeSessionRequest(writer, request, &envelope, func() string { return envelope.SessionID }) {
		return
	}
	result, err := s.manager.Result(envelope.SessionID, envelope.OperationID, envelope.Operation)
	writeIPCResult(writer, result, err)
}

func (s *Server) handleCoreStage(writer http.ResponseWriter, request *http.Request) {
	var envelope coreStageEnvelope
	if !s.decodeSessionRequest(writer, request, &envelope, func() string { return envelope.Meta.SessionID }) {
		return
	}
	status, err := s.manager.StageCore(request.Context(), envelope.Meta, envelope.Stage)
	writeIPCResult(writer, status, err)
}

func (s *Server) handleCoreStart(writer http.ResponseWriter, request *http.Request) {
	var envelope coreMutationEnvelope
	if !s.decodeSessionRequest(writer, request, &envelope, func() string { return envelope.Meta.SessionID }) {
		return
	}
	status, err := s.manager.StartCore(request.Context(), envelope.Meta, envelope.ObjectID)
	writeIPCResult(writer, status, err)
}

func (s *Server) handleCoreStop(writer http.ResponseWriter, request *http.Request) {
	var envelope coreMutationEnvelope
	if !s.decodeSessionRequest(writer, request, &envelope, func() string { return envelope.Meta.SessionID }) {
		return
	}
	status, err := s.manager.StopCore(request.Context(), envelope.Meta, envelope.ObjectID)
	writeIPCResult(writer, status, err)
}

func (s *Server) handleCoreObserve(writer http.ResponseWriter, request *http.Request) {
	var envelope coreObserveEnvelope
	if !s.decodeSessionRequest(writer, request, &envelope, func() string { return envelope.SessionID }) {
		return
	}
	status, err := s.manager.ObserveCore(request.Context(), envelope.SessionID, envelope.ObjectID)
	writeIPCResult(writer, status, err)
}

func (s *Server) decodeSessionRequest(
	writer http.ResponseWriter,
	request *http.Request,
	target any,
	sessionID func() string,
) bool {
	if !validatePOST(writer, request) {
		return false
	}
	if err := decodeIPCRequest(request, target); err != nil {
		writeIPCError(writer, http.StatusBadRequest, err)
		return false
	}
	connection, ok := request.Context().Value(connectionContextKey{}).(connectionIdentity)
	if !ok || connection.err != nil || !s.sessionUsesConnection(sessionID(), connection.id) {
		writeIPCError(writer, http.StatusForbidden, errors.New("privileged Runtime network session is not bound to this connection"))
		return false
	}
	return true
}

func (s *Server) sessionUsesConnection(sessionID, connectionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, exists := s.connections[sessionID]
	if exists && !binding.expiresAt.After(time.Now()) {
		delete(s.connections, sessionID)
		return false
	}
	return exists && sessionID != "" && connectionID != "" && binding.id == connectionID
}

func validatePOST(writer http.ResponseWriter, request *http.Request) bool {
	writer.Header().Set("Cache-Control", "no-store")
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writeIPCError(writer, http.StatusMethodNotAllowed, errors.New("privileged Runtime network endpoint only accepts POST"))
		return false
	}
	if request.URL.RawQuery != "" {
		writeIPCError(writer, http.StatusBadRequest, errors.New("privileged Runtime network endpoint does not accept query parameters"))
		return false
	}
	return true
}

func decodeIPCRequest(request *http.Request, target any) error {
	reader := io.LimitReader(request.Body, maxIPCRequestBytes+1)
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("privileged Runtime network request is invalid")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("privileged Runtime network request contains trailing data")
	}
	return nil
}

func writeIPCResult(writer http.ResponseWriter, result any, err error) {
	if err != nil {
		writeIPCError(writer, http.StatusConflict, err)
		return
	}
	writeIPCJSON(writer, http.StatusOK, result)
}

func writeIPCError(writer http.ResponseWriter, status int, err error) {
	writeIPCJSON(writer, status, errorEnvelope{Error: err.Error()})
}

func writeIPCJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
