package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type ctxKey string

const ctxUser ctxKey = "user"

func hashPassword(pw string) string {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		panic(err)
	}
	return string(h)
}

func (s *server) loadUserByToken(token string) (*User, error) {
	var expires string
	u := &User{}
	err := s.db.QueryRow(`
		SELECT u.id, u.username, u.role, u.name, u.site_id, u.team, u.worker_id, s.expires_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token = ?`, token).
		Scan(&u.ID, &u.Username, &u.Role, &u.Name, &u.SiteID, &u.Team, &u.WorkerID, &expires)
	if err != nil {
		return nil, err
	}
	exp, err := time.Parse(time.RFC3339, expires)
	if err != nil || time.Now().UTC().After(exp) {
		s.db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
		return nil, sql.ErrNoRows
	}
	u.RoleLabel = roleLabels[u.Role]
	return u, nil
}

// requireAuth 校验 Bearer token，把用户塞进 context
func (s *server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" {
			writeErr(w, http.StatusUnauthorized, "未登录或登录已过期")
			return
		}
		u, err := s.loadUserByToken(token)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "未登录或登录已过期")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxUser, u)))
	}
}

func currentUser(r *http.Request) *User {
	u, _ := r.Context().Value(ctxUser).(*User)
	return u
}

// ---------- handlers ----------

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	var (
		id       int64
		hash     string
		role     string
		name     string
		siteID   *int64
		team     string
		workerID *int64
	)
	err := s.db.QueryRow(`SELECT id, password_hash, role, name, site_id, team, worker_id FROM users WHERE username = ?`, req.Username).
		Scan(&id, &hash, &role, &name, &siteID, &team, &workerID)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) != nil {
		writeErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	token := randToken()
	_, err = s.db.Exec(`INSERT INTO sessions(token, user_id, created_at, expires_at) VALUES(?,?,?,?)`,
		token, id, nowUTC(), time.Now().UTC().Add(12*time.Hour).Format(time.RFC3339))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "登录失败，请重试")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token": token,
		"user":  &User{ID: id, Username: req.Username, Role: role, RoleLabel: roleLabels[role], Name: name, SiteID: siteID, Team: team, WorkerID: workerID},
	})
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"user": currentUser(r)})
}
