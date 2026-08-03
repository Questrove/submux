package runtimeipc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"submux/internal/runtimeapi"
	"submux/internal/runtimeprivacy"
	"submux/internal/runtimestate"
	"submux/internal/runtimeupdate"
)

type Observer interface {
	Observe(context.Context, runtimeapi.PeerIdentity) (runtimeapi.Snapshot, error)
}

type EventObserver interface {
	Events(context.Context, runtimeapi.PeerIdentity, uint64, int) ([]runtimeapi.Event, uint64, error)
}

type TrafficObserver interface {
	TrafficHistory(context.Context, runtimeapi.PeerIdentity, runtimeapi.TrafficHistoryRequest) (runtimeapi.TrafficHistory, error)
}

type ConnectionObserver interface {
	Connections(context.Context, runtimeapi.PeerIdentity, runtimeapi.ConnectionQuery) (runtimeapi.ConnectionPage, error)
}

type RuleObserver interface {
	Rules(context.Context, runtimeapi.PeerIdentity, runtimeapi.RuleQuery) (runtimeapi.RuleSet, error)
}

type Operator interface {
	UploadImport(context.Context, runtimeapi.PeerIdentity, string, int64, string, []byte) (runtimeapi.ImportContent, error)
	GetAdvancedOverride(context.Context, runtimeapi.PeerIdentity, string, string, string, bool) (runtimeapi.AdvancedOverrideDocument, error)
	PreviewCandidate(context.Context, runtimeapi.PeerIdentity, runtimeapi.PreviewCandidateRequest) (runtimeapi.CandidatePreview, error)
	Execute(context.Context, runtimeapi.PeerIdentity, string, string, runtimeapi.CreateOperationRequest) (runtimeapi.Operation, bool, error)
	GetOperation(context.Context, string) (runtimeapi.Operation, error)
	CancelOperation(context.Context, runtimeapi.PeerIdentity, string, string, string, runtimeapi.CancelOperationRequest) (runtimeapi.Operation, bool, error)
	VerifyProxy(context.Context) (runtimeapi.ProxyVerification, error)
	RevealSourceURL(context.Context, runtimeapi.PeerIdentity, string, string, string, runtimeapi.RevealSourceURLRequest) (runtimeapi.RevealSourceURLResponse, error)
	PreviewDiagnostics(context.Context, runtimeapi.PeerIdentity, string, string, string, runtimeapi.DiagnosticsRequest) (runtimeapi.DiagnosticsPreview, error)
	CreateDiagnostics(context.Context, runtimeapi.PeerIdentity, string, string, string, runtimeapi.DiagnosticsRequest) (runtimeapi.DiagnosticsResult, error)
}

type NetworkPreviewer interface {
	PreviewNetwork(context.Context, runtimeapi.PeerIdentity, runtimeapi.NetworkPreviewRequest) (runtimeapi.NetworkPreview, error)
}

type MihomoUpdateOperator interface {
	UploadMihomoUpdateBundle(context.Context, runtimeapi.PeerIdentity, int64, string, io.Reader) (runtimeapi.MihomoUpdateBundle, error)
	PreviewMihomoUpdate(context.Context, runtimeapi.PeerIdentity, runtimeapi.MihomoUpdatePreviewRequest) (runtimeapi.MihomoUpdatePlan, error)
}

type ProductUpdateOperator interface {
	PreviewProductUpdate(
		context.Context,
		runtimeapi.PeerIdentity,
		string,
		string,
		string,
		runtimeapi.ProductUpdatePreviewRequest,
	) (runtimeapi.ProductUpdatePlan, error)
}

type BackupOperator interface {
	PreviewBackup(context.Context, runtimeapi.PeerIdentity, string, string, string, runtimeapi.BackupPreviewRequest) (runtimeapi.BackupPreview, error)
	ExportBackup(context.Context, runtimeapi.PeerIdentity, string, string, string, runtimeapi.BackupExportRequest) (runtimeapi.BackupArchive, error)
	PreviewBackupRestore(context.Context, runtimeapi.PeerIdentity, string, string, string, runtimeapi.BackupRestorePreviewRequest) (runtimeapi.BackupRestorePreview, error)
}

type Server struct {
	observer           Observer
	eventObserver      EventObserver
	trafficObserver    TrafficObserver
	connectionObserver ConnectionObserver
	ruleObserver       RuleObserver
	operator           Operator
	network            NetworkPreviewer
	updates            MihomoUpdateOperator
	productUpdates     ProductUpdateOperator
	backups            BackupOperator
	authorizer         Authorizer
	runtimeVersion     string
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
	operator, _ := observer.(Operator)
	network, _ := observer.(NetworkPreviewer)
	updates, _ := observer.(MihomoUpdateOperator)
	productUpdates, _ := observer.(ProductUpdateOperator)
	backups, _ := observer.(BackupOperator)
	eventObserver, _ := observer.(EventObserver)
	trafficObserver, _ := observer.(TrafficObserver)
	connectionObserver, _ := observer.(ConnectionObserver)
	ruleObserver, _ := observer.(RuleObserver)
	runtimeVersion := ""
	if provider, ok := observer.(interface{ RuntimeVersion() string }); ok {
		runtimeVersion = provider.RuntimeVersion()
	}
	return &Server{
		observer:           observer,
		eventObserver:      eventObserver,
		trafficObserver:    trafficObserver,
		connectionObserver: connectionObserver,
		ruleObserver:       ruleObserver,
		operator:           operator,
		network:            network,
		updates:            updates,
		productUpdates:     productUpdates,
		backups:            backups,
		authorizer:         authorizer,
		runtimeVersion:     runtimeVersion,
	}, nil
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
		ReadTimeout:       10 * time.Minute,
		WriteTimeout:      10 * time.Minute,
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
	mux.HandleFunc("/v1/events", s.handleEvents)
	mux.HandleFunc("/v1/traffic/history", s.handleTrafficHistory)
	mux.HandleFunc("/v1/connections", s.handleConnections)
	mux.HandleFunc("/v1/rules", s.handleRules)
	mux.HandleFunc("/v1/imports", s.handleImport)
	mux.HandleFunc("/v1/advanced-override", s.handleAdvancedOverride)
	mux.HandleFunc("/v1/candidates/preview", s.handleCandidatePreview)
	mux.HandleFunc("/v1/network/preview", s.handleNetworkPreview)
	mux.HandleFunc("/v1/mihomo/update-bundles", s.handleMihomoUpdateBundle)
	mux.HandleFunc("/v1/mihomo/updates/preview", s.handleMihomoUpdatePreview)
	mux.HandleFunc("/v1/product/updates/preview", s.handleProductUpdatePreview)
	mux.HandleFunc("/v1/backups/preview", s.handleBackupPreview)
	mux.HandleFunc("/v1/backups/export", s.handleBackupExport)
	mux.HandleFunc("/v1/backups/restore/preview", s.handleBackupRestorePreview)
	mux.HandleFunc("/v1/operations", s.handleCreateOperation)
	mux.HandleFunc("/v1/operations/", s.handleOperation)
	mux.HandleFunc("/v1/proxy/verify", s.handleProxyVerification)
	mux.HandleFunc("/v1/sources/reveal-url", s.handleRevealSourceURL)
	mux.HandleFunc("/v1/diagnostics/preview", s.handleDiagnosticsPreview)
	mux.HandleFunc("/v1/diagnostics/create", s.handleDiagnosticsCreate)
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		s.writeError(writer, request, http.StatusNotFound, runtimeapi.ErrorInvalidRequest, "unknown Runtime IPC endpoint", false)
	})
	return mux
}

func (s *Server) handleRules(writer http.ResponseWriter, request *http.Request) {
	requestID, _, _, ok := s.validateCommon(writer, request)
	if !ok {
		return
	}
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		s.writeError(writer, request, http.StatusMethodNotAllowed, runtimeapi.ErrorInvalidRequest, "Runtime final rule viewer only accepts GET", false)
		return
	}
	if requestHasBody(request) {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime final rule viewer does not accept a request body", false)
		return
	}
	peer, ok := s.authenticatedPeer(writer, request)
	if !ok {
		return
	}
	if s.ruleObserver == nil {
		s.writeError(writer, request, http.StatusServiceUnavailable, runtimeapi.ErrorServiceUnavailable, "Runtime final rule viewer is unavailable", true)
		return
	}
	allowed := map[string]bool{"content": true, "type": true, "target": true}
	for key := range request.URL.Query() {
		if !allowed[key] {
			s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime final rule query contains an unsupported parameter", false)
			return
		}
	}
	query := runtimeapi.RuleQuery{
		Content: strings.TrimSpace(request.URL.Query().Get("content")),
		Type:    strings.TrimSpace(request.URL.Query().Get("type")),
		Target:  strings.TrimSpace(request.URL.Query().Get("target")),
	}
	for _, filter := range []string{query.Content, query.Type, query.Target} {
		if utf8.RuneCountInString(filter) > runtimeapi.RuleFilterMaxLength {
			s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime final rule filter is too long", false)
			return
		}
	}
	rules, err := s.ruleObserver.Rules(request.Context(), peer, query)
	if err != nil {
		s.writeError(writer, request, http.StatusServiceUnavailable, runtimeapi.ErrorServiceUnavailable, "Runtime final rule viewer is temporarily unavailable", true)
		return
	}
	body, err := json.Marshal(rules)
	if err != nil || len(body) > MaxResponseBytes {
		s.writeError(writer, request, http.StatusServiceUnavailable, runtimeapi.ErrorServiceUnavailable, "Runtime final rule response exceeds the size limit", true)
		return
	}
	writer.Header().Set(HeaderRequestID, requestID)
	s.writeJSON(writer, http.StatusOK, rules)
}

func (s *Server) handleConnections(writer http.ResponseWriter, request *http.Request) {
	requestID, _, _, ok := s.validateCommon(writer, request)
	if !ok {
		return
	}
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		s.writeError(writer, request, http.StatusMethodNotAllowed, runtimeapi.ErrorInvalidRequest, "Runtime connection viewer only accepts GET", false)
		return
	}
	if requestHasBody(request) {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime connection viewer does not accept a request body", false)
		return
	}
	peer, ok := s.authenticatedPeer(writer, request)
	if !ok {
		return
	}
	if s.connectionObserver == nil {
		s.writeError(writer, request, http.StatusServiceUnavailable, runtimeapi.ErrorServiceUnavailable, "Runtime connection viewer is unavailable", true)
		return
	}
	allowed := map[string]bool{"target": true, "process": true, "rule": true, "node": true, "page": true, "page_size": true}
	for key := range request.URL.Query() {
		if !allowed[key] {
			s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime connection query contains an unsupported parameter", false)
			return
		}
	}
	query := runtimeapi.ConnectionQuery{
		Target:   strings.TrimSpace(request.URL.Query().Get("target")),
		Process:  strings.TrimSpace(request.URL.Query().Get("process")),
		Rule:     strings.TrimSpace(request.URL.Query().Get("rule")),
		Node:     strings.TrimSpace(request.URL.Query().Get("node")),
		Page:     1,
		PageSize: runtimeapi.ConnectionPageDefaultSize,
	}
	for _, filter := range []string{query.Target, query.Process, query.Rule, query.Node} {
		if len(filter) > 256 {
			s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime connection filter is too long", false)
			return
		}
	}
	var err error
	if value := request.URL.Query().Get("page"); value != "" {
		query.Page, err = strconv.Atoi(value)
		if err != nil || query.Page <= 0 || query.Page > 1_000_000 {
			s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime connection page is invalid", false)
			return
		}
	}
	if value := request.URL.Query().Get("page_size"); value != "" {
		query.PageSize, err = strconv.Atoi(value)
		if err != nil || query.PageSize <= 0 || query.PageSize > runtimeapi.ConnectionPageMaxSize {
			s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime connection page size is invalid", false)
			return
		}
	}
	page, err := s.connectionObserver.Connections(request.Context(), peer, query)
	if err != nil {
		s.writeError(writer, request, http.StatusServiceUnavailable, runtimeapi.ErrorServiceUnavailable, "Runtime connection viewer is temporarily unavailable", true)
		return
	}
	body, err := json.Marshal(page)
	if err != nil || len(body) > runtimeapi.ConnectionResponseMaxBytes {
		s.writeError(writer, request, http.StatusServiceUnavailable, runtimeapi.ErrorServiceUnavailable, "Runtime connection response exceeds the size limit", true)
		return
	}
	writer.Header().Set(HeaderRequestID, requestID)
	s.writeJSON(writer, http.StatusOK, page)
}

func (s *Server) handleTrafficHistory(writer http.ResponseWriter, request *http.Request) {
	requestID, _, _, ok := s.validateCommon(writer, request)
	if !ok {
		return
	}
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		s.writeError(writer, request, http.StatusMethodNotAllowed, runtimeapi.ErrorInvalidRequest, "Runtime traffic history only accepts GET", false)
		return
	}
	if requestHasBody(request) {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime traffic history does not accept a request body", false)
		return
	}
	peer, ok := s.authenticatedPeer(writer, request)
	if !ok {
		return
	}
	if s.trafficObserver == nil {
		s.writeError(writer, request, http.StatusServiceUnavailable, runtimeapi.ErrorServiceUnavailable, "Runtime traffic history is unavailable", true)
		return
	}
	for key := range request.URL.Query() {
		if key != "after" && key != "since" && key != "limit" {
			s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime traffic history query contains an unsupported parameter", false)
			return
		}
	}
	var historyRequest runtimeapi.TrafficHistoryRequest
	var err error
	if value := request.URL.Query().Get("after"); value != "" {
		historyRequest.After, err = strconv.ParseUint(value, 10, 64)
		if err != nil {
			s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime traffic history after cursor is invalid", false)
			return
		}
	}
	if value := request.URL.Query().Get("since"); value != "" {
		historyRequest.Since, err = time.Parse(time.RFC3339Nano, value)
		if err != nil {
			s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime traffic history since time is invalid", false)
			return
		}
	}
	if historyRequest.After > 0 && !historyRequest.Since.IsZero() {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime traffic history accepts either after or since, not both", false)
		return
	}
	if value := request.URL.Query().Get("limit"); value != "" {
		historyRequest.Limit, err = strconv.Atoi(value)
		if err != nil || historyRequest.Limit <= 0 || historyRequest.Limit > runtimeapi.TrafficHistoryMaxSamples {
			s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime traffic history limit is invalid", false)
			return
		}
	}
	history, err := s.trafficObserver.TrafficHistory(request.Context(), peer, historyRequest)
	if err != nil {
		s.writeError(writer, request, http.StatusServiceUnavailable, runtimeapi.ErrorServiceUnavailable, "Runtime traffic history is temporarily unavailable", true)
		return
	}
	writer.Header().Set(HeaderRequestID, requestID)
	s.writeJSON(writer, http.StatusOK, history)
}

func (s *Server) handleBackupPreview(writer http.ResponseWriter, request *http.Request) {
	requestID, clientType, clientVersion, ok := s.validateCommon(writer, request)
	if !ok {
		return
	}
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		s.writeError(writer, request, http.StatusMethodNotAllowed, runtimeapi.ErrorInvalidRequest, "Runtime backup preview only accepts POST", false)
		return
	}
	if request.URL.RawQuery != "" {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime backup preview does not accept query parameters", false)
		return
	}
	peer, ok := s.authenticatedPeer(writer, request)
	if !ok {
		return
	}
	if s.backups == nil {
		s.writeError(writer, request, http.StatusServiceUnavailable, runtimeapi.ErrorServiceUnavailable, "Runtime backup service is unavailable", true)
		return
	}
	var previewRequest runtimeapi.BackupPreviewRequest
	if err := decodeStrictJSON(request.Body, MaxRequestBytes, &previewRequest); err != nil {
		s.writeDecodeError(writer, request, err)
		return
	}
	preview, err := s.backups.PreviewBackup(
		request.Context(), peer, clientType, clientVersion, requestID, previewRequest,
	)
	if err != nil {
		s.writeImmediateError(writer, request, err)
		return
	}
	writer.Header().Set(HeaderRequestID, requestID)
	s.writeJSON(writer, http.StatusOK, preview)
}

func (s *Server) handleBackupExport(writer http.ResponseWriter, request *http.Request) {
	requestID, clientType, clientVersion, ok := s.validateCommon(writer, request)
	if !ok {
		return
	}
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		s.writeError(writer, request, http.StatusMethodNotAllowed, runtimeapi.ErrorInvalidRequest, "Runtime backup export only accepts POST", false)
		return
	}
	if request.URL.RawQuery != "" {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime backup export does not accept query parameters", false)
		return
	}
	peer, ok := s.authenticatedPeer(writer, request)
	if !ok {
		return
	}
	if s.backups == nil {
		s.writeError(writer, request, http.StatusServiceUnavailable, runtimeapi.ErrorServiceUnavailable, "Runtime backup service is unavailable", true)
		return
	}
	if !s.validateWriteCompatibility(writer, request, clientVersion) {
		return
	}
	var exportRequest runtimeapi.BackupExportRequest
	if err := decodeStrictJSON(request.Body, MaxRequestBytes, &exportRequest); err != nil {
		s.writeDecodeError(writer, request, err)
		return
	}
	archive, err := s.backups.ExportBackup(
		request.Context(), peer, clientType, clientVersion, requestID, exportRequest,
	)
	if err != nil {
		s.writeImmediateError(writer, request, err)
		return
	}
	if archive.Size != int64(len(archive.Body)) ||
		archive.Size <= 0 ||
		archive.Size > runtimeapi.RuntimeBackupMaxBytes ||
		len(archive.SHA256) != sha256.Size*2 ||
		!validBackupFileName(archive.FileName) {
		s.writeError(writer, request, http.StatusInternalServerError, runtimeapi.ErrorInternal, "Runtime backup service returned invalid archive metadata", false)
		return
	}
	digest := sha256.Sum256(archive.Body)
	if !strings.EqualFold(hex.EncodeToString(digest[:]), archive.SHA256) {
		s.writeError(writer, request, http.StatusInternalServerError, runtimeapi.ErrorInternal, "Runtime backup service returned an archive with invalid integrity metadata", false)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", runtimeapi.RuntimeBackupContentType)
	writer.Header().Set("Content-Disposition", `attachment; filename="`+archive.FileName+`"`)
	writer.Header().Set(HeaderRequestID, requestID)
	writer.Header().Set(HeaderProtocolVersion, strconv.Itoa(runtimeapi.ProtocolVersion))
	writer.Header().Set(HeaderContentSize, strconv.FormatInt(archive.Size, 10))
	writer.Header().Set(HeaderContentSHA256, strings.ToLower(archive.SHA256))
	writer.Header().Set(HeaderBackupCreatedAt, archive.CreatedAt.UTC().Format(time.RFC3339Nano))
	writer.Header().Set(HeaderBackupRestorable, strconv.FormatBool(archive.Restorable))
	writer.Header().Set(HeaderBackupSecrets, strconv.FormatBool(archive.IncludeSecrets))
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(archive.Body)
}

func (s *Server) handleBackupRestorePreview(writer http.ResponseWriter, request *http.Request) {
	requestID, clientType, clientVersion, ok := s.validateCommon(writer, request)
	if !ok {
		return
	}
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		s.writeError(writer, request, http.StatusMethodNotAllowed, runtimeapi.ErrorInvalidRequest, "Runtime backup restore preview only accepts POST", false)
		return
	}
	if request.URL.RawQuery != "" {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime backup restore preview does not accept query parameters", false)
		return
	}
	peer, ok := s.authenticatedPeer(writer, request)
	if !ok {
		return
	}
	if s.backups == nil {
		s.writeError(writer, request, http.StatusServiceUnavailable, runtimeapi.ErrorServiceUnavailable, "Runtime backup restore preview is unavailable", true)
		return
	}
	var previewRequest runtimeapi.BackupRestorePreviewRequest
	if err := decodeStrictJSON(request.Body, MaxRequestBytes, &previewRequest); err != nil {
		s.writeDecodeError(writer, request, err)
		return
	}
	if !validContentID(previewRequest.ContentID) {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime backup restore preview request is invalid", false)
		return
	}
	preview, err := s.backups.PreviewBackupRestore(
		request.Context(), peer, clientType, clientVersion, requestID, previewRequest,
	)
	if err != nil {
		s.writeImmediateError(writer, request, err)
		return
	}
	writer.Header().Set(HeaderRequestID, requestID)
	s.writeJSON(writer, http.StatusOK, preview)
}

func (s *Server) handleMihomoUpdateBundle(writer http.ResponseWriter, request *http.Request) {
	requestID, _, clientVersion, ok := s.validateCommon(writer, request)
	if !ok {
		return
	}
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		s.writeError(writer, request, http.StatusMethodNotAllowed, runtimeapi.ErrorInvalidRequest, "Mihomo update bundles only accept POST", false)
		return
	}
	if request.URL.RawQuery != "" {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Mihomo update bundles do not accept query parameters", false)
		return
	}
	peer, ok := s.authenticatedPeer(writer, request)
	if !ok {
		return
	}
	if s.updates == nil {
		s.writeError(writer, request, http.StatusServiceUnavailable, runtimeapi.ErrorServiceUnavailable, "Mihomo update bundle service is unavailable", true)
		return
	}
	if !s.validateWriteCompatibility(writer, request, clientVersion) {
		return
	}
	if strings.TrimSpace(strings.Split(request.Header.Get("Content-Type"), ";")[0]) != runtimeapi.MihomoUpdateBundleContentType {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Mihomo update bundle content type is invalid", false)
		return
	}
	size, err := strconv.ParseInt(request.Header.Get(HeaderContentSize), 10, 64)
	if err != nil || size <= 0 || size > runtimeupdate.MaxBundleBytes {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Mihomo update bundle size is missing or invalid", false)
		return
	}
	digest := request.Header.Get(HeaderContentSHA256)
	decodedDigest, err := hex.DecodeString(digest)
	if err != nil || len(decodedDigest) != sha256.Size {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Mihomo update bundle SHA-256 is missing or invalid", false)
		return
	}
	bundle, err := s.updates.UploadMihomoUpdateBundle(
		request.Context(),
		peer,
		size,
		digest,
		io.LimitReader(request.Body, runtimeupdate.MaxBundleBytes+1),
	)
	if err != nil {
		s.writeImmediateError(writer, request, err)
		return
	}
	writer.Header().Set(HeaderRequestID, requestID)
	s.writeJSON(writer, http.StatusCreated, bundle)
}

func (s *Server) handleMihomoUpdatePreview(writer http.ResponseWriter, request *http.Request) {
	requestID, _, clientVersion, ok := s.validateCommon(writer, request)
	if !ok {
		return
	}
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		s.writeError(writer, request, http.StatusMethodNotAllowed, runtimeapi.ErrorInvalidRequest, "Mihomo update preview only accepts POST", false)
		return
	}
	if request.URL.RawQuery != "" {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Mihomo update preview does not accept query parameters", false)
		return
	}
	peer, ok := s.authenticatedPeer(writer, request)
	if !ok {
		return
	}
	if s.updates == nil {
		s.writeError(writer, request, http.StatusServiceUnavailable, runtimeapi.ErrorServiceUnavailable, "Mihomo update preview is unavailable", true)
		return
	}
	if !s.validateWriteCompatibility(writer, request, clientVersion) {
		return
	}
	var previewRequest runtimeapi.MihomoUpdatePreviewRequest
	if err := decodeStrictJSON(request.Body, MaxRequestBytes, &previewRequest); err != nil {
		s.writeDecodeError(writer, request, err)
		return
	}
	preview, err := s.updates.PreviewMihomoUpdate(request.Context(), peer, previewRequest)
	if err != nil {
		s.writeImmediateError(writer, request, err)
		return
	}
	writer.Header().Set(HeaderRequestID, requestID)
	s.writeJSON(writer, http.StatusOK, preview)
}

func (s *Server) handleProductUpdatePreview(writer http.ResponseWriter, request *http.Request) {
	requestID, clientType, clientVersion, ok := s.validateCommon(writer, request)
	if !ok {
		return
	}
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		s.writeError(writer, request, http.StatusMethodNotAllowed, runtimeapi.ErrorInvalidRequest, "Runtime product update preview only accepts POST", false)
		return
	}
	if request.URL.RawQuery != "" {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime product update preview does not accept query parameters", false)
		return
	}
	peer, ok := s.authenticatedPeer(writer, request)
	if !ok {
		return
	}
	if s.productUpdates == nil {
		s.writeError(writer, request, http.StatusServiceUnavailable, runtimeapi.ErrorServiceUnavailable, "Runtime product update preview is unavailable", true)
		return
	}
	if !s.validateWriteCompatibility(writer, request, clientVersion) {
		return
	}
	var previewRequest runtimeapi.ProductUpdatePreviewRequest
	if err := decodeStrictJSON(request.Body, MaxRequestBytes, &previewRequest); err != nil {
		s.writeDecodeError(writer, request, err)
		return
	}
	preview, err := s.productUpdates.PreviewProductUpdate(
		request.Context(),
		peer,
		clientType,
		clientVersion,
		requestID,
		previewRequest,
	)
	if err != nil {
		s.writeImmediateError(writer, request, err)
		return
	}
	writer.Header().Set(HeaderRequestID, requestID)
	s.writeJSON(writer, http.StatusOK, preview)
}

func (s *Server) handleNetworkPreview(writer http.ResponseWriter, request *http.Request) {
	requestID, _, _, ok := s.validateCommon(writer, request)
	if !ok {
		return
	}
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		s.writeError(writer, request, http.StatusMethodNotAllowed, runtimeapi.ErrorInvalidRequest, "Runtime network preview only accepts POST", false)
		return
	}
	if request.URL.RawQuery != "" {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime network preview does not accept query parameters", false)
		return
	}
	peer, ok := s.authenticatedPeer(writer, request)
	if !ok {
		return
	}
	if s.network == nil {
		s.writeError(writer, request, http.StatusServiceUnavailable, runtimeapi.ErrorServiceUnavailable, "Runtime network preview is unavailable", true)
		return
	}
	var previewRequest runtimeapi.NetworkPreviewRequest
	if err := decodeStrictJSON(request.Body, MaxRequestBytes, &previewRequest); err != nil {
		s.writeDecodeError(writer, request, err)
		return
	}
	preview, err := s.network.PreviewNetwork(request.Context(), peer, previewRequest)
	if err != nil {
		s.writeImmediateError(writer, request, err)
		return
	}
	writer.Header().Set(HeaderRequestID, requestID)
	s.writeJSON(writer, http.StatusOK, preview)
}

func (s *Server) handleEvents(writer http.ResponseWriter, request *http.Request) {
	requestID, _, _, ok := s.validateCommon(writer, request)
	if !ok {
		return
	}
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		s.writeError(writer, request, http.StatusMethodNotAllowed, runtimeapi.ErrorInvalidRequest, "Runtime events only accept GET", false)
		return
	}
	if requestHasBody(request) {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime events do not accept a request body", false)
		return
	}
	query := request.URL.Query()
	afterValues, exists := query["after"]
	if !exists || len(query) != 1 || len(afterValues) != 1 || afterValues[0] == "" {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime event cursor is missing or invalid", false)
		return
	}
	after, err := strconv.ParseUint(afterValues[0], 10, 64)
	if err != nil {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime event cursor is missing or invalid", false)
		return
	}
	peer, ok := s.authenticatedPeer(writer, request)
	if !ok {
		return
	}
	if s.eventObserver == nil {
		s.writeError(writer, request, http.StatusServiceUnavailable, runtimeapi.ErrorServiceUnavailable, "Runtime event service is unavailable", true)
		return
	}

	events, _, err := s.eventObserver.Events(
		request.Context(),
		peer,
		after,
		runtimestate.DefaultEventBatchSize,
	)
	if err != nil {
		s.writeEventError(writer, request, err)
		return
	}

	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/x-ndjson")
	writer.Header().Set(HeaderRequestID, requestID)
	writer.Header().Set(HeaderProtocolVersion, strconv.Itoa(runtimeapi.ProtocolVersion))
	writer.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(writer)
	_ = controller.SetWriteDeadline(time.Time{})
	flusher, _ := writer.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	encoder := json.NewEncoder(writer)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		for _, event := range events {
			if err := encoder.Encode(runtimeprivacy.SanitizeEvent(event)); err != nil {
				return
			}
			after = event.Cursor
		}
		if len(events) > 0 && flusher != nil {
			flusher.Flush()
		}
		if len(events) == runtimestate.DefaultEventBatchSize {
			events, _, err = s.eventObserver.Events(
				request.Context(),
				peer,
				after,
				runtimestate.DefaultEventBatchSize,
			)
			if err != nil {
				return
			}
			continue
		}
		select {
		case <-request.Context().Done():
			return
		case <-ticker.C:
			events, _, err = s.eventObserver.Events(
				request.Context(),
				peer,
				after,
				runtimestate.DefaultEventBatchSize,
			)
			if err != nil {
				return
			}
		}
	}
}

func (s *Server) handleSnapshot(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json")

	requestID, _, _, ok := s.validateCommon(writer, request)
	if !ok {
		return
	}
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		s.writeError(writer, request, http.StatusMethodNotAllowed, runtimeapi.ErrorInvalidRequest, "Runtime snapshot only accepts GET", false)
		return
	}
	if request.URL.RawQuery != "" || requestHasBody(request) {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime snapshot does not accept query parameters or a request body", false)
		return
	}
	peer, ok := s.authenticatedPeer(writer, request)
	if !ok {
		return
	}
	snapshot, err := s.observer.Observe(request.Context(), peer)
	if err != nil {
		s.writeError(writer, request, http.StatusServiceUnavailable, runtimeapi.ErrorServiceUnavailable, "Runtime state is temporarily unavailable", true)
		return
	}
	writer.Header().Set(HeaderRequestID, requestID)
	writer.Header().Set(HeaderProtocolVersion, strconv.Itoa(runtimeapi.ProtocolVersion))
	writer.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(writer).Encode(runtimeprivacy.SanitizeSnapshot(snapshot))
}

func (s *Server) handleImport(writer http.ResponseWriter, request *http.Request) {
	requestID, _, clientVersion, ok := s.validateCommon(writer, request)
	if !ok {
		return
	}
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		s.writeError(writer, request, http.StatusMethodNotAllowed, runtimeapi.ErrorInvalidRequest, "Runtime imports only accept POST", false)
		return
	}
	if request.URL.RawQuery != "" {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime imports do not accept query parameters", false)
		return
	}
	peer, ok := s.authenticatedPeer(writer, request)
	if !ok {
		return
	}
	if s.operator == nil {
		s.writeError(writer, request, http.StatusServiceUnavailable, runtimeapi.ErrorServiceUnavailable, "Runtime operation service is unavailable", true)
		return
	}
	if !s.validateWriteCompatibility(writer, request, clientVersion) {
		return
	}
	contentType := request.Header.Get("Content-Type")
	maxBytes := int64(runtimestate.MaxImportBytes)
	mediaType := strings.TrimSpace(strings.Split(contentType, ";")[0])
	if strings.EqualFold(mediaType, runtimeapi.RuntimeBackupContentType) {
		maxBytes = runtimeapi.RuntimeBackupMaxBytes
	} else if strings.EqualFold(mediaType, runtimeapi.ProductUpdateBundleContentType) {
		maxBytes = runtimeapi.RuntimeProductUpdateMaxBytes
	}
	size, err := strconv.ParseInt(request.Header.Get(HeaderContentSize), 10, 64)
	if err != nil || size <= 0 || size > maxBytes {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime import size is missing or invalid", false)
		return
	}
	digest := request.Header.Get(HeaderContentSHA256)
	decodedDigest, err := hex.DecodeString(digest)
	if err != nil || len(decodedDigest) != 32 {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime import SHA-256 is missing or invalid", false)
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxBytes+1))
	if err != nil {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime import body could not be read", false)
		return
	}
	if int64(len(body)) > maxBytes {
		s.writeError(writer, request, http.StatusRequestEntityTooLarge, runtimeapi.ErrorRequestTooLarge, "Runtime import exceeds the hard size limit", false)
		return
	}
	if int64(len(body)) != size {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime import body does not match its declared size", false)
		return
	}
	content, err := s.operator.UploadImport(request.Context(), peer, contentType, size, digest, body)
	if err != nil {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime rejected the imported content", false)
		return
	}
	writer.Header().Set(HeaderRequestID, requestID)
	s.writeJSON(writer, http.StatusCreated, content)
}

func (s *Server) handleAdvancedOverride(writer http.ResponseWriter, request *http.Request) {
	requestID, clientType, clientVersion, ok := s.validateCommon(writer, request)
	if !ok {
		return
	}
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		s.writeError(writer, request, http.StatusMethodNotAllowed, runtimeapi.ErrorInvalidRequest, "Runtime advanced override only accepts GET", false)
		return
	}
	query := request.URL.Query()
	if len(query) != 1 || len(query["reveal"]) != 1 || query.Get("reveal") != "1" || requestHasBody(request) {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime advanced override requires reveal=1 and does not accept a request body", false)
		return
	}
	peer, ok := s.authenticatedPeer(writer, request)
	if !ok {
		return
	}
	if s.operator == nil {
		s.writeError(writer, request, http.StatusServiceUnavailable, runtimeapi.ErrorServiceUnavailable, "Runtime advanced override service is unavailable", true)
		return
	}
	document, err := s.operator.GetAdvancedOverride(
		request.Context(),
		peer,
		clientType,
		clientVersion,
		requestID,
		true,
	)
	if err != nil {
		s.writeError(writer, request, http.StatusServiceUnavailable, runtimeapi.ErrorServiceUnavailable, "Runtime advanced override is temporarily unavailable", true)
		return
	}
	writer.Header().Set(HeaderRequestID, requestID)
	s.writeJSON(writer, http.StatusOK, document)
}

func (s *Server) handleCandidatePreview(writer http.ResponseWriter, request *http.Request) {
	requestID, _, _, ok := s.validateCommon(writer, request)
	if !ok {
		return
	}
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		s.writeError(writer, request, http.StatusMethodNotAllowed, runtimeapi.ErrorInvalidRequest, "Runtime candidate preview only accepts POST", false)
		return
	}
	if request.URL.RawQuery != "" {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime candidate preview does not accept query parameters", false)
		return
	}
	peer, ok := s.authenticatedPeer(writer, request)
	if !ok {
		return
	}
	if s.operator == nil {
		s.writeError(writer, request, http.StatusServiceUnavailable, runtimeapi.ErrorServiceUnavailable, "Runtime candidate preview is unavailable", true)
		return
	}
	var previewRequest runtimeapi.PreviewCandidateRequest
	if err := decodeStrictJSON(request.Body, MaxRequestBytes, &previewRequest); err != nil {
		s.writeDecodeError(writer, request, err)
		return
	}
	hasContent := validContentID(previewRequest.ContentID)
	hasSource := validSourceID(previewRequest.SourceID)
	if hasContent == hasSource ||
		(previewRequest.OverrideContentID != "" && !validContentID(previewRequest.OverrideContentID)) {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime candidate preview request is invalid", false)
		return
	}
	preview, err := s.operator.PreviewCandidate(request.Context(), peer, previewRequest)
	if err != nil {
		s.writeImmediateError(writer, request, err)
		return
	}
	writer.Header().Set(HeaderRequestID, requestID)
	s.writeJSON(writer, http.StatusOK, runtimeprivacy.SanitizeCandidatePreview(preview))
}

func (s *Server) handleCreateOperation(writer http.ResponseWriter, request *http.Request) {
	requestID, clientType, clientVersion, ok := s.validateCommon(writer, request)
	if !ok {
		return
	}
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		s.writeError(writer, request, http.StatusMethodNotAllowed, runtimeapi.ErrorInvalidRequest, "Runtime operations only accept POST", false)
		return
	}
	if request.URL.RawQuery != "" {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime operations do not accept query parameters", false)
		return
	}
	peer, ok := s.authenticatedPeer(writer, request)
	if !ok {
		return
	}
	if s.operator == nil {
		s.writeError(writer, request, http.StatusServiceUnavailable, runtimeapi.ErrorServiceUnavailable, "Runtime operation service is unavailable", true)
		return
	}
	if !s.validateWriteCompatibility(writer, request, clientVersion) {
		return
	}
	var operationRequest runtimeapi.CreateOperationRequest
	if err := decodeStrictJSON(request.Body, MaxRequestBytes, &operationRequest); err != nil {
		s.writeDecodeError(writer, request, err)
		return
	}
	if operationRequest.RequestID != requestID || !validAction(operationRequest.Action) {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime operation request is invalid", false)
		return
	}
	operation, _, err := s.operator.Execute(request.Context(), peer, clientType, clientVersion, operationRequest)
	if err != nil {
		s.writeOperationError(writer, request, err)
		return
	}
	writer.Header().Set(HeaderRequestID, requestID)
	s.writeJSON(writer, http.StatusAccepted, runtimeapi.OperationResponse{Operation: runtimeprivacy.SanitizeOperation(operation)})
}

func (s *Server) handleOperation(writer http.ResponseWriter, request *http.Request) {
	requestID, clientType, clientVersion, ok := s.validateCommon(writer, request)
	if !ok {
		return
	}
	peer, ok := s.authenticatedPeer(writer, request)
	if !ok {
		return
	}
	if s.operator == nil {
		s.writeError(writer, request, http.StatusServiceUnavailable, runtimeapi.ErrorServiceUnavailable, "Runtime operation service is unavailable", true)
		return
	}
	suffix := strings.TrimPrefix(request.URL.Path, "/v1/operations/")
	parts := strings.Split(suffix, "/")
	if len(parts) == 0 || !validIdentifier(parts[0], 128) || !strings.HasPrefix(parts[0], "op_") {
		s.writeError(writer, request, http.StatusNotFound, runtimeapi.ErrorNotFound, "Runtime operation was not found", false)
		return
	}
	switch {
	case len(parts) == 1 && request.Method == http.MethodGet:
		if request.URL.RawQuery != "" || requestHasBody(request) {
			s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime operation query is invalid", false)
			return
		}
		operation, err := s.operator.GetOperation(request.Context(), parts[0])
		if err != nil {
			s.writeOperationError(writer, request, err)
			return
		}
		writer.Header().Set(HeaderRequestID, requestID)
		s.writeJSON(writer, http.StatusOK, runtimeapi.OperationResponse{Operation: runtimeprivacy.SanitizeOperation(operation)})
	case len(parts) == 2 && parts[1] == "cancel" && request.Method == http.MethodPost:
		if request.URL.RawQuery != "" {
			s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime cancellation query is invalid", false)
			return
		}
		if !s.validateWriteCompatibility(writer, request, clientVersion) {
			return
		}
		var cancellation runtimeapi.CancelOperationRequest
		if err := decodeStrictJSON(request.Body, MaxRequestBytes, &cancellation); err != nil {
			s.writeDecodeError(writer, request, err)
			return
		}
		if cancellation.RequestID != requestID {
			s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime cancellation request ID does not match its header", false)
			return
		}
		operation, _, err := s.operator.CancelOperation(request.Context(), peer, clientType, clientVersion, parts[0], cancellation)
		if err != nil {
			s.writeOperationError(writer, request, err)
			return
		}
		writer.Header().Set(HeaderRequestID, requestID)
		s.writeJSON(writer, http.StatusOK, runtimeapi.OperationResponse{Operation: runtimeprivacy.SanitizeOperation(operation)})
	default:
		s.writeError(writer, request, http.StatusNotFound, runtimeapi.ErrorNotFound, "Runtime operation endpoint was not found", false)
	}
}

func (s *Server) handleProxyVerification(writer http.ResponseWriter, request *http.Request) {
	requestID, _, _, ok := s.validateCommon(writer, request)
	if !ok {
		return
	}
	if request.Method != http.MethodGet || request.URL.RawQuery != "" || requestHasBody(request) {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime proxy verification only accepts an empty GET request", false)
		return
	}
	if _, ok := s.authenticatedPeer(writer, request); !ok {
		return
	}
	if s.operator == nil {
		s.writeError(writer, request, http.StatusServiceUnavailable, runtimeapi.ErrorServiceUnavailable, "Runtime operation service is unavailable", true)
		return
	}
	verification, err := s.operator.VerifyProxy(request.Context())
	if err != nil {
		s.writeError(writer, request, http.StatusServiceUnavailable, runtimeapi.ErrorServiceUnavailable, "Runtime proxy verification is unavailable", true)
		return
	}
	if verification.Error != nil {
		copy := *verification.Error
		copy.Message = runtimeprivacy.RedactText(copy.Message)
		verification.Error = &copy
	}
	writer.Header().Set(HeaderRequestID, requestID)
	s.writeJSON(writer, http.StatusOK, verification)
}

func (s *Server) handleRevealSourceURL(writer http.ResponseWriter, request *http.Request) {
	requestID, clientType, clientVersion, ok := s.validateCommon(writer, request)
	if !ok {
		return
	}
	if request.Method != http.MethodPost || request.URL.RawQuery != "" {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Source URL reveal only accepts a POST request without query parameters", false)
		return
	}
	peer, ok := s.authenticatedPeer(writer, request)
	if !ok {
		return
	}
	if s.operator == nil {
		s.writeError(writer, request, http.StatusServiceUnavailable, runtimeapi.ErrorServiceUnavailable, "Runtime source reveal service is unavailable", true)
		return
	}
	var revealRequest runtimeapi.RevealSourceURLRequest
	if err := decodeStrictJSON(request.Body, MaxRequestBytes, &revealRequest); err != nil {
		s.writeDecodeError(writer, request, err)
		return
	}
	if !revealRequest.Confirm {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Revealing a source URL requires explicit confirmation", false)
		return
	}
	response, err := s.operator.RevealSourceURL(
		request.Context(),
		peer,
		clientType,
		clientVersion,
		requestID,
		revealRequest,
	)
	if err != nil {
		s.writeImmediateError(writer, request, err)
		return
	}
	writer.Header().Set(HeaderRequestID, requestID)
	s.writeJSON(writer, http.StatusOK, response)
}

func (s *Server) handleDiagnosticsPreview(writer http.ResponseWriter, request *http.Request) {
	requestID, clientType, clientVersion, ok := s.validateCommon(writer, request)
	if !ok {
		return
	}
	if request.Method != http.MethodPost || request.URL.RawQuery != "" {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime diagnostics preview only accepts a POST request without query parameters", false)
		return
	}
	peer, ok := s.authenticatedPeer(writer, request)
	if !ok {
		return
	}
	if s.operator == nil {
		s.writeError(writer, request, http.StatusServiceUnavailable, runtimeapi.ErrorServiceUnavailable, "Runtime diagnostics preview is unavailable", true)
		return
	}
	var diagnosticsRequest runtimeapi.DiagnosticsRequest
	if err := decodeStrictJSON(request.Body, MaxRequestBytes, &diagnosticsRequest); err != nil {
		s.writeDecodeError(writer, request, err)
		return
	}
	preview, err := s.operator.PreviewDiagnostics(
		request.Context(),
		peer,
		clientType,
		clientVersion,
		requestID,
		diagnosticsRequest,
	)
	if err != nil {
		s.writeImmediateError(writer, request, err)
		return
	}
	writer.Header().Set(HeaderRequestID, requestID)
	s.writeJSON(writer, http.StatusOK, preview)
}

func (s *Server) handleDiagnosticsCreate(writer http.ResponseWriter, request *http.Request) {
	requestID, clientType, clientVersion, ok := s.validateCommon(writer, request)
	if !ok {
		return
	}
	if request.Method != http.MethodPost || request.URL.RawQuery != "" {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime diagnostics creation only accepts a POST request without query parameters", false)
		return
	}
	peer, ok := s.authenticatedPeer(writer, request)
	if !ok {
		return
	}
	if s.operator == nil {
		s.writeError(writer, request, http.StatusServiceUnavailable, runtimeapi.ErrorServiceUnavailable, "Runtime diagnostics creation is unavailable", true)
		return
	}
	if !s.validateWriteCompatibility(writer, request, clientVersion) {
		return
	}
	var diagnosticsRequest runtimeapi.DiagnosticsRequest
	if err := decodeStrictJSON(request.Body, MaxRequestBytes, &diagnosticsRequest); err != nil {
		s.writeDecodeError(writer, request, err)
		return
	}
	if (diagnosticsRequest.IncludeRawConfig ||
		diagnosticsRequest.IncludeFullLogs ||
		diagnosticsRequest.IncludeNetworkInfo) &&
		!diagnosticsRequest.ConfirmSensitive {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Sensitive diagnostics require explicit confirmation after reviewing the preview", false)
		return
	}
	result, err := s.operator.CreateDiagnostics(
		request.Context(),
		peer,
		clientType,
		clientVersion,
		requestID,
		diagnosticsRequest,
	)
	if err != nil {
		s.writeImmediateError(writer, request, err)
		return
	}
	writer.Header().Set(HeaderRequestID, requestID)
	s.writeJSON(writer, http.StatusCreated, result)
}

func (s *Server) validateCommon(writer http.ResponseWriter, request *http.Request) (string, string, string, bool) {
	requestID := request.Header.Get(HeaderRequestID)
	if !validIdentifier(requestID, 128) {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime request ID is missing or invalid", false)
		return "", "", "", false
	}
	versionText := request.Header.Get(HeaderProtocolVersion)
	version, err := strconv.Atoi(versionText)
	if err != nil || version != runtimeapi.ProtocolVersion {
		s.writeError(writer, request, http.StatusUpgradeRequired, runtimeapi.ErrorProtocolUnsupported, "Runtime protocol version is not supported", false)
		return "", "", "", false
	}
	clientType := request.Header.Get(HeaderClientType)
	if clientType == "" || len(clientType) > 64 || hasControlCharacter(clientType) {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime client type is missing or invalid", false)
		return "", "", "", false
	}
	clientVersion := request.Header.Get(HeaderClientVersion)
	if clientVersion == "" || len(clientVersion) > 128 || hasControlCharacter(clientVersion) {
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime client version is missing or invalid", false)
		return "", "", "", false
	}
	return requestID, clientType, clientVersion, true
}

func (s *Server) validateWriteCompatibility(
	writer http.ResponseWriter,
	request *http.Request,
	clientVersion string,
) bool {
	if s.runtimeVersion == "" || clientVersion == s.runtimeVersion {
		return true
	}
	s.writeError(
		writer,
		request,
		http.StatusUpgradeRequired,
		runtimeapi.ErrorProtocolUnsupported,
		"Runtime client version is incompatible; restart or update the client before modifying state",
		false,
	)
	return false
}

func (s *Server) authenticatedPeer(writer http.ResponseWriter, request *http.Request) (runtimeapi.PeerIdentity, bool) {
	peerValue, ok := request.Context().Value(peerContextKey{}).(peerContextValue)
	if !ok || peerValue.err != nil {
		s.writeError(writer, request, http.StatusForbidden, runtimeapi.ErrorUnauthorized, "Runtime could not verify the local peer identity", false)
		return runtimeapi.PeerIdentity{}, false
	}
	if err := s.authorizer.Authorize(peerValue.identity); err != nil {
		s.writeError(writer, request, http.StatusForbidden, runtimeapi.ErrorUnauthorized, "Runtime peer is not authorized", false)
		return runtimeapi.PeerIdentity{}, false
	}
	return peerValue.identity, true
}

func (s *Server) writeDecodeError(writer http.ResponseWriter, request *http.Request, err error) {
	if errors.Is(err, ErrJSONTooLarge) {
		s.writeError(writer, request, http.StatusRequestEntityTooLarge, runtimeapi.ErrorRequestTooLarge, "Runtime request exceeds the hard size limit", false)
		return
	}
	s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime request JSON is invalid", false)
}

func (s *Server) writeOperationError(writer http.ResponseWriter, request *http.Request, err error) {
	var revisionError *runtimestate.RevisionConflictError
	switch {
	case errors.As(err, &revisionError):
		s.writeErrorEnvelope(writer, request, http.StatusConflict, runtimeapi.ErrorEnvelope{
			ProtocolVersion: runtimeapi.ProtocolVersion,
			Error: runtimeapi.ProtocolError{
				Code:      runtimeapi.ErrorRevisionConflict,
				Message:   "Runtime Snapshot revision has changed",
				Retryable: false,
			},
			CurrentRevision: revisionError.Current,
		})
	case errors.Is(err, runtimestate.ErrRequestConflict):
		s.writeError(writer, request, http.StatusConflict, runtimeapi.ErrorRequestConflict, "Runtime request ID is already used by another request", false)
	case errors.Is(err, runtimestate.ErrBusy):
		s.writeError(writer, request, http.StatusTooManyRequests, runtimeapi.ErrorBusy, "Runtime operation queue is full", true)
	case errors.Is(err, runtimestate.ErrOperationNotFound):
		s.writeError(writer, request, http.StatusNotFound, runtimeapi.ErrorNotFound, "Runtime operation was not found", false)
	case errors.Is(err, runtimestate.ErrNotCancellable):
		s.writeError(writer, request, http.StatusConflict, runtimeapi.ErrorNotCancellable, "Runtime operation is no longer cancellable", false)
	case errors.Is(err, runtimestate.ErrImportNotFound):
		s.writeError(writer, request, http.StatusNotFound, runtimeapi.ErrorNotFound, "Runtime imported content was not found", false)
	case errors.Is(err, runtimestate.ErrImportExpired):
		s.writeError(writer, request, http.StatusConflict, runtimeapi.ErrorContentExpired, "Runtime imported content has expired", false)
	case errors.Is(err, runtimestate.ErrImportConsumed):
		s.writeError(writer, request, http.StatusConflict, runtimeapi.ErrorContentConsumed, "Runtime imported content is already reserved or consumed", false)
	case errors.Is(err, runtimestate.ErrImportOwner):
		s.writeError(writer, request, http.StatusForbidden, runtimeapi.ErrorUnauthorized, "Runtime imported content belongs to another caller", false)
	default:
		s.writeError(writer, request, http.StatusServiceUnavailable, runtimeapi.ErrorServiceUnavailable, "Runtime operation state is temporarily unavailable", true)
	}
}

func (s *Server) writeEventError(writer http.ResponseWriter, request *http.Request, err error) {
	var cursorError *runtimestate.CursorExpiredError
	switch {
	case errors.As(err, &cursorError):
		s.writeErrorEnvelope(writer, request, http.StatusGone, runtimeapi.ErrorEnvelope{
			Error: runtimeapi.ProtocolError{
				Code:      runtimeapi.ErrorCursorExpired,
				Message:   "Runtime event cursor has expired; read a fresh Snapshot before subscribing again",
				Retryable: false,
			},
			EarliestCursor: cursorError.Earliest,
		})
	case errors.Is(err, runtimestate.ErrInvalidEventCursor):
		s.writeError(writer, request, http.StatusBadRequest, runtimeapi.ErrorInvalidRequest, "Runtime event cursor is ahead of the current state", false)
	default:
		s.writeError(writer, request, http.StatusServiceUnavailable, runtimeapi.ErrorServiceUnavailable, "Runtime events are temporarily unavailable", true)
	}
}

func (s *Server) writeImmediateError(writer http.ResponseWriter, request *http.Request, err error) {
	var exposed interface {
		ProtocolCode() string
		ProtocolMessage() string
		ProtocolRetryable() bool
	}
	if !errors.As(err, &exposed) {
		s.writeOperationError(writer, request, err)
		return
	}
	status := http.StatusBadRequest
	switch exposed.ProtocolCode() {
	case runtimeapi.ErrorServiceUnavailable:
		status = http.StatusServiceUnavailable
	case runtimeapi.ErrorUnauthorized:
		status = http.StatusForbidden
	case runtimeapi.ErrorProtocolUnsupported:
		status = http.StatusUpgradeRequired
	}
	s.writeError(
		writer,
		request,
		status,
		exposed.ProtocolCode(),
		exposed.ProtocolMessage(),
		exposed.ProtocolRetryable(),
	)
}

func (s *Server) writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set(HeaderProtocolVersion, strconv.Itoa(runtimeapi.ProtocolVersion))
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func (s *Server) writeErrorEnvelope(
	writer http.ResponseWriter,
	request *http.Request,
	status int,
	response runtimeapi.ErrorEnvelope,
) {
	requestID := request.Header.Get(HeaderRequestID)
	if !validIdentifier(requestID, 128) {
		requestID = ""
	}
	response.ProtocolVersion = runtimeapi.ProtocolVersion
	response.RequestID = requestID
	response.Error.Message = runtimeprivacy.RedactText(response.Error.Message)
	if response.Error.Code == runtimeapi.ErrorProtocolUnsupported {
		response.SupportedVersions = []int{runtimeapi.ProtocolVersion}
	}
	s.writeJSON(writer, status, response)
}

func requestHasBody(request *http.Request) bool {
	return request.ContentLength > 0 || len(request.TransferEncoding) > 0
}

func validBackupFileName(name string) bool {
	return name != "" &&
		len(name) <= 200 &&
		name == strings.TrimSpace(name) &&
		!strings.ContainsAny(name, `/\"`+"\r\n\t") &&
		strings.HasSuffix(strings.ToLower(name), ".zip")
}

func validAction(action runtimeapi.Action) bool {
	if action.Kind != runtimeapi.ActionUpdateMihomo &&
		action.Kind != runtimeapi.ActionUpdateProduct &&
		action.Params.Trust != "" {
		return false
	}
	if action.Kind != runtimeapi.ActionEnableTUN &&
		action.Kind != runtimeapi.ActionEnableGateway &&
		action.Kind != runtimeapi.ActionUpdateMihomo &&
		action.Kind != runtimeapi.ActionUpdateProduct &&
		action.Params.PlanID != "" {
		return false
	}
	switch action.Kind {
	case runtimeapi.ActionApplyImportedConfig:
		return validContentID(action.Params.ContentID) &&
			action.Params.SourceID == "" &&
			action.Params.SourceName == "" &&
			action.Params.Route == "" &&
			!action.Params.UseCached &&
			!action.Params.Confirm &&
			action.Params.ResourceKind == "" &&
			action.Params.ResourceName == ""
	case runtimeapi.ActionStartProxy, runtimeapi.ActionStopProxy:
		return action.Params.ContentID == "" &&
			action.Params.SourceID == "" &&
			action.Params.SourceName == "" &&
			action.Params.Route == "" &&
			!action.Params.UseCached &&
			!action.Params.Confirm &&
			action.Params.ResourceKind == "" &&
			action.Params.ResourceName == "" &&
			action.Params.PlanID == ""
	case runtimeapi.ActionEnableTUN, runtimeapi.ActionEnableGateway:
		return validPlanID(action.Params.PlanID) &&
			action.Params.ContentID == "" &&
			action.Params.SourceID == "" &&
			action.Params.SourceName == "" &&
			action.Params.Route == "" &&
			!action.Params.UseCached &&
			!action.Params.Confirm &&
			action.Params.ResourceKind == "" &&
			action.Params.ResourceName == ""
	case runtimeapi.ActionDisableTUN, runtimeapi.ActionDisableGateway:
		return action.Params.ContentID == "" &&
			action.Params.SourceID == "" &&
			action.Params.SourceName == "" &&
			action.Params.Route == "" &&
			!action.Params.UseCached &&
			!action.Params.Confirm &&
			action.Params.ResourceKind == "" &&
			action.Params.ResourceName == "" &&
			action.Params.PlanID == ""
	case runtimeapi.ActionUpdateMihomo:
		return validPlanID(action.Params.PlanID) &&
			action.Params.Confirm &&
			(action.Params.Trust == runtimeapi.MihomoUpdateTrustTUF ||
				action.Params.Trust == runtimeapi.MihomoUpdateTrustUpstreamOnly) &&
			action.Params.ContentID == "" &&
			action.Params.SourceID == "" &&
			action.Params.SourceName == "" &&
			action.Params.Route == "" &&
			!action.Params.UseCached &&
			action.Params.ResourceKind == "" &&
			action.Params.ResourceName == ""
	case runtimeapi.ActionRollbackMihomo:
		return action.Params.Confirm &&
			action.Params.PlanID == "" &&
			action.Params.Trust == "" &&
			action.Params.ContentID == "" &&
			action.Params.SourceID == "" &&
			action.Params.SourceName == "" &&
			action.Params.Route == "" &&
			!action.Params.UseCached &&
			action.Params.ResourceKind == "" &&
			action.Params.ResourceName == ""
	case runtimeapi.ActionCheckProduct:
		return action.Params == (runtimeapi.ActionParams{})
	case runtimeapi.ActionUpdateProduct:
		return validProductPlanID(action.Params.PlanID) &&
			action.Params.Confirm &&
			action.Params.Trust == runtimeapi.ProductUpdateTrustTUF &&
			action.Params.ContentID == "" &&
			action.Params.SourceID == "" &&
			action.Params.SourceName == "" &&
			action.Params.Route == "" &&
			!action.Params.UseCached &&
			action.Params.ResourceKind == "" &&
			action.Params.ResourceName == ""
	case runtimeapi.ActionRollbackProduct:
		return action.Params.Confirm &&
			action.Params.PlanID == "" &&
			action.Params.Trust == "" &&
			action.Params.ContentID == "" &&
			action.Params.SourceID == "" &&
			action.Params.SourceName == "" &&
			action.Params.Route == "" &&
			!action.Params.UseCached &&
			action.Params.ResourceKind == "" &&
			action.Params.ResourceName == ""
	case runtimeapi.ActionRestoreBackup:
		return validContentID(action.Params.ContentID) &&
			action.Params.Confirm &&
			action.Params.PlanID == "" &&
			action.Params.Trust == "" &&
			action.Params.SourceID == "" &&
			action.Params.SourceName == "" &&
			action.Params.Route == "" &&
			!action.Params.UseCached &&
			action.Params.ResourceKind == "" &&
			action.Params.ResourceName == ""
	case runtimeapi.ActionAddRemoteSource:
		return validContentID(action.Params.ContentID) &&
			action.Params.SourceID == "" &&
			action.Params.SourceName == "" &&
			action.Params.Route == "" &&
			!action.Params.UseCached &&
			!action.Params.Confirm &&
			action.Params.ResourceKind == "" &&
			action.Params.ResourceName == ""
	case runtimeapi.ActionAddImportedSource:
		return validContentID(action.Params.ContentID) &&
			action.Params.SourceID == "" &&
			validDisplayName(action.Params.SourceName) &&
			action.Params.Route == "" &&
			!action.Params.UseCached &&
			!action.Params.Confirm &&
			action.Params.ResourceKind == "" &&
			action.Params.ResourceName == ""
	case runtimeapi.ActionRefreshSource:
		return validSourceID(action.Params.SourceID) &&
			action.Params.ContentID == "" &&
			action.Params.SourceName == "" &&
			!action.Params.UseCached &&
			!action.Params.Confirm &&
			action.Params.ResourceKind == "" &&
			action.Params.ResourceName == "" &&
			(action.Params.Route == "" ||
				action.Params.Route == runtimeapi.SourceRouteDirect ||
				action.Params.Route == runtimeapi.SourceRouteMihomo)
	case runtimeapi.ActionApplySource:
		return validSourceID(action.Params.SourceID) &&
			action.Params.ContentID == "" &&
			action.Params.SourceName == "" &&
			action.Params.Route == "" &&
			!action.Params.UseCached &&
			!action.Params.Confirm &&
			action.Params.ResourceKind == "" &&
			action.Params.ResourceName == ""
	case runtimeapi.ActionSwitchSource:
		return validSourceID(action.Params.SourceID) &&
			action.Params.ContentID == "" &&
			action.Params.SourceName == "" &&
			!action.Params.Confirm &&
			action.Params.ResourceKind == "" &&
			action.Params.ResourceName == "" &&
			(action.Params.Route == "" ||
				action.Params.Route == runtimeapi.SourceRouteDirect ||
				action.Params.Route == runtimeapi.SourceRouteMihomo)
	case runtimeapi.ActionDeleteSource:
		return validSourceID(action.Params.SourceID) &&
			action.Params.ContentID == "" &&
			action.Params.SourceName == "" &&
			action.Params.Route == "" &&
			!action.Params.UseCached &&
			action.Params.ResourceKind == "" &&
			action.Params.ResourceName == ""
	case runtimeapi.ActionAddManagedResource:
		return validContentID(action.Params.ContentID) &&
			action.Params.SourceID == "" &&
			action.Params.SourceName == "" &&
			action.Params.Route == "" &&
			!action.Params.UseCached &&
			!action.Params.Confirm &&
			validResourceKind(action.Params.ResourceKind) &&
			validResourceName(action.Params.ResourceName)
	case runtimeapi.ActionSetAdvancedOverride:
		return validContentID(action.Params.ContentID) &&
			action.Params.SourceID == "" &&
			action.Params.SourceName == "" &&
			action.Params.Route == "" &&
			!action.Params.UseCached &&
			!action.Params.Confirm &&
			action.Params.ResourceKind == "" &&
			action.Params.ResourceName == ""
	default:
		return false
	}
}

func validResourceKind(kind string) bool {
	switch kind {
	case runtimeapi.ResourceKindProxyProvider,
		runtimeapi.ResourceKindRuleProvider,
		runtimeapi.ResourceKindCertificate,
		runtimeapi.ResourceKindPrivateKey:
		return true
	default:
		return false
	}
}

func validResourceName(name string) bool {
	return len(name) <= 128 &&
		validIdentifier(name, 128) &&
		!strings.HasPrefix(name, ".")
}

func validDisplayName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, character := range name {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func validSourceID(id string) bool {
	suffix, ok := strings.CutPrefix(id, "src_")
	if !ok || len(suffix) != 32 {
		return false
	}
	_, err := hex.DecodeString(suffix)
	return err == nil
}

func validContentID(id string) bool {
	suffix, ok := strings.CutPrefix(id, "content_")
	if !ok || len(suffix) != 32 {
		return false
	}
	_, err := hex.DecodeString(suffix)
	return err == nil
}

func validPlanID(id string) bool {
	suffix, ok := strings.CutPrefix(id, "plan_")
	if !ok || len(suffix) != 32 {
		return false
	}
	_, err := hex.DecodeString(suffix)
	return err == nil
}

func validProductPlanID(id string) bool {
	suffix, ok := strings.CutPrefix(id, "product_plan_")
	if !ok || len(suffix) != 32 {
		return false
	}
	_, err := hex.DecodeString(suffix)
	return err == nil
}

func (s *Server) writeError(
	writer http.ResponseWriter,
	request *http.Request,
	status int,
	code string,
	message string,
	retryable bool,
) {
	s.writeErrorEnvelope(writer, request, status, runtimeapi.ErrorEnvelope{
		ProtocolVersion: runtimeapi.ProtocolVersion,
		Error: runtimeapi.ProtocolError{
			Code:      code,
			Message:   message,
			Retryable: retryable,
		},
	})
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
