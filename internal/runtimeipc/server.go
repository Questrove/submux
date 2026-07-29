package runtimeipc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"submux/internal/runtimeapi"
)

type Observer interface {
	Observe(context.Context, runtimeapi.PeerIdentity) (runtimeapi.Snapshot, error)
}

type Server struct {
	observer   Observer
	authorizer Authorizer
}

type peerContextValue struct {
	identity runtimeapi.PeerIdentity
	err      error
}

type peerContextKey struct{}

func NewServer(observer Observer, authorizer Authorizer) (*Server, error) {
	if observer == nil {
		return nil, errors.New("Runtime observer is required")
	}
	if authorizer == nil {
		return nil, errors.New("Runtime authorizer is required")
	}
	return &Server{observer: observer, authorizer: authorizer}, nil
}

func (s *Server) Serve(ctx context.Context, listener LocalListener) error {
	if ctx == nil {
		return errors.New("Runtime server context is required")
	}
	if listener == nil {
		return errors.New("Runtime local listener is required")
	}
	httpServer := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ConnContext: func(connectionContext context.Context, connection net.Conn) context.Context {
			identity, err := listener.PeerIdentity(connection)
			return context.WithValue(connectionContext, peerContextKey{}, peerContextValue{
				identity: identity,
				err:      err,
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
		shutdownErr := httpServer.Shutdown(shutdownContext)
		serveErr := <-result
		return errors.Join(shutdownErr, serveErr)
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/snapshot", s.handleSnapshot)
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		s.writeError(writer, request, http.StatusNotFound, runtimeapi.ErrorInvalidRequest, "unknown Runtime IPC endpoint", false)
	})
	return mux
}

func (s *Server) handleSnapshot(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json")

	requestID, ok := s.validateRequest(writer, request)
	if !ok {
		return
	}
	peerValue, ok := request.Context().Value(peerContextKey{}).(peerContextValue)
	if !ok || peerValue.err != nil {
		s.writeError(writer, request, http.StatusForbidden, runtimeapi.ErrorUnauthorized, "Runtime could not verify the local peer identity", false)
		return
	}
	if err := s.authorizer.Authorize(peerValue.identity); err != nil {
		s.writeError(writer, request, http.StatusForbidden, runtimeapi.ErrorUnauthorized, "Runtime peer is not authorized", false)
		return
	}
	snapshot, err := s.observer.Observe(request.Context(), peerValue.identity)
	if err != nil {
		s.writeError(writer, request, http.StatusServiceUnavailable, runtimeapi.ErrorServiceUnavailable, "Runtime state is temporarily unavailable", true)
		return
	}
	writer.Header().Set(HeaderRequestID, requestID)
	writer.Header().Set(HeaderProtocolVersion, strconv.Itoa(runtimeapi.ProtocolVersion))
	writer.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(writer).Encode(snapshot)
}

func (s *Server) validateRequest(writer http.ResponseWriter, request *http.Request) (string, bool) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		s.writeError(writer, request, http.StatusMethodNotAllowed, runtimeapi.ErrorInvalidRequest, "Runtime snapshot only accepts GET", false)
		return "", false
	}
	if request.URL.RawQuery != "" {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime snapshot does not accept query parameters", false)
		return "", false
	}
	requestID := request.Header.Get(HeaderRequestID)
	if !validIdentifier(requestID, 128) {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime request ID is missing or invalid", false)
		return "", false
	}
	versionText := request.Header.Get(HeaderProtocolVersion)
	version, err := strconv.Atoi(versionText)
	if err != nil || version != runtimeapi.ProtocolVersion {
		s.writeError(writer, request, http.StatusUpgradeRequired, runtimeapi.ErrorProtocolUnsupported, "Runtime protocol version is not supported", false)
		return "", false
	}
	clientVersion := request.Header.Get(HeaderClientVersion)
	if clientVersion == "" || len(clientVersion) > 128 || hasControlCharacter(clientVersion) {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime client version is missing or invalid", false)
		return "", false
	}
	if request.ContentLength > 0 || len(request.TransferEncoding) > 0 {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime snapshot does not accept a request body", false)
		return "", false
	}
	if request.Body != nil {
		body, err := io.ReadAll(io.LimitReader(request.Body, 1))
		if err != nil || len(body) != 0 {
			s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime snapshot does not accept a request body", false)
			return "", false
		}
	}
	return requestID, true
}

func (s *Server) writeError(
	writer http.ResponseWriter,
	request *http.Request,
	status int,
	code string,
	message string,
	retryable bool,
) {
	requestID := request.Header.Get(HeaderRequestID)
	if !validIdentifier(requestID, 128) {
		requestID = ""
	}
	response := runtimeapi.ErrorEnvelope{
		ProtocolVersion: runtimeapi.ProtocolVersion,
		RequestID:       requestID,
		Error: runtimeapi.ProtocolError{
			Code:      code,
			Message:   message,
			Retryable: retryable,
		},
	}
	if code == runtimeapi.ErrorProtocolUnsupported {
		response.SupportedVersions = []int{runtimeapi.ProtocolVersion}
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set(HeaderProtocolVersion, strconv.Itoa(runtimeapi.ProtocolVersion))
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(response)
}

func validIdentifier(value string, maximum int) bool {
	if value == "" || len(value) > maximum {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			strings.ContainsRune("-_.:", character) {
			continue
		}
		return false
	}
	return true
}

func hasControlCharacter(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}

func withPeerContext(request *http.Request, identity runtimeapi.PeerIdentity, err error) *http.Request {
	return request.WithContext(context.WithValue(request.Context(), peerContextKey{}, peerContextValue{
		identity: identity,
		err:      err,
	}))
}
