// Package quote is go-ba's /api/v2/quote orchestrator (Phase 2 skeleton): decode → resolve from
// the reference snapshot → ONE pe bulk call → assemble the response envelope. Fields not yet
// ported are emitted as nulls/empties; the corpus gate's divergence hotspots are the Phase-3
// backlog, in priority order.
package quote

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/eurosender/go-ba/internal/pe"
	"github.com/eurosender/go-ba/internal/refdata"
)

// backendZone mirrors the php backend's server timezone (Carbon::today() renders pickup dates in
// it, e.g. 2026-07-17T00:00:00+02:00).
var backendZone = mustZone("Europe/Ljubljana")

func mustZone(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return l
}

// PE type ids (PriceEngineServiceTypeOptions) for the packages provider set.
var (
	packagesSelectedPEType  = 2 // FLEXI door_to_door
	packagesOptionalPETypes = []int{1, 101, 102, 103, 11, 1101, 1102, 1103, 8, 801, 802, 803, 201, 202, 203}
	// all 19 PE types the packages+pallets providers exclude couriers for (excludedCourierIds keys)
	allPETypeKeys = []string{"1", "101", "102", "103", "11", "1101", "1102", "1103", "12", "13", "2", "201", "202", "203", "4", "8", "801", "802", "803"}
)

// peTypeToEurosender collapses PE subtype ids to eurosender serviceType ids (PriceEngineServiceTypeMapper).
var peTypeToEurosender = map[int]int{
	1: 1, 101: 1, 102: 1, 103: 1,
	2: 2, 201: 2, 202: 2, 203: 2,
	11: 11, 1101: 11, 1102: 11, 1103: 11,
	8: 8, 801: 8, 802: 8, 803: 8,
	4: 4, 12: 12, 13: 13, 9: 9, 10: 10, 6: 6, 7: 7, 14: 14,
}

var peTypeToSubtype = map[int]string{
	1: "door_to_door", 2: "door_to_door", 8: "door_to_door", 11: "door_to_door",
	4: "door_to_door", 12: "door_to_door", 13: "door_to_door", 9: "door_to_door",
	10: "door_to_door", 6: "door_to_door", 7: "door_to_door", 14: "door_to_door",
	101: "door_to_shop", 201: "door_to_shop", 801: "door_to_shop", 1101: "door_to_shop",
	102: "shop_to_door", 202: "shop_to_door", 802: "shop_to_door", 1102: "shop_to_door",
	103: "shop_to_shop", 203: "shop_to_shop", 803: "shop_to_shop", 1103: "shop_to_shop",
}

// Request is the /api/v2/quote body (guest package scope for the skeleton).
type Request struct {
	AccountType  string  `json:"accountType"`
	CurrencyCode string  `json:"currencyCode"`
	Parcels      struct {
		Packages []struct {
			ParcelID string  `json:"parcelId"`
			Quantity int     `json:"quantity"`
			Weight   float64 `json:"weight"`
			Height   float64 `json:"height"`
			Width    float64 `json:"width"`
			Length   float64 `json:"length"`
		} `json:"packages"`
	} `json:"parcels"`
	Shipment struct {
		PickupAddress   reqAddress `json:"pickupAddress"`
		DeliveryAddress reqAddress `json:"deliveryAddress"`
		PickupDate      *string    `json:"pickupDate"`
	} `json:"shipment"`
}

type reqAddress struct {
	Zip       *string `json:"zip"`
	City      *string `json:"city"`
	CountryID int     `json:"countryId"`
}

type Service struct {
	Snapshot  func() *refdata.Snapshot
	PE        *pe.Client
	VersionOverride string // dev aid: pin the PE version instead of the snapshot's (GO_BA_PE_VERSION)
}

// Handle serves POST /api/v2/quote.
func (s *Service) Handle(w http.ResponseWriter, r *http.Request) {
	snap := s.Snapshot()
	if snap == nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	var req Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	resp, err := s.quote(r.Context(), snap, &req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Service) quote(ctx context.Context, snap *refdata.Snapshot, req *Request) (map[string]any, error) {
	pickup, ok := snap.CountriesByID[req.Shipment.PickupAddress.CountryID]
	if !ok {
		return nil, fmt.Errorf("unknown pickup country %d", req.Shipment.PickupAddress.CountryID)
	}
	delivery, ok := snap.CountriesByID[req.Shipment.DeliveryAddress.CountryID]
	if !ok {
		return nil, fmt.Errorf("unknown delivery country %d", req.Shipment.DeliveryAddress.CountryID)
	}

	pickupDate := defaultPickupDate(snap, pickup.ID)
	peReq := s.buildPERequest(snap, req, pickup, delivery, pickupDate)

	var prices []pe.Price
	peResp, err := s.PE.QuoteServices(ctx, peReq)
	switch err.(type) {
	case nil:
		prices = peResp.Prices
	case *pe.NoPriceError:
		// php parity: a whole-quote 422 contributes zero prices, response still assembles
	default:
		return nil, err
	}

	return s.assemble(snap, req, pickup, delivery, prices), nil
}

func (s *Service) buildPERequest(snap *refdata.Snapshot, req *Request, pickup, delivery *refdata.Country, pickupDate time.Time) *pe.Request {
	version := snap.PEVersion
	if s.VersionOverride != "" {
		version = s.VersionOverride
	}
	excluded := make(map[string][]int, len(allPETypeKeys))
	for _, k := range allPETypeKeys {
		excluded[k] = []int{}
	}
	var parcels []pe.Parcel
	for _, p := range req.Parcels.Packages {
		parcels = append(parcels, pe.Parcel{
			Type: "package", Weight: p.Weight, Length: p.Length, Width: p.Width, Height: p.Height,
			GroupID: p.ParcelID, Stackable: true, Quantity: p.Quantity,
		})
	}
	dateYMD := pickupDate.Format("2006-01-02")
	var onHoliday []string
	for _, h := range snap.HolidaysByDate[dateYMD] {
		if c := snap.CountriesByID[h.PickupCountryID]; c != nil {
			onHoliday = append(onHoliday, c.Code)
		}
	}
	if onHoliday == nil {
		onHoliday = []string{}
	}
	year := time.Now().In(backendZone).Format("2006")
	today := time.Now().In(backendZone).Format("2006-01-02")
	holidaysOnPickup := []string{}
	for _, h := range snap.HolidaysByCountry[pickup.ID] {
		// FIND_ALL_ACTIVE_FOR_CURRENT_YEAR: >= today AND same calendar year
		if h.HolidayDate >= today && h.HolidayDate[:4] == year {
			holidaysOnPickup = append(holidaysOnPickup, h.HolidayDate)
		}
	}
	sort.Strings(holidaysOnPickup)

	return &pe.Request{
		Version:              version,
		Parcels:              pe.Parcels{AllParcels: parcels, Packages: parcels, Envelopes: []pe.Parcel{}, Pallets: []pe.Parcel{}, Vans: []pe.Parcel{}, Trucks: []pe.Parcel{}, NonStandard: []pe.Parcel{}, Containers: []pe.Parcel{}},
		Client:               pe.ClientInfo{AccountType: "guest"},
		ExcludedCourierIDs:   excluded,
		PickupDate:           pickupDate.Format("2006-01-02T15:04:05-07:00"),
		SelectedServiceType:  packagesSelectedPEType,
		OptionalServiceTypes: packagesOptionalPETypes,
		Route: pe.Route{
			PickupAddress:   pe.Address{Zip: req.Shipment.PickupAddress.Zip, City: req.Shipment.PickupAddress.City, CountryID: pickup.ID, TimeZoneName: pickup.TimeZone, Country2IsoCode: pickup.Code},
			DeliveryAddress: pe.Address{Zip: req.Shipment.DeliveryAddress.Zip, City: req.Shipment.DeliveryAddress.City, CountryID: delivery.ID, Country2IsoCode: delivery.Code},
			IsEU:            pickup.EU && delivery.EU,
		},
		CountriesOnHolidayForPickup: onHoliday,
		HolidaysOnPickupCountry:     holidaysOnPickup,
		Tags:                        []string{},
		SpecificCourierIDs:          []int{},
	}
}

// defaultPickupDate ports DefaultPickupDateProvider::getForPickupCountryId: pickup = today+2
// weekdays, ordering = today+1 weekday; while either lands on an active holiday in ANY courier
// country (or the pickup country), both step one weekday forward.
func defaultPickupDate(snap *refdata.Snapshot, pickupCountryID int) time.Time {
	holidayCountries := map[int]bool{pickupCountryID: true}
	for _, id := range snap.CourierCountryIDs {
		holidayCountries[id] = true
	}
	isHoliday := func(t time.Time) bool {
		for _, h := range snap.HolidaysByDate[t.Format("2006-01-02")] {
			if holidayCountries[h.PickupCountryID] {
				return true
			}
		}
		return false
	}
	today := time.Now().In(backendZone)
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, backendZone)
	pickupDate := addWeekdays(today, 2)
	orderingDate := addWeekdays(today, 1)
	for isHoliday(pickupDate) || isHoliday(orderingDate) {
		pickupDate = addWeekdays(pickupDate, 1)
		orderingDate = addWeekdays(orderingDate, 1)
	}
	return pickupDate
}

// addWeekdays mirrors Carbon::addWeekdays: step forward n times, skipping Saturday/Sunday.
func addWeekdays(t time.Time, n int) time.Time {
	for i := 0; i < n; i++ {
		t = t.AddDate(0, 0, 1)
		for t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
			t = t.AddDate(0, 0, 1)
		}
	}
	return t
}

// assemble builds the response envelope. Skeleton scope: serviceTypes carry the price-bearing
// fields; everything not yet ported is null/empty and will surface in the corpus gate's hotspots.
func (s *Service) assemble(snap *refdata.Snapshot, req *Request, pickup, delivery *refdata.Country, prices []pe.Price) map[string]any {
	serviceTypes := make([]any, 0, len(prices))
	for _, p := range prices {
		esType := peTypeToEurosender[p.ServiceTypeID]
		subtype := peTypeToSubtype[p.ServiceTypeID]
		var tcLink any
		if t := snap.ActiveTermsByCourier[p.CourierID]; t != nil {
			tcLink = t.Link
		}
		serviceTypes = append(serviceTypes, map[string]any{
			"id":             esType,
			"serviceSubtype": subtype,
			"serviceNameKey": fmt.Sprintf("%02d-%s", esType, subtype),
			"minPickupDate":  nil, // Phase 3: PickupDateService port
			"usedPickupDate": p.PickupDate,
			"isCallRequired": false,
			"isLabelRequired": true,
			"edt":            nil, // Phase 3: EstimatedDeliveryTimeProvider (NOT pe estimatedDeliveryTime)
			"edtDateFrom":    nil,
			"edtDateTo":      nil,
			"price": map[string]any{
				"original":  map[string]any{"currencyCode": "EUR", "gross": p.CourierPrice.Price.TotalPrice, "net": p.CourierPrice.Price.TotalPrice},
				"converted": nil,
			},
			"pickupDateFee":                    nil,
			"basicInsurance":                   nil, // Phase 3: BasicInsuranceProvider
			"additionalInsurances":             []any{},
			"recommendedAdditionalInsuranceId": nil,
			"pickupExcludedDates":              []any{}, // Phase 3: PickupExcludedDaysProvider port
			"addOns":                           []any{},
			"courierTermsAndConditionsLink":    tcLink,
			"upgradePaths":                     []any{},
			"courierId":                        p.CourierID,
			"requiresShipmentValue":            true,
			"isPickupSenderAddressRequired":    false,
			"isDeliveryRecipientAddressRequired": false,
			"pickupTimeFrameSelectionPossible": false,
			"otherPickupDatePrices":            []any{},
		})
	}
	var generalTC any
	if snap.ActiveTerms != nil {
		generalTC = snap.ActiveTerms.Link
	}
	return map[string]any{
		"jsonApi": map[string]any{"version": "2.0"},
		"data": map[string]any{
			"options": map[string]any{
				"paymentMethods":              []any{}, // Phase 3
				"parcelLevelOptionalServices": []any{},
				"parcelTransportTypePrices":   []any{},
				"serviceTypes":                serviceTypes,
				"pricePerKm":                  nil,
				"truckOptions":                nil,
				"vatRate":                     nil, // Phase 3: VatCalculator port
				"exchangeRate":                nil,
				"generalTermsAndConditionsLink": generalTC,
				"isGlobalRoute":               !(pickup.EU && delivery.EU),
			},
			"order":    nil,
			"warnings": []any{},
		},
	}
}
