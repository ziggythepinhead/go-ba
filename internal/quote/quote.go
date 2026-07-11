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

	"github.com/eurosender/go-ba/internal/delegate"
	"github.com/eurosender/go-ba/internal/pe"
	"github.com/eurosender/go-ba/internal/refdata"
)

type Service struct {
	Snapshot        func() *refdata.Snapshot
	PE              *pe.Client
	VersionOverride string // dev aid: pin the PE version instead of the snapshot's (GO_BA_PE_VERSION)
	Now             func() time.Time
	// Proxy delegates non-native request classes (and native failures) to the php backend.
	// nil = delegation off (dev/corpus mode): every request is served natively.
	Proxy *delegate.Proxy
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

	// Loop guard: a request we ourselves delegated must never come back.
	if r.Header.Get(delegate.LoopGuardHeader) != "" {
		delegate.CountLoop()
		writeProblem(w, http.StatusLoopDetected, map[string]any{
			"jsonApi": map[string]any{"version": "2.0"},
			"title":   "Loop Detected",
		})
		return
	}

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, map[string]any{
			"jsonApi": map[string]any{"version": "2.0"},
			"title":   "Bad Request",
			"detail":  err.Error(),
		})
		return
	}

	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		// undecodable body: php owns the exact error shape when delegation is on
		if s.Proxy.Enabled(delegate.ReasonError) && s.Proxy.Quote(w, r, raw, delegate.ReasonError) {
			return
		}
		writeProblem(w, http.StatusBadRequest, map[string]any{
			"jsonApi": map[string]any{"version": "2.0"},
			"title":   "Bad Request",
			"detail":  err.Error(),
		})
		return
	}

	// Pre-pipeline delegation predicate (auth, coupons, van/FTL, values, sources, courier pins).
	if reason := delegationReason(&req, r.Header); reason != "" {
		if s.Proxy.Enabled(reason) {
			s.Proxy.Quote(w, r, raw, reason)
			return
		}
		delegate.Count(reason) // count even when delegation is off (dev/corpus mode)
	}

	if p := checkExtraAttributes(raw); p != nil {
		writeProblem(w, p.status, p.body())
		return
	}
	trimRequestStrings(&req)
	if p := validateRequest(&req); p != nil {
		writeProblem(w, p.status, p.body())
		return
	}

	// Native path with a catch-all fallback: on panic or internal error the request is
	// delegated instead of ever serving a wrong/500 answer.
	body, status, delegReason := func() (body map[string]any, status int, delegReason string) {
		defer func() {
			if rec := recover(); rec != nil {
				body, status, delegReason = nil, 0, delegate.ReasonError
			}
		}()
		e := newEngine(snap, s.PE, s.now, s.VersionOverride)
		return s.quote(r.Context(), e, &req, lang)
	}()

	if delegReason != "" && s.Proxy.Enabled(delegReason) {
		s.Proxy.Quote(w, r, raw, delegReason)
		return
	}
	if body == nil { // native failure and no proxy available
		delegate.Count(delegate.ReasonError)
		writeProblem(w, http.StatusInternalServerError, map[string]any{
			"jsonApi": map[string]any{"version": "2.0"},
			"title":   "Internal Server Error",
		})
		return
	}
	if delegReason != "" {
		delegate.Count(delegReason) // proxy off: count and serve the native answer (dev/corpus mode)
	}
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

// quote runs the full php QuoteAction flow. Returns (body, http status, delegation reason).
// A non-empty delegation reason means the native answer must not be served when a proxy exists
// (php runs live checks go-ba does not replicate for that request class).
func (s *Service) quote(ctx context.Context, e *engine, req *Request, lang string) (map[string]any, int, string) {
	d, err := resolve(req, e.snap, e.now)
	if err != nil {
		// blanket catch(Exception) → 422 {jsonApi, title: message} (spec 08 §1.3 producer 1)
		return map[string]any{
			"jsonApi": map[string]any{"version": "2.0"},
			"title":   err.Error(),
		}, http.StatusUnprocessableEntity, ""
	}

	r := e.getDynamicPrice(ctx, d)

	// courierTag fallback (§6.1) — guests can never trigger it (tag+guest 422s in validation);
	// authed requests are delegated wholesale.

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

	// Post-pipeline predicate: fully-addressed + FedEx-priced → php owns the answer (live
	// FedEx availability filter + IPE addon).
	delegReason := fedexAddressedReason(d, r)

	body := e.buildEnvelope(d, r, lang, req.SelectedServiceTypeID != nil)
	return body, http.StatusOK, delegReason
}
