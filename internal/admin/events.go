package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"hookd/internal/store"
	"hookd/internal/validate"
)

// formOverhead is how much larger than three times the payload limit a
// send-event form may be. URL encoding can triple the payload's size, and
// the event type and idempotency key need room too.
const formOverhead = 4 << 10

// eventsData is the event list page of one account (R15).
type eventsData struct {
	Account store.Account
	Events  []eventRow
	// Type is the ?type= filter, or empty for all types.
	Type string
	// Next is the cursor of the next page, or empty on the last page.
	Next string
	// Paged is true when this is not the first page.
	Paged bool
}

// eventRow is one event in the list with its delivery counts. Pending
// counts the deliveries that are pending or being sent.
type eventRow struct {
	store.EventSummary
	Succeeded, Failed, Pending int
}

// listEvents shows the account's events, newest first, pageSize at a time
// (R15). The ?type= filter keeps one event type, and the ?before= cursor is
// the ID of the last event of the previous page.
func (h *Handler) listEvents(w http.ResponseWriter, r *http.Request) {
	acct, ok := h.account(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	data := eventsData{Account: acct, Type: strings.TrimSpace(q.Get("type")), Paged: q.Get("before") != ""}
	evs, err := h.store.ListEvents(r.Context(), acct.ID, store.EventFilter{
		EventType: data.Type,
		Before:    q.Get("before"),
		Limit:     pageSize + 1,
	})
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	if len(evs) > pageSize {
		evs = evs[:pageSize]
		data.Next = evs[pageSize-1].ID
	}
	for _, e := range evs {
		data.Events = append(data.Events, eventRow{
			EventSummary: e,
			Succeeded:    e.Deliveries[store.DeliverySucceeded],
			Failed:       e.Deliveries[store.DeliveryFailed],
			Pending:      e.Deliveries[store.DeliveryPending] + e.Deliveries[store.DeliveryInProgress],
		})
	}
	h.render(w, r, http.StatusOK, "events", "Events of "+acct.Name, data)
}

// eventFormData is the send-event form, with the input as entered so an
// invalid form is shown again unchanged (R5).
type eventFormData struct {
	Account        store.Account
	EventType      string
	Payload        string
	IdempotencyKey string
	Errors         []string
}

// newEventForm shows the empty send-event form.
func (h *Handler) newEventForm(w http.ResponseWriter, r *http.Request) {
	acct, ok := h.account(w, r)
	if !ok {
		return
	}
	h.render(w, r, http.StatusOK, "event_new", "Send an event", eventFormData{Account: acct})
}

// sendEvent checks the send-event form with the same rules as
// POST /v1/events, stores the event, wakes the workers and goes to the
// event's page (R14, N4). If the idempotency key repeats one from within
// the idempotency window, it goes to the existing event instead.
func (h *Handler) sendEvent(w http.ResponseWriter, r *http.Request) {
	acct, ok := h.account(w, r)
	if !ok {
		return
	}
	// Read the limit once, so the form is checked against one value even if
	// the setting changes meanwhile.
	maxPayload := h.maxPayloadBytes()
	r.Body = http.MaxBytesReader(w, r.Body, 3*maxPayload+formOverhead)
	if err := r.ParseForm(); err != nil {
		var tooLarge *http.MaxBytesError
		msg := "Hookd could not read the form. Try sending it again."
		if errors.As(err, &tooLarge) {
			msg = sentence(validate.PayloadTooLargeMessage(maxPayload))
		}
		h.render(w, r, http.StatusUnprocessableEntity, "event_new", "Send an event", eventFormData{Account: acct, Errors: []string{msg}})
		return
	}

	form := eventFormData{
		Account:        acct,
		EventType:      r.PostFormValue("event_type"),
		Payload:        r.PostFormValue("payload"),
		IdempotencyKey: r.PostFormValue("idempotency_key"),
	}
	eventType := strings.TrimSpace(form.EventType)
	if _, msg := validate.EventType(eventType); msg != "" {
		form.Errors = append(form.Errors, sentence(msg))
	}
	if _, msg := validate.Payload([]byte(form.Payload), maxPayload); msg != "" {
		form.Errors = append(form.Errors, sentence(msg))
	}
	key, _, msg := validate.IdempotencyKey(form.IdempotencyKey)
	if msg != "" {
		form.Errors = append(form.Errors, sentence(msg))
	}
	if len(form.Errors) > 0 {
		h.render(w, r, http.StatusUnprocessableEntity, "event_new", "Send an event", form)
		return
	}

	e, created, err := h.store.CreateEvent(r.Context(), acct.ID, store.NewEvent{
		EventType:      eventType,
		Payload:        []byte(form.Payload),
		IdempotencyKey: key,
	})
	if errors.Is(err, store.ErrNotFound) {
		h.notFound(w, r)
		return
	}
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	result := "existing"
	if created {
		h.signalWake()
		result = "created"
	}
	http.Redirect(w, r, eventPath(acct.ID, e.ID)+"?sent="+result, http.StatusSeeOther)
}

// signalWake tells the delivery workers that new work is waiting, without
// blocking.
func (h *Handler) signalWake() {
	if h.wake == nil {
		return
	}
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// eventData is the page of one event (R16).
type eventData struct {
	Account store.Account
	Event   store.Event
	// Path is the event page's address without the query, which the page
	// refreshes to.
	Path string
	// Payload is the payload, indented if it is valid JSON.
	Payload    string
	Deliveries []deliveryRow
	// Active is true while any delivery is not final, so the page refreshes
	// itself.
	Active bool
	// Sent is "created" right after the event was sent from the form, and
	// "existing" when the form's idempotency key returned an earlier event.
	Sent string
	// RefreshSeconds is how often the page refreshes while Active.
	RefreshSeconds int
	// RefreshURL is where the page refreshes to: Path, on the same page of
	// attempts.
	RefreshURL string
	// After is the ?after= cursor of this page of attempts, or empty on the
	// first page.
	After string
	// AttemptsNext is the cursor of the next page of attempts, or empty on
	// the last page.
	AttemptsNext string
}

// deliveryRow is the delivery of the event to one endpoint.
type deliveryRow struct {
	store.DeliveryStatus
	// URL is the endpoint's URL, or empty if the endpoint has been deleted.
	URL string
	// State is the status in words.
	State string
	// AttemptList is the delivery's attempts on this page, oldest first.
	AttemptList []store.Attempt
}

const (
	// refreshInterval is how often an event page refreshes itself while a
	// delivery is pending for its first attempt or being sent, or a retry
	// is due within refreshSoon (R15).
	refreshInterval = 2 * time.Second
	// refreshSoon is how close a retry must be due for the page to refresh
	// every refreshInterval.
	refreshSoon = 10 * time.Second
	// refreshWaiting is how often the page refreshes while every delivery
	// not final waits for a retry further off than refreshSoon.
	refreshWaiting = 30 * time.Second
	// eventAttemptsPage is the most attempts an event page shows.
	eventAttemptsPage = 200
)

// eventRefresh is how often the event page with these deliveries refreshes
// itself, or 0 once every delivery is final (R15).
func eventRefresh(deliveries []store.DeliveryStatus, now time.Time) time.Duration {
	var every time.Duration
	for _, d := range deliveries {
		switch {
		case d.Status == store.DeliveryInProgress,
			d.Status == store.DeliveryPending && (d.NextAttemptAt == nil || d.NextAttemptAt.Sub(now) <= refreshSoon):
			return refreshInterval
		case d.Status == store.DeliveryPending:
			every = refreshWaiting
		}
	}
	return every
}

// showEvent shows an event with its payload and its delivery to each target
// endpoint with its attempts, eventAttemptsPage at a time from the ?after=
// cursor (R15). While a delivery is not final, the page refreshes itself as
// eventRefresh says; it never sends the event again.
func (h *Handler) showEvent(w http.ResponseWriter, r *http.Request) {
	acct, ok := h.account(w, r)
	if !ok {
		return
	}
	e, err := h.store.GetEvent(r.Context(), acct.ID, r.PathValue("msg"))
	if !h.endpointDone(w, r, err) {
		return
	}
	data := eventData{
		Account: acct,
		Event:   e,
		Path:    eventPath(acct.ID, e.ID),
		Payload: string(e.Payload),
		After:   r.URL.Query().Get("after"),
	}
	data.RefreshURL = data.Path
	if data.After != "" {
		data.RefreshURL += "?after=" + url.QueryEscape(data.After)
	}
	if every := eventRefresh(e.Deliveries, time.Now()); every > 0 {
		data.Active = true
		data.RefreshSeconds = int(every / time.Second)
	}
	attempts, err := h.store.ListEventAttempts(r.Context(), acct.ID, e.ID, store.AttemptFilter{
		After: data.After,
		Limit: eventAttemptsPage + 1,
	})
	if !h.endpointDone(w, r, err) {
		return
	}
	if len(attempts) > eventAttemptsPage {
		attempts = attempts[:eventAttemptsPage]
		data.AttemptsNext = attempts[eventAttemptsPage-1].ID
	}
	byEndpoint := map[string][]store.Attempt{}
	for _, a := range attempts {
		byEndpoint[a.EndpointID] = append(byEndpoint[a.EndpointID], a)
	}
	var indented bytes.Buffer
	if json.Indent(&indented, bytes.TrimSpace(e.Payload), "", "  ") == nil {
		data.Payload = indented.String()
	}
	if s := r.URL.Query().Get("sent"); s == "created" || s == "existing" {
		data.Sent = s
	}
	for _, d := range e.Deliveries {
		row := deliveryRow{DeliveryStatus: d, State: strings.ReplaceAll(d.Status, "_", " "), AttemptList: byEndpoint[d.EndpointID]}
		ep, err := h.store.GetEndpoint(r.Context(), acct.ID, d.EndpointID)
		switch {
		case err == nil:
			row.URL = ep.URL
		case !errors.Is(err, store.ErrNotFound):
			h.storeError(w, r, err)
			return
		}
		data.Deliveries = append(data.Deliveries, row)
	}
	h.render(w, r, http.StatusOK, "event", "Event", data)
}

// eventPath is the address of an event's page.
func eventPath(acctID, eventID string) string {
	return "/admin/accounts/" + acctID + "/events/" + url.PathEscape(eventID)
}
