package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

// prodShape is one anonymized row extracted from prod unfinished_order_data (see
// ai/go-be/PHASE0_GATE_by_Claude.md for provenance and the extraction script).
type prodShape struct {
	T                      string                   `json:"t"`
	LoggedUser             bool                     `json:"loggedUser"`
	AccountType            *string                  `json:"accountType"`
	CurrencyCode           *string                  `json:"currencyCode"`
	SelectedServiceType    *int                     `json:"selectedServiceType"`
	SelectedServiceSubtype *string                  `json:"selectedServiceSubtype"`
	Step                   *int                     `json:"step"`
	Pickup                 shapeAddress             `json:"pickup"`
	Delivery               shapeAddress             `json:"delivery"`
	PickupDate             *string                  `json:"pickupDate"`
	Parcels                map[string][]shapeParcel `json:"parcels"`
}

type shapeAddress struct {
	Zip           *string `json:"zip"`
	City          *string `json:"city"`
	CountryID     *int    `json:"countryId"`
	Region        *string `json:"region"`
	PudoPointCode *string `json:"pudoPointCode"`
}

type shapeParcel struct {
	Weight   *float64 `json:"weight"`
	Width    *float64 `json:"width"`
	Height   *float64 `json:"height"`
	Length   *float64 `json:"length"`
	Quantity int      `json:"quantity"`
}

type shapeCase struct {
	name    string
	payload map[string]any
}

// buildShapeCases turns prod shapes into quote payloads following the real order journey's
// refinement ladder: the same customer hits /api/v2/quote ~5 times, each call carrying more
// address detail. L1 = countries only (widget), L2 = +zips, L3 = fully addressed with the
// selected serviceType and pickupDate (checkout).
func buildShapeCases(path string, maxCases int) ([]shapeCase, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var cases []shapeCase
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	i := 0
	for scanner.Scan() {
		var s prodShape
		if err := json.Unmarshal(scanner.Bytes(), &s); err != nil {
			continue
		}
		if s.Pickup.CountryID == nil || s.Delivery.CountryID == nil || len(s.Parcels) == 0 {
			continue
		}
		i++
		fullyAddressed := s.Pickup.Zip != nil && s.Pickup.City != nil && s.Delivery.Zip != nil && s.Delivery.City != nil
		// ladder sampling: every 4th shape contributes its coarse and mid rungs; every
		// fully-addressed shape contributes the full rung (the class synthetic corpora miss)
		if i%4 == 0 {
			cases = append(cases,
				shapeCase{fmt.Sprintf("prod%03d/L1-coarse", i), shapePayload(s, false, false, false)},
				shapeCase{fmt.Sprintf("prod%03d/L2-zips", i), shapePayload(s, true, false, false)},
			)
		}
		if fullyAddressed {
			cases = append(cases, shapeCase{fmt.Sprintf("prod%03d/L3-full", i), shapePayload(s, true, true, true)})
		}
		if len(cases) >= maxCases {
			break
		}
	}
	return cases, scanner.Err()
}

func shapePayload(s prodShape, withZips, withCities, withSelection bool) map[string]any {
	addr := func(a shapeAddress) map[string]any {
		out := map[string]any{
			"zip": nil, "city": nil, "street": nil, "additionalInfo": nil, "region": nil,
			"countryId": *a.CountryID, "customFields": []any{}, "comment": nil, "pudoPointCode": nil,
		}
		if withZips && a.Zip != nil {
			out["zip"] = *a.Zip
		}
		if withCities {
			if a.City != nil {
				out["city"] = *a.City
			}
			if a.Region != nil {
				out["region"] = *a.Region
			}
			if a.PudoPointCode != nil {
				out["pudoPointCode"] = *a.PudoPointCode
			}
		}
		return out
	}
	parcels := map[string]any{}
	n := 0
	for parcelType, list := range s.Parcels {
		var typed []any
		for _, p := range list {
			typed = append(typed, map[string]any{
				"parcelId": fmt.Sprintf("00000000-0000-4000-8000-%012d", n),
				"quantity": p.Quantity, "weight": p.Weight,
				"height": p.Height, "width": p.Width, "length": p.Length, "value": nil,
			})
			n++
		}
		if typed != nil {
			parcels[parcelType] = typed
		}
	}
	accountType := "person"
	if s.AccountType != nil && *s.AccountType != "" {
		accountType = *s.AccountType
	}
	currency := "EUR"
	if s.CurrencyCode != nil && *s.CurrencyCode != "" {
		currency = *s.CurrencyCode
	}
	payload := map[string]any{
		"paymentMethod":         "credit_card",
		"selectedServiceTypeId": nil,
		"serviceSubtype":        "door_to_door",
		"accountType":           accountType,
		"additionalInsuranceId": nil,
		"couponCode":            nil,
		"currencyCode":          currency,
		"parcels":               parcels,
		"shipment": map[string]any{
			"pickupAddress":   addr(s.Pickup),
			"deliveryAddress": addr(s.Delivery),
			"pickupDate":      nil,
			"addOns":          []any{},
			"value":           nil,
		},
		"unfinishedOrderUuid": nil,
	}
	if withSelection {
		if s.SelectedServiceType != nil {
			payload["selectedServiceTypeId"] = *s.SelectedServiceType
		}
		if s.SelectedServiceSubtype != nil && *s.SelectedServiceSubtype != "" {
			payload["serviceSubtype"] = *s.SelectedServiceSubtype
		}
		if s.PickupDate != nil && *s.PickupDate != "" {
			payload["shipment"].(map[string]any)["pickupDate"] = *s.PickupDate
		}
	}
	return payload
}
