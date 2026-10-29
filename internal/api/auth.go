package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"hookd/internal/ids"
	"hookd/internal/store"
)

type accountKey struct{}

// accountFrom returns the account that authenticated the request. It is set
// on every request that reaches a /v1 handler.
func accountFrom(ctx context.Context) (store.Account, bool) {
	a, ok := ctx.Value(accountKey{}).(store.Account)
	return a, ok
}

// authenticate requires a valid "Authorization: Bearer <key>" header. It
// hashes the key, looks up its account and puts the account in the request
// context. Without a valid key it answers 401. The header and the key are
// never logged (N4).
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, msg := bearerToken(r.Header.Get("Authorization"))
		if msg != "" {
			unauthorized(w, msg)
			return
		}
		acct, err := s.store.AccountByAPIKeyHash(r.Context(), ids.HashAPIKey(key))
		if errors.Is(err, store.ErrNotFound) {
			unauthorized(w, "invalid API key")
			return
		}
		if err != nil {
			s.log.ErrorContext(r.Context(), "look up api key", "err", err)
			writeError(w, http.StatusInternalServerError, CodeInternal, "internal server error")
			return
		}
		if info := requestInfoFrom(r.Context()); info != nil {
			info.accountID = acct.ID
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), accountKey{}, acct)))
	})
}

// bearerToken returns the token of a bearer Authorization header, or a
// message saying why there is none.
func bearerToken(header string) (token, msg string) {
	if header == "" {
		return "", "missing API key: send \"Authorization: Bearer <key>\""
	}
	scheme, token, ok := strings.Cut(header, " ")
	token = strings.TrimSpace(token)
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t") {
		return "", "malformed Authorization header: want \"Bearer <key>\""
	}
	return token, ""
}

func unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="hookd"`)
	writeError(w, http.StatusUnauthorized, CodeUnauthorized, msg)
}
