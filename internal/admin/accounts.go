package admin

import (
	"net/http"
	"strings"

	"hookd/internal/store"
)

// accountsData is the account list page: one page of accounts and the
// "new account" form.
type accountsData struct {
	Accounts []store.AccountSummary
	// Next is the cursor of the next page, or empty on the last page.
	Next string
	// Paged is true when this is not the first page.
	Paged bool
	// Name and Error are the form's input and its error, if any.
	Name  string
	Error string
}

// listAccounts shows the accounts, oldest first, pageSize at a time. The
// ?after= cursor is the ID of the last account of the previous page (R6).
func (h *Handler) listAccounts(w http.ResponseWriter, r *http.Request) {
	h.renderAccounts(w, r, http.StatusOK, accountsData{})
}

// renderAccounts shows the page of accounts given by r's ?after= cursor with
// the form as in data.
func (h *Handler) renderAccounts(w http.ResponseWriter, r *http.Request, status int, data accountsData) {
	after := r.URL.Query().Get("after")
	accts, err := h.store.ListAccounts(r.Context(), pageSize+1, after)
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	if len(accts) > pageSize {
		accts = accts[:pageSize]
		data.Next = accts[pageSize-1].ID
	}
	data.Accounts = accts
	data.Paged = after != ""
	h.render(w, r, status, "accounts", "Accounts", data)
}

// accountCreatedData is the page shown once after an account is created.
type accountCreatedData struct {
	Account store.Account
	Key     string
}

// createAccount creates an account and its first API key, like
// "hookd account create", and shows the key this one time (R7, R8). It
// answers with the page instead of redirecting, so the key is never in a
// URL or the browser history.
func (h *Handler) createAccount(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		h.renderAccounts(w, r, http.StatusUnprocessableEntity, accountsData{
			Name:  r.PostFormValue("name"),
			Error: "Enter a name for the account.",
		})
		return
	}
	acct, err := h.store.CreateAccount(r.Context(), name)
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	_, key, err := h.store.CreateAPIKey(r.Context(), acct.ID)
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, "account_created", "Account created", accountCreatedData{Account: acct, Key: key})
}
