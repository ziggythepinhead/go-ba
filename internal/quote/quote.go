// Package quote is go-ba's /api/v2/quote orchestrator: decode → resolve (spec 02) → PE pipeline
// (spec 03) → envelope (specs 04/07/08). The php backend is the semantic oracle; the corpus gate
// is the spec. Guest scope: no auth, no coupons, no courier tags.
package quote

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/eurosender/go-ba/internal/pe"
	"github.com/eurosender/go-ba/internal/refdata"
)

type Service struct {
	Snapshot        func() *refdata.Snapshot
	PE              *pe.Client
	VersionOverride string // dev aid: pin the PE version instead of the snapshot's (GO_BA_PE_VERSION)
	Now             func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Handle serves POST /api/v2/quote.
func (s *Service) Handle(w http.ResponseWriter, r *http.Request) {
	snap := s.Snapshot()
	if snap == nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	lang := requestLanguage(r)

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, map[string]any{
			"jsonApi": map[string]any{"version": "2.0"},
			"title":   "Bad Request",
			"detail":  err.Error(),
		})
		return
	}
	if p := checkExtraAttributes(raw); p != nil {
		writeProblem(w, p.status, p.body())
		return
	}
	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, map[string]any{
			"jsonApi": map[string]any{"version": "2.0"},
			"title":   "Bad Request",
			"detail":  err.Error(),
		})
		return
	}
	trimRequestStrings(&req)
	if p := validateRequest(&req); p != nil {
		writeProblem(w, p.status, p.body())
		return
	}

	e := newEngine(snap, s.PE, s.now, s.VersionOverride)
	body, status := s.quote(r.Context(), e, &req, lang)
	w.Header().Set("Content-Type", "application/vnd.api+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// requestLanguage: exact-match Accept-Language against the 14 supported codes, else en.
func requestLanguage(r *http.Request) string {
	al := r.Header.Get("Accept-Language")
	if supportedLanguages[al] {
		return al
	}
	return "en"
}

func writeProblem(w http.ResponseWriter, status int, body map[string]any) {
	w.Header().Set("Content-Type", "application/api-problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// quote runs the full php QuoteAction flow. Returns (body, http status).
func (s *Service) quote(ctx context.Context, e *engine, req *Request, lang string) (map[string]any, int) {
	d, err := resolve(req, e.snap, e.now)
	if err != nil {
		// blanket catch(Exception) → 422 {jsonApi, title: message} (spec 08 §1.3 producer 1)
		return map[string]any{
			"jsonApi": map[string]any{"version": "2.0"},
			"title":   err.Error(),
		}, http.StatusUnprocessableEntity
	}

	r := e.getDynamicPrice(ctx, d)

	// courierTag fallback (§6.1) — guests can never trigger it (tag+guest 422s in validation);
	// kept for shape completeness once auth lands.

	// EXPRESS→FREIGHT probe (spec 08 §6.2)
	if !r.success && d.containsPallets() &&
		(req.SelectedServiceTypeID == nil || *req.SelectedServiceTypeID != stIndividualOffer) &&
		d.serviceType == stExpress {
		fd := *d
		fd.serviceType = stFreight
		fr := e.getDynamicPrice(ctx, &fd)
		if fr.success {
			r.alternatives = []altService{altFromResp(fr)}
			// freight insurances keyed 04-door_to_door — Round C
		}
	}

	body := e.buildEnvelope(d, r, lang, req.SelectedServiceTypeID != nil)
	return body, http.StatusOK
}
