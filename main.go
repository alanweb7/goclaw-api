package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
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

func main() {
	cfg := config{
		ListenAddr: getenv("PORT", "8080"),
		WSURL:      getenv("GOCLAW_WS_URL", "wss://ws.geoclaw.pullse.ia.br/ws"),
		Token:      os.Getenv("GOCLAW_TOKEN"),
	}

	if cfg.Token == "" {
		log.Fatal("missing GOCLAW_TOKEN")
	}

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

	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if strings.TrimSpace(req.UserID) == "" {
		writeError(w, http.StatusBadRequest, "user_id is required")
		return
	}
	if strings.TrimSpace(req.AgentID) == "" {
		writeError(w, http.StatusBadRequest, "agent_id is required")
		return
	}
	if strings.TrimSpace(req.SessionKey) == "" {
		writeError(w, http.StatusBadRequest, "session_key is required")
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		writeError(w, http.StatusBadRequest, "message is required")
		return
	}
	if req.Locale == "" {
		req.Locale = "pt-BR"
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

func getenv(k, fallback string) string {
	v := os.Getenv(k)
	if v == "" {
		return fallback
	}
	return v
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
