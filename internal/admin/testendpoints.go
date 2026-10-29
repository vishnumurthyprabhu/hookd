package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	standardwebhooks "github.com/standard-webhooks/standard-webhooks/libraries/go"

	"hookd/internal/store"
	"hookd/internal/validate"
)

// receiverBodyOverhead is how much more than the largest event payload the
// built-in receiver reads, as the API allows for its JSON envelope.
const receiverBodyOverhead = 64 << 10

// testEndpointData is what the page of a test endpoint shows besides the
// edit form: the receiver's answer status form and its received requests
// (R19, R20).
type testEndpointData struct {
	Receiver store.TestReceiver
	Requests []testRequestRow
	// Status is the answer status form's input, and StatusErrors its
	// errors.
	Status       string
	StatusErrors []string
}

// testRequestRow is one received request on a test endpoint's page.
type testRequestRow struct {
	store.TestRequest
	// Headers are the request's headers, sorted by name.
	Headers []headerLine
	// Body is the body, indented if it is valid JSON.
	Body string
}

type headerLine struct {
	Name, Value string
}

// newTestRequestRow prepares a received request for the page.
func newTestRequestRow(req store.TestRequest) testRequestRow {
	row := testRequestRow{TestRequest: req, Body: string(req.Body)}
	for name, values := range req.Headers {
		for _, v := range values {
			row.Headers = append(row.Headers, headerLine{Name: name, Value: v})
		}
	}
	sort.SliceStable(row.Headers, func(i, j int) bool { return row.Headers[i].Name < row.Headers[j].Name })
	var indented bytes.Buffer
	if json.Indent(&indented, bytes.TrimSpace(req.Body), "", "  ") == nil {
		row.Body = indented.String()
	}
	return row
}

// renderEndpoint shows the page of the endpoint in form, which holds the
// edit form. For a test endpoint it also loads the receiver and its
// received requests, newest first. form.Status, if empty, becomes the
// receiver's current status. Every endpoint page also shows the endpoint's
// recent attempts.
func (h *Handler) renderEndpoint(w http.ResponseWriter, r *http.Request, status int, form endpointFormData) {
	rcvs, err := h.store.ListTestReceivers(r.Context(), form.Account.ID)
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	for _, rc := range rcvs {
		if rc.EndpointID == form.Endpoint.ID {
			form.Test, form.Receiver = true, rc
		}
	}
	if form.Test {
		reqs, err := h.store.ListTestRequests(r.Context(), form.Account.ID, form.Endpoint.ID, store.MaxTestRequests)
		if !h.endpointDone(w, r, err) {
			return
		}
		for _, req := range reqs {
			form.Requests = append(form.Requests, newTestRequestRow(req))
		}
		if form.Status == "" {
			form.Status = strconv.Itoa(form.Receiver.ResponseStatus)
		}
	}
	form.attemptsData, err = h.loadEndpointAttempts(r, form.Account.ID, form.Endpoint.ID)
	if !h.endpointDone(w, r, err) {
		return
	}
	h.render(w, r, status, "endpoint", "Endpoint", form)
}

// parseStatus checks an answer status as entered: a whole number from 100
// to 599 (R19). It returns the status, or an error message for the page.
func parseStatus(raw string) (int, string) {
	s, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || s < 100 || s > 599 {
		return 0, "Answer status must be a whole number from 100 to 599."
	}
	return s, ""
}

// testEndpointFormData is the "new test endpoint" form, with the input as
// entered (R5).
type testEndpointFormData struct {
	Account     store.Account
	Description string
	EventTypes  string
	Status      string
	Errors      []string
}

// newTestEndpointForm shows the empty "new test endpoint" form.
func (h *Handler) newTestEndpointForm(w http.ResponseWriter, r *http.Request) {
	acct, ok := h.account(w, r)
	if !ok {
		return
	}
	h.render(w, r, http.StatusOK, "test_endpoint_new", "New test endpoint", testEndpointFormData{
		Account: acct,
		Status:  strconv.Itoa(store.DefaultTestResponseStatus),
	})
}

// createTestEndpoint creates, in one step, an endpoint that delivers to the
// built-in receiver (R17), and goes to its page.
func (h *Handler) createTestEndpoint(w http.ResponseWriter, r *http.Request) {
	acct, ok := h.account(w, r)
	if !ok {
		return
	}
	form := testEndpointFormData{
		Account:     acct,
		Description: r.PostFormValue("description"),
		EventTypes:  r.PostFormValue("event_types"),
		Status:      r.PostFormValue("status"),
	}
	description := strings.TrimSpace(form.Description)
	if _, msg := validate.Description(description); msg != "" {
		form.Errors = append(form.Errors, sentence(msg))
	}
	types, _, msg := validate.EventTypes(splitEventTypes(form.EventTypes))
	if msg != "" {
		form.Errors = append(form.Errors, sentence(msg))
	}
	status, msg := parseStatus(form.Status)
	if msg != "" {
		form.Errors = append(form.Errors, msg)
	}
	if len(form.Errors) > 0 {
		h.render(w, r, http.StatusUnprocessableEntity, "test_endpoint_new", "New test endpoint", form)
		return
	}
	te, err := h.store.CreateTestEndpoint(r.Context(), acct.ID, store.NewTestEndpoint{
		Description:    description,
		EventTypes:     types,
		ResponseStatus: status,
		BaseURL:        h.receiverBaseURL,
	})
	if !h.endpointDone(w, r, err) {
		return
	}
	http.Redirect(w, r, endpointPath(acct.ID, te.Endpoint.ID), http.StatusSeeOther)
}

// setTestStatus sets the status a test endpoint's receiver answers with
// (R19) and goes back to the endpoint page.
func (h *Handler) setTestStatus(w http.ResponseWriter, r *http.Request) {
	acct, ep, ok := h.accountEndpoint(w, r)
	if !ok {
		return
	}
	raw := r.PostFormValue("status")
	status, msg := parseStatus(raw)
	if msg != "" {
		form := endpointFormData{
			Account:     acct,
			Endpoint:    ep,
			URL:         ep.URL,
			Description: ep.Description,
			EventTypes:  strings.Join(ep.EventTypes, "\n"),
		}
		form.Status, form.StatusErrors = raw, []string{msg}
		h.renderEndpoint(w, r, http.StatusUnprocessableEntity, form)
		return
	}
	if !h.endpointDone(w, r, h.store.SetTestReceiverStatus(r.Context(), acct.ID, ep.ID, status)) {
		return
	}
	http.Redirect(w, r, endpointPath(acct.ID, ep.ID), http.StatusSeeOther)
}

// clearTestRequests deletes a test endpoint's received requests (R20) and
// goes back to the endpoint page.
func (h *Handler) clearTestRequests(w http.ResponseWriter, r *http.Request) {
	acct, ep, ok := h.accountEndpoint(w, r)
	if !ok {
		return
	}
	if !h.endpointDone(w, r, h.store.ClearTestRequests(r.Context(), acct.ID, ep.ID)) {
		return
	}
	http.Redirect(w, r, endpointPath(acct.ID, ep.ID), http.StatusSeeOther)
}

// receiveTestRequest is the built-in test receiver (R18). It checks the
// request's Standard Webhooks signature with the official library and the
// test endpoint's secret, stores the request, and answers with the
// receiver's configured status (R19). An unknown receiver, or one whose
// endpoint is deleted, answers 404 and stores nothing. The recorded headers
// and body are never logged (N3).
func (h *Handler) receiveTestRequest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("trcv")
	d, err := h.store.GetTestReceiver(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && d.EndpointDeleted) {
		receiverAnswer(w, http.StatusNotFound, map[string]any{"error": "no such test receiver"})
		return
	}
	if err != nil {
		h.log.ErrorContext(r.Context(), "test receiver: load", "receiver", id, "err", err)
		receiverAnswer(w, http.StatusServiceUnavailable, map[string]any{"error": "database unavailable"})
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxPayloadBytes()+receiverBodyOverhead))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			receiverAnswer(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "body too large"})
			return
		}
		receiverAnswer(w, http.StatusBadRequest, map[string]any{"error": "could not read the body"})
		return
	}

	in := store.NewTestRequest{
		WebhookID:      r.Header.Get("webhook-id"),
		Headers:        r.Header,
		Body:           body,
		ResponseStatus: d.Receiver.ResponseStatus,
	}
	if err := verify(d.Secret, body, r.Header); err != nil {
		in.VerifyError = err.Error()
	} else {
		in.Verified = true
	}
	_, err = h.store.RecordTestRequest(r.Context(), id, in)
	if errors.Is(err, store.ErrNotFound) {
		// The endpoint was deleted in the meantime.
		receiverAnswer(w, http.StatusNotFound, map[string]any{"error": "no such test receiver"})
		return
	}
	if err != nil {
		h.log.ErrorContext(r.Context(), "test receiver: record", "receiver", id, "err", err)
		receiverAnswer(w, http.StatusServiceUnavailable, map[string]any{"error": "database unavailable"})
		return
	}
	receiverAnswer(w, in.ResponseStatus, map[string]any{"verified": in.Verified})
}

// verify checks a request with the official Standard Webhooks library, so
// the check does not depend on Hookd's own signer.
func verify(secret string, body []byte, header http.Header) error {
	wh, err := standardwebhooks.NewWebhook(secret)
	if err != nil {
		return err
	}
	return wh.Verify(body, header)
}

// receiverAnswer writes the receiver's short JSON answer with status. A
// status that has no body, such as 204, is sent without one.
func receiverAnswer(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Cache-Control", "no-store")
	if status < 200 || status == http.StatusNoContent || status == http.StatusNotModified {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
