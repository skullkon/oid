package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type sessionStore struct {
	mu       sync.RWMutex
	sessions map[string]session
}

type session struct {
	Email   string
	Name    string
	Subject string
	Issuer  string
	Roles   []string
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: make(map[string]session)}
}

func (s *sessionStore) set(id string, data session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = data
}

func (s *sessionStore) get(id string) (session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, ok := s.sessions[id]
	return data, ok
}

func (s *sessionStore) delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

type server struct {
	store      *sessionStore
	oauth2Cfg  *oauth2.Config
	verifier   *oidc.IDTokenVerifier
	issuer     string
	usePKCE    bool
	cookieName string
}

type idTokenClaims struct {
	Email   string   `json:"email"`
	Name    string   `json:"name"`
	Roles   []string `json:"roles"`
	Groups  []string `json:"groups"`
	Subject string   `json:"sub"`
	Issuer  string   `json:"iss"`
}

func main() {
	issuer := mustEnv("OIDC_ISSUER")
	clientID := mustEnv("OIDC_CLIENT_ID")
	clientSecret := os.Getenv("OIDC_CLIENT_SECRET")
	redirectURL := mustEnv("OIDC_REDIRECT_URL")

	usePKCE := strings.EqualFold(os.Getenv("OIDC_USE_PKCE"), "true")
	cookieName := envOrDefault("SESSION_COOKIE", "oidc_example_session")
	listenAddr := envOrDefault("LISTEN_ADDR", ":8080")

	ctx := context.Background()
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		log.Fatalf("init provider: %v", err)
	}

	config := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  redirectURL,
		Scopes:       scopesFromEnv(),
	}

	verifier := provider.Verifier(&oidc.Config{ClientID: clientID})

	srv := &server{
		store:      newSessionStore(),
		oauth2Cfg:  config,
		verifier:   verifier,
		issuer:     issuer,
		usePKCE:    usePKCE,
		cookieName: cookieName,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleIndex)
	mux.HandleFunc("/login", srv.handleLogin)
	mux.HandleFunc("/callback", srv.handleCallback)
	mux.HandleFunc("/me", srv.handleMe)
	mux.HandleFunc("/logout", srv.handleLogout)
	mux.HandleFunc("/health", handleHealth)

	log.Printf("listening on %s", listenAddr)
	if err := http.ListenAndServe(listenAddr, logRequest(mux)); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := readCookie(r, s.cookieName)
	if !ok {
		fmt.Fprintf(w, "OIDC demo. Visit /login to authenticate.\n")
		return
	}

	info, ok := s.store.get(sessionID)
	if !ok {
		fmt.Fprintf(w, "Session missing. Visit /login.\n")
		return
	}

	fmt.Fprintf(w, "Logged in as %s (%s). Visit /me or /logout.\n", info.Name, info.Email)
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	state := mustRandom(32)
	verifier := ""
	opts := []oauth2.AuthCodeOption{}
	if s.usePKCE {
		verifier = mustRandom(64)
		opts = append(opts, oauth2.SetAuthURLParam("code_challenge", pkceChallenge(verifier)))
		opts = append(opts, oauth2.SetAuthURLParam("code_challenge_method", "S256"))
	}

	writeCookie(w, "oidc_state", state, 10*time.Minute)
	if verifier != "" {
		writeCookie(w, "oidc_verifier", verifier, 10*time.Minute)
	}

	url := s.oauth2Cfg.AuthCodeURL(state, opts...)
	http.Redirect(w, r, url, http.StatusFound)
}

func (s *server) handleCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	state := r.URL.Query().Get("state")
	if state == "" {
		http.Error(w, "missing state", http.StatusBadRequest)
		return
	}

	storedState, ok := readCookie(r, "oidc_state")
	if !ok || storedState != state {
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}

	opts := []oauth2.AuthCodeOption{}
	if s.usePKCE {
		verifier, ok := readCookie(r, "oidc_verifier")
		if !ok {
			http.Error(w, "missing pkce verifier", http.StatusBadRequest)
			return
		}
		opts = append(opts, oauth2.SetAuthURLParam("code_verifier", verifier))
	}

	token, err := s.oauth2Cfg.Exchange(ctx, code, opts...)
	if err != nil {
		http.Error(w, fmt.Sprintf("token exchange failed: %v", err), http.StatusBadGateway)
		return
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		http.Error(w, "missing id_token", http.StatusBadGateway)
		return
	}

	idToken, err := s.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		http.Error(w, fmt.Sprintf("verify id_token failed: %v", err), http.StatusBadGateway)
		return
	}

	claims := idTokenClaims{}
	if err := idToken.Claims(&claims); err != nil {
		http.Error(w, fmt.Sprintf("decode claims failed: %v", err), http.StatusBadGateway)
		return
	}

	if claims.Subject == "" {
		claims.Subject = idToken.Subject
	}
	if claims.Issuer == "" {
		claims.Issuer = idToken.Issuer
	}

	roles := claims.Roles
	if len(roles) == 0 {
		roles = claims.Groups
	}

	id := mustRandom(32)
	s.store.set(id, session{
		Email:   claims.Email,
		Name:    claims.Name,
		Subject: claims.Subject,
		Issuer:  claims.Issuer,
		Roles:   roles,
	})

	writeCookie(w, s.cookieName, id, 12*time.Hour)
	clearCookie(w, "oidc_state")
	clearCookie(w, "oidc_verifier")

	http.Redirect(w, r, "/me", http.StatusFound)
}

func (s *server) handleMe(w http.ResponseWriter, r *http.Request) {
	id, ok := readCookie(r, s.cookieName)
	if !ok {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}

	info, ok := s.store.get(id)
	if !ok {
		http.Error(w, "session missing", http.StatusUnauthorized)
		return
	}

	payload := map[string]any{
		"email":   info.Email,
		"name":    info.Name,
		"sub":     info.Subject,
		"issuer":  info.Issuer,
		"roles":   info.Roles,
		"message": "use roles/claims to authorize requests",
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(payload)
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	id, ok := readCookie(r, s.cookieName)
	if ok {
		s.store.delete(id)
	}

	clearCookie(w, s.cookieName)
	fmt.Fprintln(w, "logged out")
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

func logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

func mustEnv(key string) string {
	value := os.Getenv(key)
	if value == "" {
		log.Fatalf("missing required env: %s", key)
	}
	return value
}

func envOrDefault(key, fallback string) string {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	return value
}

func scopesFromEnv() []string {
	raw := os.Getenv("OIDC_SCOPES")
	if raw == "" {
		return []string{"openid", "profile", "email"}
	}
	parts := strings.Split(raw, ",")
	var scopes []string
	for _, p := range parts {
		s := strings.TrimSpace(p)
		if s != "" {
			scopes = append(scopes, s)
		}
	}
	return scopes
}

func mustRandom(size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Errorf("random: %w", err))
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func writeCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		Expires:  time.Now().Add(ttl),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func readCookie(r *http.Request, name string) (string, bool) {
	cookie, err := r.Cookie(name)
	if err != nil {
		return "", false
	}
	return cookie.Value, true
}

func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}
