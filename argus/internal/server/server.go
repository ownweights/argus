package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/ace-foundry/argus-testing/argus/internal/domain"
	"github.com/ace-foundry/argus-testing/argus/internal/policy"
	"github.com/ace-foundry/argus-testing/argus/internal/store"
	"github.com/coder/websocket"
)

const defaultModel = "gemini-2.5-flash"

type Runner interface {
	Run(context.Context, string, domain.RunAuthorization)
}

type noOpRunner struct{}

func (noOpRunner) Run(context.Context, string, domain.RunAuthorization) {}

type Options struct {
	StaticDir        string
	ScreenshotDir    string
	GeminiConfigured bool
	OpenAIConfigured bool
	KimiConfigured   bool
	Model            string
}

type validationError struct {
	Loc  []any  `json:"loc"`
	Type string `json:"type"`
	Msg  string `json:"msg"`
}

type Server struct {
	store  *store.Store
	runner Runner
	hub    *eventHub

	staticDir     string
	screenshotDir string
	settings      domain.SettingsResponse

	tasksMu sync.Mutex
	tasks   map[string]context.CancelFunc
	closing bool
	tasksWG sync.WaitGroup
}

type runAdmission struct {
	server *Server
	once   sync.Once
}

func (a *runAdmission) done() { a.once.Do(a.server.tasksWG.Done) }

func New(runStore *store.Store, runner Runner, options Options) (*Server, error) {
	if runStore == nil {
		return nil, errors.New("store is required")
	}
	if runner == nil {
		runner = noOpRunner{}
	}
	if err := runStore.Initialize(); err != nil {
		return nil, err
	}
	if options.Model == "" {
		options.Model = defaultModel
	}
	if !options.GeminiConfigured {
		options.GeminiConfigured = os.Getenv("GEMINI_API_KEY") != ""
	}
	if !options.OpenAIConfigured {
		options.OpenAIConfigured = os.Getenv("OPENAI_API_KEY") != ""
	}
	if !options.KimiConfigured {
		options.KimiConfigured = os.Getenv("KIMI_API_KEY") != ""
	}
	providers := []domain.ProviderInfo{
		{ID: domain.ProviderGemini, Available: options.GeminiConfigured, Default: true},
		{ID: domain.ProviderGPT, Available: options.OpenAIConfigured},
		{ID: domain.ProviderKimi, Available: options.KimiConfigured},
	}
	server := &Server{store: runStore, runner: runner, hub: newEventHub(), staticDir: options.StaticDir, screenshotDir: options.ScreenshotDir, settings: domain.SettingsResponse{GeminiConfigured: options.GeminiConfigured, Model: options.Model, Providers: providers}, tasks: map[string]context.CancelFunc{}}
	events, err := runStore.ReconcileInterrupted()
	if err != nil {
		return nil, err
	}
	for _, event := range events {
		server.Publish(event)
	}
	return server, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
		s.serveAPI(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/ws/runs/") {
		s.serveEvents(w, r, strings.TrimPrefix(r.URL.Path, "/ws/runs/"))
		return
	}
	if r.URL.Path == "/screenshots" || strings.HasPrefix(r.URL.Path, "/screenshots/") {
		s.serveScreenshot(w, r, strings.TrimPrefix(r.URL.Path, "/screenshots/"))
		return
	}
	s.serveStatic(w, r)
}

func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == domain.RunsEndpoint && r.Method == http.MethodPost:
		s.createRun(w, r)
	case r.URL.Path == domain.RunsEndpoint && r.Method == http.MethodGet:
		s.listRuns(w, r)
	case r.URL.Path == domain.SettingsEndpoint && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.settings)
	case strings.HasPrefix(r.URL.Path, "/api/runs/"):
		s.runRoute(w, r, strings.TrimPrefix(r.URL.Path, "/api/runs/"))
	default:
		writeError(w, http.StatusNotFound, "Not found")
	}
}

func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	request, validation := decodeCreateRequest(io.LimitReader(r.Body, 1<<20))
	if len(validation) > 0 {
		writeValidation(w, validation)
		return
	}
	if !s.providerAvailable(request.Provider) {
		writeValidation(w, []validationError{{Loc: []any{"body", "provider"}, Type: "value_error", Msg: "Provider is not configured"}})
		return
	}
	admission := s.admitRun()
	if admission == nil {
		writeError(w, http.StatusServiceUnavailable, "Server is shutting down")
		return
	}
	started := false
	defer func() {
		if !started {
			admission.done()
		}
	}()

	authorization := domain.RunAuthorization{}
	if request.Authorization != nil {
		authorization = *request.Authorization
	}
	runPolicy := domain.RunPolicy{
		AllowMutations:   authorization.AllowMutations,
		AllowDestructive: authorization.AllowDestructive,
		AllowedOrigins:   append([]string(nil), authorization.AllowedOrigins...),
	}
	run, err := s.store.CreateRunWithProvider(SanitizeURL(request.URL), request.Instructions, request.Provider, runPolicy)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Internal server error")
		return
	}
	event, err := s.store.AddEvent(run.ID, domain.EventRunQueued, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Internal server error")
		return
	}
	s.Publish(*event)
	if !s.start(run.ID, authorization, admission) {
		writeError(w, http.StatusServiceUnavailable, "Server is shutting down")
		return
	}
	started = true
	writeJSON(w, http.StatusCreated, run)
}

func (s *Server) providerAvailable(provider domain.ProviderID) bool {
	for _, configured := range s.settings.Providers {
		if configured.ID == provider {
			return configured.Available
		}
	}
	return false
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			writeValidation(w, []validationError{{Loc: []any{"query", "limit"}, Type: "int_parsing", Msg: "Input should be a valid integer, unable to parse string as an integer"}})
			return
		}
		limit = parsed
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 500 {
		limit = 500
	}
	runs, err := s.store.ListRuns(limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Internal server error")
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func (s *Server) runRoute(w http.ResponseWriter, r *http.Request, path string) {
	if strings.HasSuffix(path, "/cancel") {
		id := strings.TrimSuffix(path, "/cancel")
		if id == "" || strings.Contains(id, "/") || r.Method != http.MethodPost {
			writeError(w, http.StatusNotFound, "Not found")
			return
		}
		s.cancelRun(w, id)
		return
	}
	if path == "" || strings.Contains(path, "/") || r.Method != http.MethodGet {
		writeError(w, http.StatusNotFound, "Not found")
		return
	}
	run, err := s.store.GetRun(path, true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Internal server error")
		return
	}
	if run == nil {
		writeError(w, http.StatusNotFound, "Run not found")
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) cancelRun(w http.ResponseWriter, id string) {
	run, err := s.store.GetRun(id, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Internal server error")
		return
	}
	if run == nil {
		writeError(w, http.StatusNotFound, "Run not found")
		return
	}
	event, err := s.store.Transition(id, []domain.RunStatus{domain.RunStatusQueued, domain.RunStatusRunning}, domain.RunStatusCancelled, domain.EventRunCancelled, nil, nil, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Internal server error")
		return
	}
	if event != nil {
		s.Publish(*event)
		s.cancelTask(id)
	}
	run, err = s.store.GetRun(id, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Internal server error")
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) admitRun() *runAdmission {
	s.tasksMu.Lock()
	defer s.tasksMu.Unlock()
	if s.closing {
		return nil
	}
	s.tasksWG.Add(1)
	return &runAdmission{server: s}
}

func (s *Server) start(id string, authorization domain.RunAuthorization, admission *runAdmission) bool {
	ctx, cancel := context.WithCancel(context.Background())
	s.tasksMu.Lock()
	if s.closing && admission == nil {
		s.tasksMu.Unlock()
		cancel()
		return false
	}
	if admission == nil {
		s.tasksWG.Add(1)
	}
	s.tasks[id] = cancel
	s.tasksMu.Unlock()
	go func() {
		if admission == nil {
			defer s.tasksWG.Done()
		} else {
			defer admission.done()
		}
		s.runner.Run(ctx, id, authorization)
		s.tasksMu.Lock()
		delete(s.tasks, id)
		s.tasksMu.Unlock()
	}()
	return true
}

// Close rejects new runs and stops runs that are already active.
func (s *Server) Close() {
	s.tasksMu.Lock()
	s.closing = true
	cancels := make([]context.CancelFunc, 0, len(s.tasks))
	for _, cancel := range s.tasks {
		cancels = append(cancels, cancel)
	}
	s.tasksMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// Wait waits for runs stopped by Close.
func (s *Server) Wait() { s.tasksWG.Wait() }

func (s *Server) cancelTask(id string) {
	s.tasksMu.Lock()
	cancel := s.tasks[id]
	s.tasksMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Server) Publish(event domain.RunEvent) { s.hub.publish(event) }

func (s *Server) serveEvents(w http.ResponseWriter, r *http.Request, id string) {
	if id == "" || strings.Contains(id, "/") {
		writeError(w, http.StatusNotFound, "Not found")
		return
	}
	run, err := s.store.GetRun(id, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Internal server error")
		return
	}
	if run == nil {
		// The run lookup intentionally occurs before upgrading; a close frame still requires an HTTP upgrade.
		conn, err := websocket.Accept(w, r, nil)
		if err == nil {
			_ = conn.Close(websocket.StatusCode(4404), "Run not found")
		}
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	subscriber, unsubscribe := s.hub.subscribe(id)
	defer unsubscribe()
	lastID := int64(0)
	if !s.replayEvents(r.Context(), conn, id, &lastID) {
		return
	}
	current, err := s.store.GetRun(id, false)
	if err != nil {
		return
	}
	if current != nil && isTerminal(current.Status) {
		_ = s.replayEvents(r.Context(), conn, id, &lastID)
		return
	}
	disconnected := make(chan struct{})
	go func() { _, _, _ = conn.Read(r.Context()); close(disconnected) }()
	for {
		select {
		case <-disconnected:
			return
		case <-subscriber.overflow:
			return
		case event := <-subscriber.events:
			if event.ID <= lastID {
				continue
			}
			if err := writeEvent(r.Context(), conn, event); err != nil {
				return
			}
			lastID = event.ID
			if isTerminalEvent(event.Type) {
				return
			}
		}
	}
}

func (s *Server) replayEvents(ctx context.Context, conn *websocket.Conn, id string, lastID *int64) bool {
	events, err := s.store.EventsAfter(id, *lastID)
	if err != nil {
		return false
	}
	for _, event := range events {
		if event.ID <= *lastID {
			continue
		}
		if err := writeEvent(ctx, conn, event); err != nil {
			return false
		}
		*lastID = event.ID
		if isTerminalEvent(event.Type) {
			return false
		}
	}
	return true
}

func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	if s.staticDir == "" || r.Method != http.MethodGet {
		writeError(w, http.StatusNotFound, "Not found")
		return
	}
	if candidate, ok := containedFile(s.staticDir, r.URL.Path); ok {
		http.ServeFile(w, r, candidate)
		return
	}
	if index, ok := containedFile(s.staticDir, "index.html"); ok {
		http.ServeFile(w, r, index)
		return
	}
	writeError(w, http.StatusNotFound, "Not found")
}

func (s *Server) serveScreenshot(w http.ResponseWriter, r *http.Request, path string) {
	if r.Method != http.MethodGet || s.screenshotDir == "" {
		writeError(w, http.StatusNotFound, "Not found")
		return
	}
	candidate, ok := containedFile(s.screenshotDir, path)
	if !ok {
		writeError(w, http.StatusNotFound, "Not found")
		return
	}
	http.ServeFile(w, r, candidate)
}

func containedFile(root, path string) (string, bool) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}
	path = strings.TrimPrefix(filepath.Clean("/"+path), "/")
	if path == "." {
		return "", false
	}
	candidate, err := filepath.EvalSymlinks(filepath.Join(resolvedRoot, path))
	if err != nil || !within(resolvedRoot, candidate) {
		return "", false
	}
	info, err := os.Stat(candidate)
	return candidate, err == nil && !info.IsDir()
}

func ValidateURL(value string) error {
	parsed, _, _, err := parseURLCompatible(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return errors.New("URL must use HTTP(S) and include a hostname")
	}
	if parsed.User != nil {
		return errors.New("URLs containing credentials are not allowed")
	}
	for _, pair := range strings.Split(parsed.RawQuery, "&") {
		key, _, _ := strings.Cut(pair, "=")
		key = queryUnescapeCompatible(key)
		if sensitiveKey(key) {
			return errors.New("URLs containing sensitive query parameters are not allowed")
		}
	}
	return nil
}

func parseURLCompatible(value string) (*url.URL, string, string, error) {
	parsed, err := url.Parse(value)
	if err == nil {
		return parsed, "", "", nil
	}
	parsed, err = url.Parse(escapeInvalidPercents(value))
	if err != nil {
		return nil, "", "", err
	}
	return parsed, rawPath(value), rawFragment(value), nil
}

func escapeInvalidPercents(value string) string {
	var escaped strings.Builder
	escaped.Grow(len(value))
	for index := 0; index < len(value); index++ {
		if value[index] != '%' || index+2 < len(value) && isHex(value[index+1]) && isHex(value[index+2]) {
			escaped.WriteByte(value[index])
			continue
		}
		escaped.WriteString("%25")
	}
	return escaped.String()
}

func isHex(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
}

func rawPath(value string) string {
	authority := strings.Index(value, "://")
	if authority < 0 {
		return ""
	}
	path := value[authority+3:]
	start := strings.IndexAny(path, "/?#")
	if start < 0 || path[start] != '/' {
		return ""
	}
	path = path[start:]
	if end := strings.IndexAny(path, "?#"); end >= 0 {
		return path[:end]
	}
	return path
}

func rawFragment(value string) string {
	fragment := strings.IndexByte(value, '#')
	if fragment < 0 {
		return ""
	}
	return value[fragment+1:]
}

func queryUnescapeCompatible(value string) string {
	decoded, _ := url.QueryUnescape(escapeInvalidPercents(value))
	return decoded
}

func SanitizeURL(value string) string {
	parsed, rawPath, rawFragment, err := parseURLCompatible(value)
	if err != nil || parsed.Hostname() == "" {
		return "[invalid URL]"
	}
	host := strings.ToLower(parsed.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := parsed.Port(); port != "" {
		parsedPort, err := strconv.Atoi(port)
		if err != nil || parsedPort < 0 || parsedPort > 65535 {
			return "[invalid URL]"
		}
		host += ":" + strconv.Itoa(parsedPort)
	}
	query, ok := sanitizedQuery(parsed.RawQuery)
	if !ok {
		return "[invalid URL]"
	}
	path := parsed.EscapedPath()
	if rawPath != "" {
		path = rawPath
	}
	result := parsed.Scheme + "://" + host + path
	if query != "" {
		result += "?" + query
	}
	fragment := parsed.EscapedFragment()
	if rawFragment != "" {
		fragment = rawFragment
	}
	if fragment != "" {
		result += "#" + fragment
	}
	return result
}

func sanitizedQuery(raw string) (string, bool) {
	if raw == "" {
		return "", true
	}
	pairs := make([]string, 0)
	for _, pair := range strings.Split(raw, "&") {
		if pair == "" {
			continue
		}
		key, value, _ := strings.Cut(pair, "=")
		key = queryUnescapeCompatible(key)
		value = queryUnescapeCompatible(value)
		if !sensitiveKey(key) {
			pairs = append(pairs, url.QueryEscape(key)+"="+url.QueryEscape(value))
		}
	}
	return strings.Join(pairs, "&"), true
}

func decodeCreateRequest(body io.Reader) (domain.CreateRequest, []validationError) {
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(body)
	if err := decoder.Decode(&fields); err != nil {
		return domain.CreateRequest{}, []validationError{{Loc: []any{"body"}, Type: "json_invalid", Msg: "JSON decode error"}}
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return domain.CreateRequest{}, []validationError{{Loc: []any{"body"}, Type: "json_invalid", Msg: "JSON decode error"}}
	}
	request := domain.CreateRequest{Provider: domain.ProviderGemini}
	validation := make([]validationError, 0, 3)
	request.URL = requiredString(fields, "url", &validation)
	request.Instructions = requiredString(fields, "instructions", &validation)
	if raw, ok := fields["provider"]; ok && string(raw) != "null" {
		var provider string
		if err := json.Unmarshal(raw, &provider); err != nil {
			validation = append(validation, validationError{Loc: []any{"body", "provider"}, Type: "string_type", Msg: "Input should be a valid string"})
		} else {
			request.Provider = domain.ProviderID(provider)
			if !domain.ValidProvider(request.Provider) {
				validation = append(validation, validationError{Loc: []any{"body", "provider"}, Type: "value_error", Msg: "Provider must be gemini, gpt, or kimi"})
			}
		}
	}
	if raw, ok := fields["authorization"]; ok && string(raw) != "null" {
		var authorization domain.RunAuthorization
		if err := json.Unmarshal(raw, &authorization); err != nil {
			validation = append(validation, validationError{Loc: []any{"body", "authorization"}, Type: "object_type", Msg: "Input should be a valid object"})
		} else {
			request.Authorization = &authorization
		}
	}
	if len(validation) > 0 {
		return request, validation
	}
	if err := ValidateURL(request.URL); err != nil {
		validation = append(validation, validationError{Loc: []any{"body", "url"}, Type: "value_error", Msg: err.Error()})
	}
	length := utf8.RuneCountInString(request.Instructions)
	if length < 1 {
		validation = append(validation, validationError{Loc: []any{"body", "instructions"}, Type: "string_too_short", Msg: "String should have at least 1 character"})
	}
	if length > 10000 {
		validation = append(validation, validationError{Loc: []any{"body", "instructions"}, Type: "string_too_long", Msg: "String should have at most 10000 characters"})
	}
	authorization := domain.RunAuthorization{}
	if request.Authorization != nil {
		authorization = *request.Authorization
	}
	if len(authorization.AllowedOrigins) > 20 {
		validation = append(validation, validationError{Loc: []any{"body", "authorization", "allowed_origins"}, Type: "too_long", Msg: "At most 20 additional origins are allowed"})
	}
	if len(authorization.SecretBindings) > domain.MaxSecretBindings {
		validation = append(validation, validationError{Loc: []any{"body", "authorization", "secret_bindings"}, Type: "too_long", Msg: "At most 20 secret bindings are allowed"})
	} else if err := domain.ValidateSecretBindings(authorization.SecretBindings); err != nil {
		validation = append(validation, validationError{Loc: []any{"body", "authorization", "secret_bindings"}, Type: "value_error", Msg: "Secret binding names must be identifiers and values must be non-empty and at most 4096 bytes"})
	}
	policyTarget := request.URL
	if parsed, _, _, err := parseURLCompatible(request.URL); err == nil {
		policyTarget = parsed.Scheme + "://" + parsed.Host
	}
	if _, err := policy.New(policyTarget, authorization); err != nil {
		validation = append(validation, validationError{Loc: []any{"body", "authorization", "allowed_origins"}, Type: "value_error", Msg: "Allowed origins must be HTTP(S) origins without credentials, paths, queries, or fragments"})
	}
	return request, validation
}

func requiredString(fields map[string]json.RawMessage, field string, validation *[]validationError) string {
	raw, ok := fields[field]
	if !ok {
		*validation = append(*validation, validationError{Loc: []any{"body", field}, Type: "missing", Msg: "Field required"})
		return ""
	}
	if string(raw) == "null" {
		*validation = append(*validation, validationError{Loc: []any{"body", field}, Type: "string_type", Msg: "Input should be a valid string"})
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		*validation = append(*validation, validationError{Loc: []any{"body", field}, Type: "string_type", Msg: "Input should be a valid string"})
	}
	return value
}

func sensitiveKey(value string) bool {
	var normalized strings.Builder
	for _, char := range strings.ToLower(value) {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
			normalized.WriteRune(char)
		}
	}
	key := normalized.String()
	sensitive := map[string]bool{"token": true, "accesstoken": true, "refreshtoken": true, "idtoken": true, "apikey": true, "key": true, "password": true, "passwd": true, "pwd": true, "secret": true, "clientsecret": true, "auth": true, "authorization": true, "credential": true, "credentials": true, "code": true}
	return sensitive[key] || strings.HasSuffix(key, "token") || strings.HasSuffix(key, "apikey") || strings.HasSuffix(key, "password") || strings.HasSuffix(key, "passwd") || strings.HasSuffix(key, "secret") || strings.HasSuffix(key, "auth") || strings.HasSuffix(key, "credential") || strings.HasSuffix(key, "code")
}

func writeEvent(ctx context.Context, conn *websocket.Conn, event domain.RunEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}
func isTerminal(status domain.RunStatus) bool {
	return status == domain.RunStatusPassed || status == domain.RunStatusFailed || status == domain.RunStatusCancelled
}
func isTerminalEvent(event domain.EventType) bool {
	return event == domain.EventRunCompleted || event == domain.EventRunFailed || event == domain.EventRunCancelled
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]string{"detail": detail})
}
func writeValidation(w http.ResponseWriter, detail []validationError) {
	writeJSON(w, http.StatusUnprocessableEntity, map[string][]validationError{"detail": detail})
}
func within(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

const eventQueueSize = 256

type subscription struct {
	events       chan domain.RunEvent
	overflow     chan struct{}
	done         chan struct{}
	once         sync.Once
	overflowOnce sync.Once
}

type eventHub struct {
	mu          sync.Mutex
	subscribers map[string]map[*subscription]struct{}
}

func newEventHub() *eventHub {
	return &eventHub{subscribers: map[string]map[*subscription]struct{}{}}
}
func (h *eventHub) publish(event domain.RunEvent) {
	h.mu.Lock()
	subscribers := make([]*subscription, 0, len(h.subscribers[event.RunID]))
	for subscriber := range h.subscribers[event.RunID] {
		subscribers = append(subscribers, subscriber)
	}
	h.mu.Unlock()
	for _, subscriber := range subscribers {
		select {
		case <-subscriber.done:
		case subscriber.events <- event:
		default:
			subscriber.overflowOnce.Do(func() { close(subscriber.overflow) })
		}
	}
}
func (h *eventHub) subscribe(id string) (*subscription, func()) {
	subscriber := &subscription{events: make(chan domain.RunEvent, eventQueueSize), overflow: make(chan struct{}), done: make(chan struct{})}
	h.mu.Lock()
	if h.subscribers[id] == nil {
		h.subscribers[id] = map[*subscription]struct{}{}
	}
	h.subscribers[id][subscriber] = struct{}{}
	h.mu.Unlock()
	return subscriber, func() {
		subscriber.once.Do(func() {
			h.mu.Lock()
			delete(h.subscribers[id], subscriber)
			if len(h.subscribers[id]) == 0 {
				delete(h.subscribers, id)
			}
			close(subscriber.done)
			h.mu.Unlock()
		})
	}
}
