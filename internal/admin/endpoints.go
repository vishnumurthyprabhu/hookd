package admin

import (
	"errors"
	"net/http"
	"strings"

	"hookd/internal/store"
	"hookd/internal/validate"
)

// endpointsData is the endpoint list page of one account (R11).
type endpointsData struct {
	Account   store.Account
	Endpoints []endpointRow
	// Next is the cursor of the next page, or empty on the last page.
	Next string
	// Paged is true when this is not the first page.
	Paged bool
}

// endpointRow is one endpoint in the list.
type endpointRow struct {
	store.Endpoint
	// Test is true for a test endpoint, which delivers to the built-in
	// receiver.
	Test bool
}

// listEndpoints shows the account's endpoints, oldest first, pageSize at a
// time, with test endpoints marked. The ?after= cursor is the ID of the last
// endpoint of the previous page.
func (h *Handler) listEndpoints(w http.ResponseWriter, r *http.Request) {
	acct, ok := h.account(w, r)
	if !ok {
		return
	}
	after := r.URL.Query().Get("after")
	eps, err := h.store.ListEndpoints(r.Context(), acct.ID, pageSize+1, after)
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	rcvs, err := h.store.ListTestReceivers(r.Context(), acct.ID)
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	test := map[string]bool{}
	for _, rc := range rcvs {
		test[rc.EndpointID] = true
	}

	data := endpointsData{Account: acct, Paged: after != ""}
	if len(eps) > pageSize {
		eps = eps[:pageSize]
		data.Next = eps[pageSize-1].ID
	}
	for _, e := range eps {
		data.Endpoints = append(data.Endpoints, endpointRow{Endpoint: e, Test: test[e.ID]})
	}
	h.render(w, r, http.StatusOK, "endpoints", "Endpoints of "+acct.Name, data)
}

// endpointFormData is the create and the edit form. URL, Description and
// EventTypes hold the input as entered, so an invalid form is shown again
// unchanged (R5).
type endpointFormData struct {
	Account store.Account
	// Endpoint is the endpoint being edited, or the zero value on the create
	// form.
	Endpoint    store.Endpoint
	Test        bool
	URL         string
	Description string
	EventTypes  string
	Errors      []string
}

// endpointInput is a checked endpoint form.
type endpointInput struct {
	URL         string
	Description string
	EventTypes  []string
}

// parseEndpointForm checks the endpoint form of r with the same rules as the
// API (R11, N4) and returns the input and the form as entered. Event types
// are separated by commas or new lines, and none means all types.
func parseEndpointForm(r *http.Request) (endpointInput, endpointFormData) {
	form := endpointFormData{
		URL:         r.PostFormValue("url"),
		Description: r.PostFormValue("description"),
		EventTypes:  r.PostFormValue("event_types"),
	}
	in := endpointInput{
		URL:         strings.TrimSpace(form.URL),
		Description: strings.TrimSpace(form.Description),
	}
	if _, msg := validate.URL(in.URL); msg != "" {
		form.Errors = append(form.Errors, sentence(msg))
	}
	if _, msg := validate.Description(in.Description); msg != "" {
		form.Errors = append(form.Errors, sentence(msg))
	}
	types, _, msg := validate.EventTypes(splitEventTypes(form.EventTypes))
	if msg != "" {
		form.Errors = append(form.Errors, sentence(msg))
	}
	in.EventTypes = types
	return in, form
}

// splitEventTypes splits a list of event types separated by commas or new
// lines, dropping spaces around each and empty entries.
func splitEventTypes(s string) []string {
	types := []string{}
	for _, t := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		if t = strings.TrimSpace(t); t != "" {
			types = append(types, t)
		}
	}
	return types
}

// sentence turns a check's message into a sentence for the page.
func sentence(msg string) string {
	for field, name := range map[string]string{"url ": "URL ", "event_type ": "Event type ", "Idempotency-Key ": "Idempotency key "} {
		if rest, ok := strings.CutPrefix(msg, field); ok {
			return name + rest + "."
		}
	}
	return strings.ToUpper(msg[:1]) + msg[1:] + "."
}

// newEndpointForm shows the empty create form.
func (h *Handler) newEndpointForm(w http.ResponseWriter, r *http.Request) {
	acct, ok := h.account(w, r)
	if !ok {
		return
	}
	h.render(w, r, http.StatusOK, "endpoint_new", "New endpoint", endpointFormData{Account: acct})
}

// createEndpoint creates an enabled endpoint and goes to the endpoint list.
// The secret is not shown here; it has its own page (R12).
func (h *Handler) createEndpoint(w http.ResponseWriter, r *http.Request) {
	acct, ok := h.account(w, r)
	if !ok {
		return
	}
	in, form := parseEndpointForm(r)
	if len(form.Errors) > 0 {
		form.Account = acct
		h.render(w, r, http.StatusUnprocessableEntity, "endpoint_new", "New endpoint", form)
		return
	}
	_, _, err := h.store.CreateEndpoint(r.Context(), acct.ID, store.NewEndpoint{
		URL:         in.URL,
		Description: in.Description,
		EventTypes:  in.EventTypes,
	})
	if errors.Is(err, store.ErrNotFound) {
		h.notFound(w, r)
		return
	}
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/accounts/"+acct.ID+"/endpoints", http.StatusSeeOther)
}

// showEndpoint shows the edit form of an endpoint, without its secret.
func (h *Handler) showEndpoint(w http.ResponseWriter, r *http.Request) {
	acct, ep, ok := h.accountEndpoint(w, r)
	if !ok {
		return
	}
	test, ok := h.isTestEndpoint(w, r, acct.ID, ep.ID)
	if !ok {
		return
	}
	h.render(w, r, http.StatusOK, "endpoint", "Endpoint", endpointFormData{
		Account:     acct,
		Endpoint:    ep,
		Test:        test,
		URL:         ep.URL,
		Description: ep.Description,
		EventTypes:  strings.Join(ep.EventTypes, "\n"),
	})
}

// updateEndpoint saves the edit form and goes back to the endpoint page.
func (h *Handler) updateEndpoint(w http.ResponseWriter, r *http.Request) {
	acct, ep, ok := h.accountEndpoint(w, r)
	if !ok {
		return
	}
	in, form := parseEndpointForm(r)
	if len(form.Errors) > 0 {
		test, ok := h.isTestEndpoint(w, r, acct.ID, ep.ID)
		if !ok {
			return
		}
		form.Account, form.Endpoint, form.Test = acct, ep, test
		h.render(w, r, http.StatusUnprocessableEntity, "endpoint", "Endpoint", form)
		return
	}
	_, err := h.store.UpdateEndpoint(r.Context(), acct.ID, ep.ID, store.EndpointUpdate{
		URL:         &in.URL,
		Description: &in.Description,
		EventTypes:  &in.EventTypes,
	})
	if !h.endpointDone(w, r, err) {
		return
	}
	http.Redirect(w, r, endpointPath(acct.ID, ep.ID), http.StatusSeeOther)
}

// enableEndpoint and disableEndpoint switch an endpoint on or off (R11) and
// go back to the endpoint page.
func (h *Handler) enableEndpoint(w http.ResponseWriter, r *http.Request) {
	h.setEnabled(w, r, true)
}

func (h *Handler) disableEndpoint(w http.ResponseWriter, r *http.Request) {
	h.setEnabled(w, r, false)
}

func (h *Handler) setEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	acct, ep, ok := h.accountEndpoint(w, r)
	if !ok {
		return
	}
	_, err := h.store.UpdateEndpoint(r.Context(), acct.ID, ep.ID, store.EndpointUpdate{Enabled: &enabled})
	if !h.endpointDone(w, r, err) {
		return
	}
	http.Redirect(w, r, endpointPath(acct.ID, ep.ID), http.StatusSeeOther)
}

// endpointPageData is a page about one endpoint: the delete confirm page
// and the secret page.
type endpointPageData struct {
	Account  store.Account
	Endpoint store.Endpoint
	Secret   string
}

// confirmDeleteEndpoint shows the confirm page. It changes nothing (R13).
func (h *Handler) confirmDeleteEndpoint(w http.ResponseWriter, r *http.Request) {
	acct, ep, ok := h.accountEndpoint(w, r)
	if !ok {
		return
	}
	h.render(w, r, http.StatusOK, "delete_endpoint", "Delete endpoint", endpointPageData{Account: acct, Endpoint: ep})
}

// deleteEndpoint soft-deletes the endpoint, as the API does, and goes to the
// endpoint list (R13, N4).
func (h *Handler) deleteEndpoint(w http.ResponseWriter, r *http.Request) {
	acct, ep, ok := h.accountEndpoint(w, r)
	if !ok {
		return
	}
	if !h.endpointDone(w, r, h.store.DeleteEndpoint(r.Context(), acct.ID, ep.ID)) {
		return
	}
	http.Redirect(w, r, "/admin/accounts/"+acct.ID+"/endpoints", http.StatusSeeOther)
}

// showEndpointSecret shows the endpoint's signing secret with a copy button.
// No other page shows it (R12).
func (h *Handler) showEndpointSecret(w http.ResponseWriter, r *http.Request) {
	acct, ep, ok := h.accountEndpoint(w, r)
	if !ok {
		return
	}
	secret, err := h.store.GetEndpointSecret(r.Context(), acct.ID, ep.ID)
	if !h.endpointDone(w, r, err) {
		return
	}
	h.render(w, r, http.StatusOK, "endpoint_secret", "Signing secret", endpointPageData{Account: acct, Endpoint: ep, Secret: secret})
}

// endpointPath is the address of an endpoint's page.
func endpointPath(acctID, epID string) string {
	return "/admin/accounts/" + acctID + "/endpoints/" + epID
}

// accountEndpoint loads the account and the endpoint named by the {acct} and
// {ep} path values. An endpoint of another account, or a deleted one, is not
// found, like an unknown one.
func (h *Handler) accountEndpoint(w http.ResponseWriter, r *http.Request) (store.Account, store.Endpoint, bool) {
	acct, ok := h.account(w, r)
	if !ok {
		return store.Account{}, store.Endpoint{}, false
	}
	ep, err := h.store.GetEndpoint(r.Context(), acct.ID, r.PathValue("ep"))
	if !h.endpointDone(w, r, err) {
		return store.Account{}, store.Endpoint{}, false
	}
	return acct, ep, true
}

// isTestEndpoint reports whether the endpoint is a test endpoint. If the
// store fails, it writes the error page and returns false as its second
// result.
func (h *Handler) isTestEndpoint(w http.ResponseWriter, r *http.Request, acctID, epID string) (test, ok bool) {
	rcvs, err := h.store.ListTestReceivers(r.Context(), acctID)
	if err != nil {
		h.storeError(w, r, err)
		return false, false
	}
	for _, rc := range rcvs {
		if rc.EndpointID == epID {
			return true, true
		}
	}
	return false, true
}

// endpointDone writes the error page for err, the result of a store call
// about one endpoint, and reports whether err was nil.
func (h *Handler) endpointDone(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, store.ErrNotFound):
		h.notFound(w, r)
	default:
		h.storeError(w, r, err)
	}
	return false
}
