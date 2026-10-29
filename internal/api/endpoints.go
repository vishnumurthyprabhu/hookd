package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"hookd/internal/store"
	"hookd/internal/validate"
)

const (
	// maxEndpointBodyBytes is the largest body POST and PATCH
	// /v1/endpoints accept.
	maxEndpointBodyBytes = 64 << 10

	// defaultListLimit and maxListLimit bound the page size of
	// GET /v1/endpoints.
	defaultListLimit = 50
	maxListLimit     = 100
)

type createEndpointRequest struct {
	URL         string   `json:"url"`
	Description string   `json:"description"`
	EventTypes  []string `json:"event_types"`
}

// updateEndpointRequest holds the fields of PATCH /v1/endpoints/{id}.
// Fields left out, or null, are not changed.
type updateEndpointRequest struct {
	URL         *string   `json:"url"`
	Description *string   `json:"description"`
	EventTypes  *[]string `json:"event_types"`
	Enabled     *bool     `json:"enabled"`
}

type endpointResponse struct {
	ID          string    `json:"id"`
	URL         string    `json:"url"`
	Description string    `json:"description"`
	EventTypes  []string  `json:"event_types"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// Secret is only set when the endpoint is created.
	Secret string `json:"secret,omitempty"`
}

type endpointListResponse struct {
	Data []endpointResponse `json:"data"`
	// HasMore is true when there are endpoints after the last one in Data.
	// Pass the last ID as "after" to get them.
	HasMore bool `json:"has_more"`
}

type secretResponse struct {
	Secret string `json:"secret"`
}

func newEndpointResponse(e store.Endpoint) endpointResponse {
	types := e.EventTypes
	if types == nil {
		types = []string{}
	}
	return endpointResponse{
		ID:          e.ID,
		URL:         e.URL,
		Description: e.Description,
		EventTypes:  types,
		Enabled:     e.Enabled,
		CreatedAt:   e.CreatedAt.UTC(),
		UpdatedAt:   e.UpdatedAt.UTC(),
	}
}

// createEndpoint handles POST /v1/endpoints (R4, R5). It answers 201 with
// the new endpoint, including its signing secret.
func (s *Server) createEndpoint(w http.ResponseWriter, r *http.Request) {
	acct, _ := accountFrom(r.Context())

	var req createEndpointRequest
	if status, code, msg := decodeBody(w, r, &req); code != "" {
		writeError(w, status, code, msg)
		return
	}
	if code, msg := validate.URL(req.URL); code != "" {
		writeError(w, http.StatusBadRequest, code, msg)
		return
	}
	if code, msg := validate.Description(req.Description); code != "" {
		writeError(w, http.StatusBadRequest, code, msg)
		return
	}
	types, code, msg := validate.EventTypes(req.EventTypes)
	if code != "" {
		writeError(w, http.StatusBadRequest, code, msg)
		return
	}

	e, secret, err := s.store.CreateEndpoint(r.Context(), acct.ID, store.NewEndpoint{
		URL:         req.URL,
		Description: req.Description,
		EventTypes:  types,
	})
	if err != nil {
		s.log.ErrorContext(r.Context(), "create endpoint", "err", err)
		writeError(w, http.StatusInternalServerError, CodeInternal, "internal server error")
		return
	}
	out := newEndpointResponse(e)
	out.Secret = secret
	w.Header().Set("Location", "/v1/endpoints/"+e.ID)
	writeJSON(w, http.StatusCreated, out)
}

// listEndpoints handles GET /v1/endpoints (R6). It returns up to "limit"
// endpoints, oldest first, after the endpoint ID given as "after".
func (s *Server) listEndpoints(w http.ResponseWriter, r *http.Request) {
	acct, _ := accountFrom(r.Context())

	q := r.URL.Query()
	limit := defaultListLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxListLimit {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest,
				fmt.Sprintf("limit must be a whole number from 1 to %d", maxListLimit))
			return
		}
		limit = n
	}

	// Ask for one more than the page holds to learn whether there are more.
	eps, err := s.store.ListEndpoints(r.Context(), acct.ID, limit+1, q.Get("after"))
	if err != nil {
		s.log.ErrorContext(r.Context(), "list endpoints", "err", err)
		writeError(w, http.StatusInternalServerError, CodeInternal, "internal server error")
		return
	}
	out := endpointListResponse{Data: make([]endpointResponse, 0, len(eps))}
	if len(eps) > limit {
		eps = eps[:limit]
		out.HasMore = true
	}
	for _, e := range eps {
		out.Data = append(out.Data, newEndpointResponse(e))
	}
	writeJSON(w, http.StatusOK, out)
}

// getEndpoint handles GET /v1/endpoints/{id} (R6). The secret is left out.
func (s *Server) getEndpoint(w http.ResponseWriter, r *http.Request) {
	acct, _ := accountFrom(r.Context())
	e, err := s.store.GetEndpoint(r.Context(), acct.ID, r.PathValue("id"))
	if err != nil {
		s.endpointError(w, r, "get endpoint", err)
		return
	}
	writeJSON(w, http.StatusOK, newEndpointResponse(e))
}

// updateEndpoint handles PATCH /v1/endpoints/{id} (R6, R7). Only the fields
// in the body are changed.
func (s *Server) updateEndpoint(w http.ResponseWriter, r *http.Request) {
	acct, _ := accountFrom(r.Context())

	var req updateEndpointRequest
	if status, code, msg := decodeBody(w, r, &req); code != "" {
		writeError(w, status, code, msg)
		return
	}
	u := store.EndpointUpdate{Description: req.Description, Enabled: req.Enabled}
	if req.URL != nil {
		if code, msg := validate.URL(*req.URL); code != "" {
			writeError(w, http.StatusBadRequest, code, msg)
			return
		}
		u.URL = req.URL
	}
	if req.Description != nil {
		if code, msg := validate.Description(*req.Description); code != "" {
			writeError(w, http.StatusBadRequest, code, msg)
			return
		}
	}
	if req.EventTypes != nil {
		types, code, msg := validate.EventTypes(*req.EventTypes)
		if code != "" {
			writeError(w, http.StatusBadRequest, code, msg)
			return
		}
		u.EventTypes = &types
	}

	e, err := s.store.UpdateEndpoint(r.Context(), acct.ID, r.PathValue("id"), u)
	if err != nil {
		s.endpointError(w, r, "update endpoint", err)
		return
	}
	writeJSON(w, http.StatusOK, newEndpointResponse(e))
}

// deleteEndpoint handles DELETE /v1/endpoints/{id} (R6, R7). It answers 204.
func (s *Server) deleteEndpoint(w http.ResponseWriter, r *http.Request) {
	acct, _ := accountFrom(r.Context())
	if err := s.store.DeleteEndpoint(r.Context(), acct.ID, r.PathValue("id")); err != nil {
		s.endpointError(w, r, "delete endpoint", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// getEndpointSecret handles GET /v1/endpoints/{id}/secret (R5).
func (s *Server) getEndpointSecret(w http.ResponseWriter, r *http.Request) {
	acct, _ := accountFrom(r.Context())
	secret, err := s.store.GetEndpointSecret(r.Context(), acct.ID, r.PathValue("id"))
	if err != nil {
		s.endpointError(w, r, "get endpoint secret", err)
		return
	}
	writeJSON(w, http.StatusOK, secretResponse{Secret: secret})
}

// endpointError answers 404 for store.ErrNotFound, which covers endpoints of
// other accounts and deleted ones, and 500 otherwise.
func (s *Server) endpointError(w http.ResponseWriter, r *http.Request, op string, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, CodeNotFound, "endpoint not found")
		return
	}
	s.log.ErrorContext(r.Context(), op, "err", err)
	writeError(w, http.StatusInternalServerError, CodeInternal, "internal server error")
}

// decodeBody reads a single JSON object from the request body into v,
// rejecting unknown fields. On failure it returns the status, code and
// message of the error to answer with.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) (status int, code, msg string) {
	body, tooLarge, err := readBody(w, r, maxEndpointBodyBytes)
	if tooLarge {
		return http.StatusRequestEntityTooLarge, CodePayloadTooLarge,
			fmt.Sprintf("request body is larger than %d bytes", maxEndpointBodyBytes)
	}
	if err != nil {
		return http.StatusBadRequest, CodeInvalidRequest, "could not read the request body"
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		switch {
		case errors.As(err, &syntaxErr), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
			return http.StatusBadRequest, CodeInvalidJSON, "request body is not valid JSON"
		case errors.As(err, &typeErr) && typeErr.Field == "":
			return http.StatusBadRequest, CodeInvalidRequest, "request body must be a JSON object"
		case errors.As(err, &typeErr):
			return http.StatusBadRequest, CodeInvalidRequest,
				fmt.Sprintf("field %q has the wrong type", typeErr.Field)
		default:
			return http.StatusBadRequest, CodeInvalidRequest, "invalid request body: " + err.Error()
		}
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return http.StatusBadRequest, CodeInvalidJSON, "request body must hold a single JSON object"
	}
	return 0, "", ""
}
