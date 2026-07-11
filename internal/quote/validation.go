// Request decode validation: ports the active-at-decode Symfony constraints (spec 01).
// Corpus-relevant scope: recursive string trim, ALLOW_EXTRA_ATTRIBUTES=false for van/truck
// parcels (400), NotBlank(allowNull) on trimmed-empty address fields (422 invalid-params).
// Exotic constraints (Choice lists, Latin regex, dimension GreaterThanOrEqual) are ported where
// the corpus exercises them; the rest are documented in spec 01 for later.
package quote

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// notBlankCode is Symfony NotBlank::IS_BLANK_ERROR.
const notBlankCode = "c1051bb4-d103-4f74-8988-acbcafc7fdc3"

type invalidParam struct {
	Name      string  `json:"name"`
	Reason    string  `json:"reason"`
	RowNumber *int    `json:"row_number"`
	Code      *string `json:"code"`
}

// problem is a validation outcome: either a 400 (detail) or a 422 (invalid-params).
type problem struct {
	status int
	title  string
	detail string
	params []invalidParam
}

func (p *problem) body() map[string]any {
	out := map[string]any{
		"jsonApi": map[string]any{"version": "2.0"},
		"title":   p.title,
	}
	if p.detail != "" {
		out["detail"] = p.detail
	}
	if len(p.params) > 0 {
		arr := make([]any, 0, len(p.params))
		for _, ip := range p.params {
			var code any
			if ip.Code != nil {
				code = *ip.Code
			}
			arr = append(arr, map[string]any{
				"name": ip.Name, "reason": ip.Reason, "row_number": nil, "code": code,
			})
		}
		out["invalid-params"] = arr
	}
	return out
}

var quoteVanAllowed = setOf2("parcelId", "type", "value", "content", "optionalServices", "bulkImportFileLineNumber")
var quoteTruckAllowed = setOf2("parcelId", "type", "weight", "value", "notes", "cargoPackagingType", "euroPalletQuantity", "loadingMeters", "bulkImportFileLineNumber")

// checkExtraAttributes ports the serializer's ALLOW_EXTRA_ATTRIBUTES=false for the DTOs that
// don't accept dimension fields (vans, trucks) → 400 BadRequest with the sorted unknown list.
func checkExtraAttributes(raw []byte) *problem {
	var tree struct {
		Parcels map[string]json.RawMessage `json:"parcels"`
	}
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil
	}
	check := func(listRaw json.RawMessage, allowed map[string]bool) *problem {
		var list []map[string]json.RawMessage
		if err := json.Unmarshal(listRaw, &list); err != nil {
			return nil
		}
		for _, obj := range list {
			var unknown []string
			for k := range obj {
				if !allowed[k] {
					unknown = append(unknown, k)
				}
			}
			if len(unknown) > 0 {
				sort.Strings(unknown)
				quoted := make([]string, len(unknown))
				for i, u := range unknown {
					quoted[i] = fmt.Sprintf("%q", u)
				}
				return &problem{
					status: 400, title: "Bad Request",
					detail: fmt.Sprintf("Extra attributes are not allowed (%s are unknown).", strings.Join(quoted, ", ")),
				}
			}
		}
		return nil
	}
	if v, ok := tree.Parcels["vans"]; ok {
		if p := check(v, quoteVanAllowed); p != nil {
			return p
		}
	}
	if v, ok := tree.Parcels["trucks"]; ok {
		if p := check(v, quoteTruckAllowed); p != nil {
			return p
		}
	}
	return nil
}

// trimRequestStrings ports the serializer's recursive trim on decoded strings.
func trimRequestStrings(req *Request) {
	trimP := func(s *string) *string {
		if s == nil {
			return nil
		}
		t := strings.TrimSpace(*s)
		return &t
	}
	for _, a := range []*ReqAddress{&req.Shipment.PickupAddress, &req.Shipment.DeliveryAddress} {
		a.Zip = trimP(a.Zip)
		a.City = trimP(a.City)
		a.Street = trimP(a.Street)
		a.Region = trimP(a.Region)
		a.PudoPointCode = trimP(a.PudoPointCode)
	}
	req.CouponCode = trimP(req.CouponCode)
	req.CourierTag = trimP(req.CourierTag)
}

// validateRequest ports the group-quotes validation pass (corpus-relevant constraints).
// Returns nil when valid.
func validateRequest(req *Request) *problem {
	var params []invalidParam
	code := notBlankCode
	blank := func(name string, v *string) {
		if v != nil && *v == "" {
			params = append(params, invalidParam{Name: name, Reason: "This field is mandatory.", Code: &code})
		}
	}
	// PostalAddress property order: countryId, zip, city, street, additionalInfo, region;
	// pickup address validates before delivery (QuoteShipment declaration order).
	blank("shipment.pickupAddress.zip", req.Shipment.PickupAddress.Zip)
	blank("shipment.pickupAddress.city", req.Shipment.PickupAddress.City)
	blank("shipment.pickupAddress.street", req.Shipment.PickupAddress.Street)
	blank("shipment.pickupAddress.region", req.Shipment.PickupAddress.Region)
	blank("shipment.deliveryAddress.zip", req.Shipment.DeliveryAddress.Zip)
	blank("shipment.deliveryAddress.city", req.Shipment.DeliveryAddress.City)
	blank("shipment.deliveryAddress.street", req.Shipment.DeliveryAddress.Street)
	blank("shipment.deliveryAddress.region", req.Shipment.DeliveryAddress.Region)

	if len(params) > 0 {
		return &problem{status: 422, title: "Incorrect object parameters.", params: params}
	}
	return nil
}
