package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"
)

// ---------- JSON helpers ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON: %v", err)
	}
}

// writeErr 业务错误简写
func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

// decodeJSON 解析请求体
func decodeJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

// apiError 业务错误，可携带当前状态与允许流转，供前端渲染错误态/冲突态
type apiError struct {
	HTTPCode int
	Msg      string
	Current  string
	Allowed  []string
}

func (e *apiError) write(w http.ResponseWriter) {
	body := map[string]any{"error": e.Msg}
	if e.Current != "" {
		body["current_status"] = e.Current
	}
	if e.Allowed != nil {
		body["allowed"] = e.Allowed
	}
	writeJSON(w, e.HTTPCode, body)
}

// ---------- misc ----------

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }

func todayLocal() string { return time.Now().Format("2006-01-02") }

func randToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// maskName 姓名脱敏：仅保留姓氏，如 张伟 -> 张*
func maskName(full string) string {
	r := []rune(strings.TrimSpace(full))
	if len(r) == 0 {
		return "*"
	}
	return string(r[0]) + "*"
}

// maskIDCard 身份证脱敏：前4后4
func maskIDCard(card string) string {
	r := []rune(card)
	if len(r) <= 8 {
		return card
	}
	return string(r[:4]) + strings.Repeat("*", len(r)-8) + string(r[len(r)-4:])
}

// maskPhone 手机号脱敏：前3后4
func maskPhone(p string) string {
	r := []rune(p)
	if len(r) < 7 {
		return p
	}
	return string(r[:3]) + "****" + string(r[len(r)-4:])
}

// workerLabel 列表/看板展示用：脱敏缩写 + 工号
func workerLabel(fullName, jobNo string) string {
	return maskName(fullName) + " · " + jobNo
}
