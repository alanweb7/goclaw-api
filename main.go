package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

type config struct {
	ListenAddr string
	WSURL      string
	Token      string
}

type requestFrame struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Method string `json:"method,omitempty"`
	Params any    `json:"params,omitempty"`
}

type responseFrame struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	OK      bool            `json:"ok"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   *frameError     `json:"error,omitempty"`
}

type eventFrame struct {
	Type    string          `json:"type"`
	Event   string          `json:"event"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type frameError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type chatRequest struct {
	UserID     string `json:"user_id"`
	AgentID    string `json:"agent_id"`
	SessionKey string `json:"session_key"`
	Message    string `json:"message"`
	Stream     bool   `json:"stream"`
	Locale     string `json:"locale"`
}

type chatResponse struct {
	SessionKey string `json:"session_key"`
	Content    string `json:"content"`
}

type chatSendResponse struct {
	Content   string `json:"content"`
	Cancelled bool   `json:"cancelled"`
	Injected  bool   `json:"injected"`
}

type createJobRequest struct {
	UserID          string            `json:"user_id"`
	AgentID         string            `json:"agent_id"`
	SessionKey      string            `json:"session_key"`
	Message         string            `json:"message"`
	Stream          bool              `json:"stream"`
	Locale          string            `json:"locale"`
	CallbackURL     string            `json:"callback_url,omitempty"`
	CallbackHeaders map[string]string `json:"callback_headers,omitempty"`
}

type jobResponse struct {
	ID              string            `json:"id"`
	Status          string            `json:"status"`
	UserID          string            `json:"user_id"`
	AgentID         string            `json:"agent_id"`
	SessionKey      string            `json:"session_key"`
	Message         string            `json:"message"`
	Content         string            `json:"content,omitempty"`
	Error           string            `json:"error,omitempty"`
	CallbackURL     string            `json:"callback_url,omitempty"`
	CallbackHeaders map[string]string `json:"callback_headers,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

type jobStore struct {
	mu   sync.RWMutex
	jobs map[string]*jobResponse
}

var jobCounter uint64

const (
	jobStatusQueued    = "queued"
	jobStatusRunning   = "running"
	jobStatusSucceeded = "succeeded"
	jobStatusFailed    = "failed"
)

func main() {
	token, err := loadToken()
	if err != nil {
		log.Fatal(err)
	}

	cfg := config{
		ListenAddr: getenv("PORT", "8080"),
		WSURL:      getenv("GOCLAW_WS_URL", "wss://ws.geoclaw.pullse.ia.br/ws"),
		Token:      token,
	}

	if cfg.Token == "" {
		log.Fatal("missing GOCLAW_TOKEN")
	}

	jobs := &jobStore{jobs: make(map[string]*jobResponse)}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/v1/chat", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		handleChat(w, r, cfg)
	})
	mux.HandleFunc("/v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		handleCreateJob(w, r, cfg, jobs)
	})
	mux.HandleFunc("/v1/jobs/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		handleGetJob(w, r, jobs)
	})

	srv := &http.Server{
		Addr:              ":" + strings.TrimPrefix(cfg.ListenAddr, ":"),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("goclaw-api listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func handleChat(w http.ResponseWriter, r *http.Request, cfg config) {
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	req, err := decodeAndValidateChatRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	content, err := callGoClaw(ctx, cfg, req)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, chatResponse{
		SessionKey: req.SessionKey,
		Content:    content,
	})
}

func decodeAndValidateChatRequest(r *http.Request) (chatRequest, error) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return chatRequest{}, errors.New("invalid JSON body")
	}

	if strings.TrimSpace(req.UserID) == "" {
		return chatRequest{}, errors.New("user_id is required")
	}
	if strings.TrimSpace(req.AgentID) == "" {
		return chatRequest{}, errors.New("agent_id is required")
	}
	if strings.TrimSpace(req.SessionKey) == "" {
		return chatRequest{}, errors.New("session_key is required")
	}
	if strings.TrimSpace(req.Message) == "" {
		return chatRequest{}, errors.New("message is required")
	}
	if req.Locale == "" {
		req.Locale = "pt-BR"
	}
	return req, nil
}

func handleCreateJob(w http.ResponseWriter, r *http.Request, cfg config, jobs *jobStore) {
	var in createJobRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	req := chatRequest{
		UserID:     in.UserID,
		AgentID:    in.AgentID,
		SessionKey: in.SessionKey,
		Message:    in.Message,
		Stream:     in.Stream,
		Locale:     in.Locale,
	}
	if err := validateCallbackURL(in.CallbackURL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Locale == "" {
		req.Locale = "pt-BR"
	}
	if strings.TrimSpace(req.UserID) == "" || strings.TrimSpace(req.AgentID) == "" || strings.TrimSpace(req.SessionKey) == "" || strings.TrimSpace(req.Message) == "" {
		writeError(w, http.StatusBadRequest, "user_id, agent_id, session_key and message are required")
		return
	}

	jobID := newJobID()
	now := time.Now().UTC()
	job := &jobResponse{
		ID:              jobID,
		Status:          jobStatusQueued,
		UserID:          req.UserID,
		AgentID:         req.AgentID,
		SessionKey:      req.SessionKey,
		Message:         req.Message,
		CallbackURL:     in.CallbackURL,
		CallbackHeaders: in.CallbackHeaders,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	jobs.put(job)

	go processJob(cfg, jobs, jobID, req)

	writeJSON(w, http.StatusAccepted, map[string]string{
		"id":     jobID,
		"status": jobStatusQueued,
	})
}

func handleGetJob(w http.ResponseWriter, r *http.Request, jobs *jobStore) {
	id := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/v1/jobs/"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "job id is required")
		return
	}
	job, ok := jobs.get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func processJob(cfg config, jobs *jobStore, jobID string, req chatRequest) {
	jobs.update(jobID, func(j *jobResponse) {
		j.Status = jobStatusRunning
		j.UpdatedAt = time.Now().UTC()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	content, err := callGoClaw(ctx, cfg, req)
	if err != nil {
		jobs.update(jobID, func(j *jobResponse) {
			j.Status = jobStatusFailed
			j.Error = err.Error()
			j.UpdatedAt = time.Now().UTC()
		})
		publishCallback(jobs, jobID)
		return
	}

	jobs.update(jobID, func(j *jobResponse) {
		j.Status = jobStatusSucceeded
		j.Content = content
		j.UpdatedAt = time.Now().UTC()
	})
	publishCallback(jobs, jobID)
}

func validateCallbackURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("callback_url is invalid")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("callback_url must start with http:// or https://")
	}
	if u.Host == "" {
		return errors.New("callback_url host is required")
	}
	return nil
}

func publishCallback(jobs *jobStore, jobID string) {
	job, ok := jobs.get(jobID)
	if !ok || strings.TrimSpace(job.CallbackURL) == "" {
		return
	}
	body, err := json.Marshal(job)
	if err != nil {
		return
	}

	client := &http.Client{Timeout: 10 * time.Second}
	for i := 0; i < 3; i++ {
		req, err := http.NewRequest(http.MethodPost, job.CallbackURL, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		for k, v := range job.CallbackHeaders {
			if strings.TrimSpace(k) != "" {
				req.Header.Set(k, v)
			}
		}

		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return
			}
		}
		time.Sleep(time.Duration(i+1) * time.Second)
	}
}

func callGoClaw(ctx context.Context, cfg config, req chatRequest) (string, error) {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, cfg.WSURL, nil)
	if err != nil {
		return "", fmt.Errorf("ws dial failed: %w", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))

	if err := writeFrame(conn, requestFrame{
		Type:   "req",
		ID:     "1",
		Method: "connect",
		Params: map[string]any{
			"token":   cfg.Token,
			"user_id": req.UserID,
			"locale":  req.Locale,
		},
	}); err != nil {
		return "", fmt.Errorf("connect write failed: %w", err)
	}

	if err := waitConnectOK(ctx, conn, "1"); err != nil {
		return "", err
	}

	if err := writeFrame(conn, requestFrame{
		Type:   "req",
		ID:     "2",
		Method: "chat.send",
		Params: map[string]any{
			"agentId":    req.AgentID,
			"sessionKey": req.SessionKey,
			"message":    req.Message,
			"stream":     req.Stream,
		},
	}); err != nil {
		return "", fmt.Errorf("chat.send write failed: %w", err)
	}

	return waitChatResult(ctx, conn, "2", req.Stream)
}

func waitConnectOK(ctx context.Context, conn *websocket.Conn, expectedID string) error {
	for {
		select {
		case <-ctx.Done():
			return errors.New("timeout waiting connect response")
		default:
		}
		_, data, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read connect response failed: %w", err)
		}

		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(data, &envelope); err != nil {
			continue
		}

		var frameType string
		if b, ok := envelope["type"]; ok {
			_ = json.Unmarshal(b, &frameType)
		}
		if frameType != "res" {
			continue
		}

		var res responseFrame
		if err := json.Unmarshal(data, &res); err != nil {
			continue
		}
		if res.ID != expectedID {
			continue
		}
		if !res.OK {
			if res.Error != nil && res.Error.Message != "" {
				return fmt.Errorf("connect rejected: %s", res.Error.Message)
			}
			return errors.New("connect rejected")
		}
		return nil
	}
}

func waitChatResult(ctx context.Context, conn *websocket.Conn, expectedID string, stream bool) (string, error) {
	var chunks []string
	for {
		select {
		case <-ctx.Done():
			return "", errors.New("timeout waiting chat response")
		default:
		}

		_, data, err := conn.ReadMessage()
		if err != nil {
			return "", fmt.Errorf("read chat response failed: %w", err)
		}

		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(data, &envelope); err != nil {
			continue
		}
		var frameType string
		if b, ok := envelope["type"]; ok {
			_ = json.Unmarshal(b, &frameType)
		}

		if frameType == "event" && stream {
			var ev eventFrame
			if err := json.Unmarshal(data, &ev); err != nil {
				continue
			}
			if ev.Event == "chat" {
				var payload struct {
					Chunk string `json:"chunk"`
				}
				if json.Unmarshal(ev.Payload, &payload) == nil && payload.Chunk != "" {
					chunks = append(chunks, payload.Chunk)
				}
			}
			continue
		}

		if frameType != "res" {
			continue
		}

		var res responseFrame
		if err := json.Unmarshal(data, &res); err != nil {
			continue
		}
		if res.ID != expectedID {
			continue
		}
		if !res.OK {
			if res.Error != nil && res.Error.Message != "" {
				return "", fmt.Errorf("chat error: %s", res.Error.Message)
			}
			return "", errors.New("chat failed")
		}

		var out chatSendResponse
		if len(res.Payload) > 0 {
			_ = json.Unmarshal(res.Payload, &out)
		}

		if out.Content != "" {
			return out.Content, nil
		}
		if stream && len(chunks) > 0 {
			return strings.Join(chunks, ""), nil
		}
		if out.Cancelled {
			return "", errors.New("chat cancelled")
		}
		if out.Injected {
			return "", nil
		}
		return "", nil
	}
}

func writeFrame(conn *websocket.Conn, v any) error {
	return conn.WriteJSON(v)
}

func newJobID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	n := atomic.AddUint64(&jobCounter, 1)
	return "job_" + strconv.FormatInt(time.Now().Unix(), 36) + "_" + fmt.Sprintf("%x", b[:]) + "_" + strconv.FormatUint(n, 36)
}

func (s *jobStore) put(job *jobResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[job.ID] = job
}

func (s *jobStore) get(id string) (jobResponse, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	job, ok := s.jobs[id]
	if !ok {
		return jobResponse{}, false
	}
	out := *job
	return out, true
}

func (s *jobStore) update(id string, fn func(*jobResponse)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		return
	}
	fn(job)
}

func getenv(k, fallback string) string {
	v := os.Getenv(k)
	if v == "" {
		return fallback
	}
	return v
}

func loadToken() (string, error) {
	// Priority: GOCLAW_TOKEN (env) > GOCLAW_TOKEN_FILE (Docker secret).
	if token := strings.TrimSpace(os.Getenv("GOCLAW_TOKEN")); token != "" {
		return token, nil
	}
	if p := strings.TrimSpace(os.Getenv("GOCLAW_TOKEN_FILE")); p != "" {
		data, err := os.ReadFile(filepath.Clean(p))
		if err != nil {
			return "", fmt.Errorf("failed reading GOCLAW_TOKEN_FILE: %w", err)
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			return "", errors.New("GOCLAW_TOKEN_FILE is empty")
		}
		return token, nil
	}
	return "", errors.New("missing GOCLAW_TOKEN or GOCLAW_TOKEN_FILE")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
