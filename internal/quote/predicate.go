// Delegation predicate: which requests go-be does NOT serve natively (yet) and forwards to the
// php backend instead. Each reason is metric-counted; the set shrinks as ports land.
package quote

import (
	"net/http"

	"github.com/eurosender/go-be/internal/delegate"
)

// delegationReason returns the first matching pre-pipeline delegation reason, or "".
func delegationReason(req *Request, hdr http.Header) string {
	if hdr.Get("Authorization") != "" || hdr.Get("x-api-key") != "" {
		return delegate.ReasonAuth
	}
	if req.CouponCode != nil && *req.CouponCode != "" {
		return delegate.ReasonCoupon
	}
	if len(req.Parcels.Vans) > 0 || len(req.Parcels.Trucks) > 0 || req.RouteDistance != nil {
		return delegate.ReasonVanFTL
	}
	if req.SelectedServiceTypeID != nil {
		switch *req.SelectedServiceTypeID {
		case stVan, stFTL, stContainer, stRail:
			return delegate.ReasonVanFTL
		}
	}
	if req.CourierID != nil {
		return delegate.ReasonCourierPin
	}
	if req.Source != nil && *req.Source != "" && *req.Source != "website" && *req.Source != "app" {
		return delegate.ReasonSource
	}
	if req.Shipment.Value != nil && *req.Shipment.Value > 0 {
		return delegate.ReasonValue
	}
	for _, lists := range [][]ReqParcel{req.Parcels.Packages, req.Parcels.Envelopes} {
		for _, p := range lists {
			if p.Value != nil && *p.Value > 0 {
				return delegate.ReasonValue
			}
		}
	}
	for _, p := range req.Parcels.Pallets {
		if p.Value != nil && *p.Value > 0 {
			return delegate.ReasonValue
		}
	}
	return ""
}

// fedexAddressedReason is the post-pipeline check: fully-addressed request whose priced result
// involves a FedEx-group courier — php runs live FedEx availability/IPE checks there that go-be
// does not replicate. Returns "" when the native answer is safe.
func fedexAddressedReason(d *quoteData, r *peResp) string {
	fullyAddressed := nonEmpty(d.pickupZip) && nonEmpty(d.pickupCity) && nonEmpty(d.deliveryZip) && nonEmpty(d.deliveryCity)
	if !fullyAddressed {
		return ""
	}
	if r.courierID != nil && fedexGroup[*r.courierID] {
		return delegate.ReasonFedexAddressed
	}
	for _, a := range r.alternatives {
		if a.courierID != nil && fedexGroup[*a.courierID] {
			return delegate.ReasonFedexAddressed
		}
	}
	return ""
}

func nonEmpty(s *string) bool { return s != nil && *s != "" }
