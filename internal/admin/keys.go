package admin

import (
	"errors"
	"net/http"

	"hookd/internal/store"
)

// accountData is the account page: the account, its API keys and links to
// its other pages (R9, R10).
type accountData struct {
	Account store.Account
	Keys    []store.APIKey
}

// showAccount shows an account and its API keys, by prefix only.
func (h *Handler) showAccount(w http.ResponseWriter, r *http.Request) {
	acct, ok := h.account(w, r)
	if !ok {
		return
	}
	keys, err := h.store.ListAPIKeys(r.Context(), acct.ID)
	if errors.Is(err, store.ErrNotFound) {
		h.notFound(w, r)
		return
	}
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, "account", acct.Name, accountData{Account: acct, Keys: keys})
}

// keyCreatedData is the page shown once after an API key is created.
type keyCreatedData struct {
	Account store.Account
	Prefix  string
	Key     string
}

// createKey creates an API key for the account and shows it this one time
// (R8). Like createAccount it answers with the page instead of redirecting,
// so the key is never in a URL or the browser history.
func (h *Handler) createKey(w http.ResponseWriter, r *http.Request) {
	acct, ok := h.account(w, r)
	if !ok {
		return
	}
	k, key, err := h.store.CreateAPIKey(r.Context(), acct.ID)
	if errors.Is(err, store.ErrNotFound) {
		h.notFound(w, r)
		return
	}
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, "key_created", "API key created", keyCreatedData{Account: acct, Prefix: k.Prefix, Key: key})
}

// revokeKeyData is the confirm page for revoking an API key.
type revokeKeyData struct {
	Account store.Account
	Key     store.APIKey
}

// confirmRevokeKey shows the confirm page. It changes nothing (R9).
func (h *Handler) confirmRevokeKey(w http.ResponseWriter, r *http.Request) {
	acct, key, ok := h.accountKey(w, r)
	if !ok {
		return
	}
	h.render(w, r, http.StatusOK, "revoke_key", "Revoke API key", revokeKeyData{Account: acct, Key: key})
}

// revokeKey deletes the API key, so it stops working with the API at once,
// and goes back to the account page (R9).
func (h *Handler) revokeKey(w http.ResponseWriter, r *http.Request) {
	acct, key, ok := h.accountKey(w, r)
	if !ok {
		return
	}
	_, err := h.store.DeleteAPIKey(r.Context(), key.ID)
	if errors.Is(err, store.ErrNotFound) {
		h.notFound(w, r)
		return
	}
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/accounts/"+acct.ID, http.StatusSeeOther)
}

// account loads the account named by the {acct} path value. If it is not
// found, or the store fails, it writes the error page and returns false.
func (h *Handler) account(w http.ResponseWriter, r *http.Request) (store.Account, bool) {
	acct, err := h.store.GetAccount(r.Context(), r.PathValue("acct"))
	if errors.Is(err, store.ErrNotFound) {
		h.notFound(w, r)
		return store.Account{}, false
	}
	if err != nil {
		h.storeError(w, r, err)
		return store.Account{}, false
	}
	return acct, true
}

// accountKey loads the account and the API key named by the {acct} and {key}
// path values. A key of another account is not found, like an unknown one.
func (h *Handler) accountKey(w http.ResponseWriter, r *http.Request) (store.Account, store.APIKey, bool) {
	acct, ok := h.account(w, r)
	if !ok {
		return store.Account{}, store.APIKey{}, false
	}
	keys, err := h.store.ListAPIKeys(r.Context(), acct.ID)
	if errors.Is(err, store.ErrNotFound) {
		h.notFound(w, r)
		return store.Account{}, store.APIKey{}, false
	}
	if err != nil {
		h.storeError(w, r, err)
		return store.Account{}, store.APIKey{}, false
	}
	id := r.PathValue("key")
	for _, k := range keys {
		if k.ID == id {
			return acct, k, true
		}
	}
	h.notFound(w, r)
	return store.Account{}, store.APIKey{}, false
}
