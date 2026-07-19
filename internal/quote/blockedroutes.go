// GET /api/v2/countries/blocked-routes — native go-be implementation of
// GetBlockedRoutesServicesAction. Two halves merged into one flat string list:
//
//	automated  — "no prices, no route": probe the PE with 11 default-parcel payloads per
//	             (pickup, delivery, userType) (DynamicDataPerParcelGroupProvider) and block the
//	             FE options that came back unpriced. Pure function of the PE version → cached per
//	             (pair, userType, version); optional warmer precomputes all price_engine_route
//	             pairs. php equivalent: AutomatedBlockedRoutesServices + blocked_services_on_route
//	             store (go-be replaces the store+cron with its version-keyed cache).
//	simplified — admin config from enabled_simplified_routes matched against country groups
//	             (SimplifiedBlockedRoutesServices). Pure snapshot lookup.
//
// Top clients (authenticated requests) are delegated to php — their rows are per-user.
package quote

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/eurosender/go-be/internal/delegate"
	"github.com/eurosender/go-be/internal/pe"
	"github.com/eurosender/go-be/internal/refdata"
)

// FE option strings (BlockedRoutesServiceOptions).
var automatedFrontendServices = []string{"packages", "pallets", "envelopes", "vans", "ftls", "containers"}
var simplifiedFrontendServices = []string{"air_freight", "container_by_ship", "container_by_train", "relocation_services", "car_transport", "individuals"}

// serviceTypeToFEOption (MAP_SERVICE_TYPE_TO_SERVICE; EXPRESS+envelopes handled separately).
var serviceTypeToFEOption = map[int]string{
	stSelection: "packages", stExpress: "packages", stFlexi: "packages", stRegularPlus: "packages",
	stFreight: "pallets", stFreightPriority: "pallets", stFreightPriorityExpress: "pallets",
	stVan: "vans", stFTL: "ftls", stIndividualOffer: "individuals", stContainer: "containers",
}

// TEMPORARILY_UNAVAILABLE_COUNTRIES (CountryOptions::isActive).
var unavailableCountries = setOf(54, 142, 206, 221, 3, 22, 101, 113, 177)

// blocked-routes userType values the FE sends (query param; php default 'guest').
var blockedRoutesUserTypes = []string{"guest", "person", "company"}

// probeItem describes one of the 11 PE probes (DynamicDataPerParcelGroupProvider).
type probeItem struct {
	serviceType int
	parcelType  string  // php Parcel::PARCEL_TYPE_*
	weight      float64 // DefaultParcelPropertiesSetter dims
	length      float64
	width       float64
	height      float64
	distanceKM  int // RouteDto distance (10000 default, 400 for the van probe)
}

var blockedRoutesProbes = []probeItem{
	{stSelection, "package", 1, 15, 14, 14, 10000},
	{stFlexi, "package", 1, 15, 14, 14, 10000},
	{stFreight, "euro-pallet", 240, 120, 80, 100, 10000},
	{stFreightPriority, "euro-pallet", 240, 120, 80, 100, 10000},
	{stFreightPriorityExpress, "euro-pallet", 240, 120, 80, 100, 10000},
	{stRegularPlus, "package", 1, 15, 14, 14, 10000},
	{stExpress, "package", 1, 15, 14, 14, 10000},
	{stVan, "large-van", 1000, 410, 210, 230, 400},
	{stFTL, "full-truck-load", 22000, 1360, 245, 245, 10000},
	{stContainer, "container", 22000, 610, 235, 240, 10000},
	{stExpress, "envelope", 0.5, 35, 28, 1, 10000},
}

// BlockedRoutes serves and caches the endpoint.
type BlockedRoutes struct {
	engine func() *engine // fresh engine per call (snapshot may swap)
	proxy  *delegate.Proxy

	mu    sync.Mutex
	cache map[string][]string // "version|pickup|delivery|userType" -> automated blocked list
}

// NewBlockedRoutes wires the handler; engine factory must return nil until the snapshot is ready.
func NewBlockedRoutes(engineFactory func() *engine, proxy *delegate.Proxy) *BlockedRoutes {
	return &BlockedRoutes{engine: engineFactory, proxy: proxy, cache: map[string][]string{}}
}

// Handler serves GET /api/v2/countries/blocked-routes.
func (b *BlockedRoutes) Handler(w http.ResponseWriter, r *http.Request) {
	e := b.engine()
	if e == nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	// top clients need user resolution (per-user store rows) — php owns authenticated calls
	if r.Header.Get("Authorization") != "" || r.Header.Get("x-api-key") != "" {
		if b.proxy.Enabled(delegate.ReasonAuth) {
			b.proxy.Get(w, r, r.URL.RequestURI(), delegate.ReasonAuth)
			return
		}
	}

	q := r.URL.Query()
	pickupID, _ := strconv.Atoi(q.Get("pickupCountryId"))
	deliveryID, _ := strconv.Atoi(q.Get("deliveryCountryId"))
	writeCached := func(body map[string]any) {
		w.Header().Set("Content-Type", "application/vnd.api+json")
		w.Header().Set("Vary", "Authorization")
		w.Header().Set("Cache-Control", "max-age=3600, private")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(body)
	}
	// php: empty(pickupCountryId) || empty(deliveryCountryId) → bare ApiResponse (data null)
	if pickupID == 0 || deliveryID == 0 {
		writeCached(map[string]any{"jsonApi": map[string]any{"version": "2.0"}, "data": nil})
		return
	}
	userType := q.Get("userType")
	if userType == "" {
		userType = "guest"
	}

	automated := b.automatedBlocked(r.Context(), e, pickupID, deliveryID, userType)
	simplified := simplifiedBlocked(e.snap, pickupID, deliveryID)
	merged := append(append([]string{}, automated...), simplified...)
	out := make([]any, len(merged))
	for i, s := range merged {
		out[i] = s
	}
	writeCached(map[string]any{"jsonApi": map[string]any{"version": "2.0"}, "data": out})
}

// automatedBlocked returns the "no prices, no route" half, cached per (pair, userType, version).
func (b *BlockedRoutes) automatedBlocked(ctx context.Context, e *engine, pickupID, deliveryID int, userType string) []string {
	version := e.snap.PEVersion
	if e.versionOverride != "" {
		version = e.versionOverride
	}
	key := fmt.Sprintf("%s|%d|%d|%s", version, pickupID, deliveryID, userType)
	b.mu.Lock()
	if v, ok := b.cache[key]; ok {
		b.mu.Unlock()
		return v
	}
	b.mu.Unlock()

	blocked := computeAutomatedBlocked(ctx, e, pickupID, deliveryID, userType)

	b.mu.Lock()
	b.cache[key] = blocked
	b.mu.Unlock()
	return blocked
}

// computeAutomatedBlocked ports AutomatedBlockedRoutesServices::getEnabledServicesFor +
// the AUTOMATED_FRONTEND_SERVICES diff. Batch-item isolation mirrors php's getMultiPriceBatched:
// a failed probe contributes nothing (and a 422 means "nothing priced" for that probe).
func computeAutomatedBlocked(ctx context.Context, e *engine, pickupID, deliveryID int, userType string) []string {
	enabled := map[string]bool{}
	for _, probe := range blockedRoutesProbes {
		req, err := e.buildProbeRequest(pickupID, deliveryID, userType, probe)
		if err != nil {
			continue // e.g. unknown country / no vat rate: probe contributes nothing
		}
		resp, err := e.pe.QuoteServices(ctx, req)
		if err != nil {
			continue
		}
		for _, p := range resp.Prices {
			opt, ok := serviceTypeToFEOption[p.ServiceTypeID]
			if !ok {
				continue // subtype-encoded ids map to null in php
			}
			if p.ServiceTypeID == stExpress && priceHasEnvelopes(p) {
				opt = "envelopes"
			}
			enabled[opt] = true
		}
	}
	var blocked []string
	for _, opt := range automatedFrontendServices {
		if !enabled[opt] {
			blocked = append(blocked, opt)
		}
	}
	if blocked == nil {
		blocked = []string{}
	}
	return blocked
}

// priceHasEnvelopes checks the PE-echoed parcels for envelope entries (mapToFeOptions' flag).
func priceHasEnvelopes(p pe.Price) bool {
	if len(p.Parcels) == 0 {
		return false
	}
	var parcels struct {
		Envelopes []json.RawMessage `json:"envelopes"`
	}
	if err := json.Unmarshal(p.Parcels, &parcels); err != nil {
		return false
	}
	return len(parcels.Envelopes) > 0
}

// buildProbeRequest mirrors createFromGeneralData + the PE request build for one probe item:
// default parcel of the probe's type, EUR, no zips/cities, vatRateId for today, fixed route
// distance, customerType from the userType (mapToAccountType), no courier exclusions.
func (e *engine) buildProbeRequest(pickupID, deliveryID int, userType string, probe probeItem) (*pe.Request, error) {
	vatRateID, err := vatRateIDFor(e.snap, pickupID, deliveryID, startOfDay(e.now().In(backendZone)))
	if err != nil {
		return nil, err
	}
	customerType := "guest"
	switch userType {
	case "person":
		customerType = "individual"
	case "company":
		customerType = "business"
	}
	item := parcel{
		groupID: "1", customerParcelID: "1", ptype: probe.parcelType,
		weight: probe.weight, length: probe.length, width: probe.width, height: probe.height,
		isStackable: true,
	}
	// DefaultParcelPropertiesSetter cargo defaults: container → 20GP, trucks → 13.60 LDM
	switch probe.parcelType {
	case "container":
		cpt := "20GP"
		ldm := 13.60
		item.cargoPackagingType = &cpt
		item.cargoQuantity = &ldm
	case "full-truck-load", "less-than-truck-load":
		ldm := 13.60
		item.cargoQuantity = &ldm
	}
	d := &quoteData{
		serviceType:                probe.serviceType,
		customerType:               customerType,
		pickupCountryID:            pickupID,
		deliveryCountryID:          deliveryID,
		currencyID:                 1,
		vatRateID:                  vatRateID,
		priceVersion:               e.snap.PEVersion,
		routeDistanceKM:            probe.distanceKM,
		items:                      []parcel{item},
		excludedCouriersPerService: map[int][]int{},
	}
	return e.buildPERequest(d, probe.serviceType, nil, nil, nil, false), nil
}

// simplifiedBlocked ports SimplifiedBlockedRoutesServices::fetchAndStoreBlockedRoutes.
func simplifiedBlocked(snap *refdata.Snapshot, pickupID, deliveryID int) []string {
	pickup := snap.CountriesByID[pickupID]
	delivery := snap.CountriesByID[deliveryID]
	if pickup == nil || delivery == nil {
		return []string{}
	}
	if unavailableCountries[pickupID] || unavailableCountries[deliveryID] {
		return append([]string{}, simplifiedFrontendServices...)
	}
	// group rows by service, prefiltered like the SQL (pickup_location/delivery_location must be
	// the country code or one of the group names)
	byService := map[string][]refdata.SimplifiedRoute{}
	for _, row := range snap.SimplifiedRoutes {
		if !locationSelectable(row.PickupLocation, pickup) || !locationSelectable(row.DeliveryLocation, delivery) {
			continue
		}
		byService[row.Service] = append(byService[row.Service], row)
	}
	var notEnabled []string
	for _, svc := range simplifiedFrontendServices {
		rows, ok := byService[svc]
		if !ok || !anyValidRoute(rows, pickup, delivery) {
			notEnabled = append(notEnabled, svc)
		}
	}
	if notEnabled == nil {
		notEnabled = []string{}
	}
	return notEnabled
}

// locationSelectable mirrors the SQL IN-list: exact country code or a known group name.
func locationSelectable(location string, c *refdata.Country) bool {
	switch location {
	case "Europe", "Continental Europe", "Not Continental Europe", "Europe Mainland", "Anywhere":
		return true
	}
	return location == c.Code
}

func anyValidRoute(rows []refdata.SimplifiedRoute, pickup, delivery *refdata.Country) bool {
	for _, r := range rows {
		if isValidLocation(r.PickupLocation, pickup) && isValidLocation(r.DeliveryLocation, delivery) {
			return true
		}
	}
	return false
}

// isValidLocation ports SimplifiedBlockedRoutesServices::isValidLocation.
func isValidLocation(location string, c *refdata.Country) bool {
	switch location {
	case "Anywhere":
		return true
	case "Europe":
		return c.EU
	case "Continental Europe":
		return isContinentalEurope(c)
	case "Not Continental Europe":
		return !isContinentalEurope(c)
	case "Europe Mainland":
		return isEuMainland(c)
	}
	return c.Code == location
}

// isContinentalEurope ports Countries::isContinentalEurope (realCode = first 2 chars).
func isContinentalEurope(c *refdata.Country) bool {
	real := realCountryCode(c)
	if c.EU && real != "CY" {
		return true
	}
	switch real {
	case "GB", "CH", "NO", "RS", "BA", "AL", "MK", "XK":
		return true
	}
	return false
}

// isEuMainland ports Countries::isEuMainland (full code compared: PTM/ESB excluded).
func isEuMainland(c *refdata.Country) bool {
	return c.EU && c.Code != "PTM" && c.Code != "ESB"
}

func realCountryCode(c *refdata.Country) string {
	if len(c.Code) > 2 {
		return strings.ToUpper(c.Code[:2])
	}
	return strings.ToUpper(c.Code)
}

// Warm precomputes the automated half for every price_engine_route pair × userType. Optional
// (GO_BE_BLOCKED_ROUTES_WARM=1); runs off the snapshot poller's version so a PE publish triggers
// a fresh sweep. Concurrency-bounded; the cache swap is per-entry (stale entries keep serving).
func (b *BlockedRoutes) Warm(ctx context.Context, concurrency int) {
	e := b.engine()
	if e == nil {
		return
	}
	type job struct {
		pickup, delivery int
		userType         string
	}
	var jobs []job
	for key := range e.snap.RoutesByFromTo {
		for _, ut := range blockedRoutesUserTypes {
			jobs = append(jobs, job{key[0], key[1], ut})
		}
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, j := range jobs {
		select {
		case <-ctx.Done():
			return
		default:
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(j job) {
			defer wg.Done()
			defer func() { <-sem }()
			b.automatedBlocked(ctx, e, j.pickup, j.delivery, j.userType)
		}(j)
	}
	wg.Wait()
	log.Printf("blocked-routes warm complete: %d pairs × %d user types (version %s)",
		len(e.snap.RoutesByFromTo), len(blockedRoutesUserTypes), e.snap.PEVersion)
}

// NewBlockedRoutesFromService wires the endpoint to the quote service's snapshot/PE/proxy.
func (s *Service) NewBlockedRoutesFromService() *BlockedRoutes {
	return NewBlockedRoutes(func() *engine {
		snap := s.Snapshot()
		if snap == nil {
			return nil
		}
		return newEngine(snap, s.PE, s.now, s.VersionOverride)
	}, s.Proxy)
}
