package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/ybd156/fnos-cloudflared/internal/paths"
)

// sendJSON writes a JSON response. Responses are never cached: the UI polls
// them and a stale tunnel list is indistinguishable from a broken one.
func sendJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		// A payload that cannot be marshalled is a programming error; answer
		// with a valid error document rather than a truncated one.
		body = []byte(`{"error":"服务器内部错误"}`)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// sendError is the shape every API failure uses.
func sendError(w http.ResponseWriter, status int, message string) {
	sendJSON(w, status, map[string]string{"error": message})
}

// apiError carries an HTTP status alongside a message that is safe to show.
type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func badRequest(msg string) error { return &apiError{status: http.StatusBadRequest, msg: msg} }

// statusOf maps an error to an HTTP status, defaulting to 500.
func statusOf(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.status
	}
	return http.StatusInternalServerError
}

// readBodyRaw reads the request body, enforcing the size cap.
//
// An oversized body answers 413 rather than dropping the connection: a client
// that only sees ECONNRESET cannot tell "too large" from "server crashed".
func readBodyRaw(r *http.Request) ([]byte, error) {
	limited := io.LimitReader(r.Body, paths.MaxBodyBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, badRequest("读取请求体失败")
	}
	if len(raw) > paths.MaxBodyBytes {
		return nil, &apiError{status: http.StatusRequestEntityTooLarge, msg: "请求体过大"}
	}
	return raw, nil
}

// decodeObject turns a JSON object body into its raw fields.
func decodeObject(raw []byte) (map[string]json.RawMessage, error) {
	if len(raw) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, badRequest("请求体不是合法 JSON")
	}
	if body == nil {
		body = map[string]json.RawMessage{}
	}
	return body, nil
}

// readBody decodes a JSON object body.
func readBody(r *http.Request) (map[string]json.RawMessage, error) {
	raw, err := readBodyRaw(r)
	if err != nil {
		return nil, err
	}
	return decodeObject(raw)
}

// bodyString reads one string field from a decoded body, tolerating absence.
func bodyString(body map[string]json.RawMessage, key string) string {
	raw, ok := body[key]
	if !ok || string(raw) == "null" {
		return ""
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	return v
}
