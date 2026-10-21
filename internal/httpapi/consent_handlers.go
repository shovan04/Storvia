package httpapi

import (
	"net/http"

	"github.com/shovan04/storvia-project/internal/storage/service"
)

// ConsentHandler exposes the tenant's outage-fallback consent.
//
// The disclosure itself is not served from here. It is a document a tenant has to be
// shown and agree to, and a client that wants to render it should link to whatever
// copy the deployment publishes; the endpoint reports the version so a client can
// tell whether its cached copy is still the one that was agreed to.
type ConsentHandler struct {
	svc *service.ConsentService
}

// NewConsentHandler builds the handler.
func NewConsentHandler(svc *service.ConsentService) *ConsentHandler {
	return &ConsentHandler{svc: svc}
}

// Show returns the tenant's current consent state.
func (h *ConsentHandler) Show(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFrom(r)
	if err != nil {
		WriteProblem(w, r, err)
		return
	}

	view, err := h.svc.Show(r.Context(), scope)
	if err != nil {
		WriteProblem(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, view)
}

// Accept records the tenant's acceptance of the current disclosure.
func (h *ConsentHandler) Accept(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFrom(r)
	if err != nil {
		WriteProblem(w, r, err)
		return
	}

	view, err := h.svc.Accept(r.Context(), scope)
	if err != nil {
		WriteProblem(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, view)
}

// Withdraw records the tenant's refusal and closes the gate immediately.
//
// Withdrawal matters more than acceptance: it is the one direction that has to work
// the moment the tenant asks, because it stops their files being routed anywhere new.
func (h *ConsentHandler) Withdraw(w http.ResponseWriter, r *http.Request) {
	scope, err := scopeFrom(r)
	if err != nil {
		WriteProblem(w, r, err)
		return
	}

	view, err := h.svc.Withdraw(r.Context(), scope)
	if err != nil {
		WriteProblem(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, view)
}
