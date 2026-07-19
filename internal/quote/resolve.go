// Resolve stage: request DTO → quoteData (php DynamicPriceData). Ports
// DynamicPriceDataFactory::createFromRequest, Parcels::createFromQuoteParcels,
// ServiceTypeDetector, DimensionsChecker, VatCalculator/VatLocationMatrix. Spec: 02.
package quote

import (
	"fmt"
	"strings"
	"time"

	"github.com/eurosender/go-be/internal/refdata"
)

// Request is the full /api/v2/quote body.
type Request struct {
	PaymentMethod         *string    `json:"paymentMethod"`
	SelectedServiceTypeID *int       `json:"selectedServiceTypeId"`
	ServiceSubtype        *string    `json:"serviceSubtype"`
	AccountType           string     `json:"accountType"`
	AdditionalInsuranceID *int       `json:"additionalInsuranceId"`
	CouponCode            *string    `json:"couponCode"`
	CurrencyCode          string     `json:"currencyCode"`
	Parcels               ReqParcels `json:"parcels"`
	Shipment              struct {
		PickupAddress   ReqAddress `json:"pickupAddress"`
		DeliveryAddress ReqAddress `json:"deliveryAddress"`
		PickupDate      *string    `json:"pickupDate"`
		AddOns          []string   `json:"addOns"`
		Value           *float64   `json:"value"`
	} `json:"shipment"`
	UnfinishedOrderUUID   *string  `json:"unfinishedOrderUuid"`
	Source                *string  `json:"source"`
	CourierTag            *string  `json:"courierTag"`
	CourierID             *int     `json:"courierId"`
	PreferredCouriersOnly bool     `json:"preferredCouriersOnly"`
	RouteDistance         *float64 `json:"routeDistance"`
}

type ReqParcels struct {
	Packages  []ReqParcel `json:"packages"`
	Envelopes []ReqParcel `json:"envelopes"`
	Pallets   []ReqPallet `json:"pallets"`
	Vans      []ReqVan    `json:"vans"`
	Trucks    []ReqTruck  `json:"trucks"`
}

type ReqParcel struct {
	ParcelID string   `json:"parcelId"`
	Quantity *int     `json:"quantity"`
	Weight   *float64 `json:"weight"`
	Height   *float64 `json:"height"`
	Width    *float64 `json:"width"`
	Length   *float64 `json:"length"`
	Value    *float64 `json:"value"`
	Content  *string  `json:"content"`
}

type ReqPallet struct {
	ReqParcel
	IsStackable *bool `json:"isStackable"`
}

type ReqVan struct {
	ParcelID         string   `json:"parcelId"`
	Type             string   `json:"type"` // small-van | large-van
	Value            *float64 `json:"value"`
	Content          *string  `json:"content"`
	OptionalServices []int    `json:"optionalServices"`
}

type ReqTruck struct {
	ParcelID           string   `json:"parcelId"`
	Type               *string  `json:"type"`
	Weight             *float64 `json:"weight"`
	Value              *float64 `json:"value"`
	CargoPackagingType *string  `json:"cargoPackagingType"`
	EuroPalletQuantity *int     `json:"euroPalletQuantity"`
	LoadingMeters      *float64 `json:"loadingMeters"`
}

type ReqAddress struct {
	Zip           *string `json:"zip"`
	City          *string `json:"city"`
	Street        *string `json:"street"`
	Region        *string `json:"region"`
	CountryID     int     `json:"countryId"`
	PudoPointCode *string `json:"pudoPointCode"`
	TimeZoneName  *string `json:"timeZoneName"`
}

// parcel is the expanded flat item (php Parcel).
type parcel struct {
	groupID               string // expanded id (with #i suffix for quantity>1)
	customerParcelID      string
	ptype                 string // package|pallet|envelope|small-van|large-van|full-truck-load|less-than-truck-load|non-standard
	weight                float64
	length, width, height float64
	value                 *float64
	optionalServices      []int
	isStackable           bool
	cargoQuantity         *float64
	cargoPackagingType    *string
}

// quoteData is the php DynamicPriceData equivalent (guest scope).
type quoteData struct {
	serviceType                    int
	serviceTypeIsDetected          bool
	serviceSubtypeRaw              *string // raw request value (may be nil); getter defaults door_to_door
	paymentType                    string
	insuranceID                    *int
	customerType                   string // guest|business
	accountType                    string // request accountType (guest path)
	pickupCountryID                int
	deliveryCountryID              int
	pickupRegionID                 *int
	deliveryRegionID               *int
	pickupZip, pickupCity          *string
	deliveryZip, deliveryCity      *string
	currencyID                     int
	couponCode                     *string
	hasFlexibleBooking             bool
	extraIDs                       []int
	items                          []parcel
	pickupDate                     *time.Time // request pickupDate (startOfDay, backend zone); nil = default
	pickupTimeZone                 *time.Location
	vatRateID                      int
	priceVersion                   string
	source                         string
	useRequestedServiceTypeOnly    bool
	useRequestedServiceSubtypeOnly bool
	valuePerShipment               *float64
	optionalServiceTypes           []int
	courierIDs                     []int
	excludedCouriersPerService     map[int][]int // guest: all empty
	routeDistanceKM                int           // RouteDto distance (0 on the quote path; blocked-routes probes set it)

	// derived flags
	isGlobalRouteFlag bool
}

func (d *quoteData) getServiceSubtype() string {
	if d.serviceSubtypeRaw != nil && *d.serviceSubtypeRaw != "" {
		return *d.serviceSubtypeRaw
	}
	return subD2D
}

func (d *quoteData) getTotalValue() float64 {
	if d.valuePerShipment != nil {
		return *d.valuePerShipment
	}
	v := 0.0
	for _, p := range d.items {
		if p.value != nil {
			v += *p.value
		}
	}
	return v
}

func (d *quoteData) getMaxParcelValue() float64 {
	max := 0.0
	for _, p := range d.items {
		v := 0.0
		if p.value != nil {
			v = *p.value
		}
		if v > max {
			max = v
		}
	}
	return max
}

func (d *quoteData) containsPallets() bool {
	for _, p := range d.items {
		if p.ptype == "pallet" || p.ptype == "euro-pallet" {
			return true
		}
	}
	return false
}

func (d *quoteData) containsOnlyPallets() bool {
	n := 0
	for _, p := range d.items {
		if p.ptype == "pallet" || p.ptype == "euro-pallet" {
			n++
		}
	}
	return n > 0 && n == len(d.items)
}

func (d *quoteData) containsOnlyPackages() bool {
	n := 0
	for _, p := range d.items {
		if p.ptype == "package" {
			n++
		}
	}
	return n > 0 && n == len(d.items)
}

func (d *quoteData) containsVan() bool {
	for _, p := range d.items {
		if p.ptype == "small-van" || p.ptype == "large-van" {
			return true
		}
	}
	return false
}

func (d *quoteData) containsTrucks() bool {
	for _, p := range d.items {
		if p.ptype == "full-truck-load" || p.ptype == "less-than-truck-load" {
			return true
		}
	}
	return false
}

func (d *quoteData) containsOnlyEnvelopes() bool {
	n := 0
	for _, p := range d.items {
		if p.ptype == "envelope" {
			n++
		}
	}
	return n > 0 && n == len(d.items)
}

func (d *quoteData) containsEnvelopes() bool {
	for _, p := range d.items {
		if p.ptype == "envelope" {
			return true
		}
	}
	return false
}

// invalidParcelError mirrors InvalidParcelDataException → blanket 422 {title: message}.
type invalidParcelError struct{ msg string }

func (e *invalidParcelError) Error() string { return e.msg }

// EN messages for the detector errors (Lang::t of api.check_dimensions/... keys, WORKING_LANG=en).
const (
	msgDoesNotMeetMinimum         = "Please check the parcel dimensions. The parcel does not meet the minimum required dimensions."
	msgExceedsMaximum             = "Please check the parcel dimensions. The parcel exceeds the maximum allowed dimensions."
	msgDoesNotMeetMinimumEnvelope = "Please check the envelope dimensions. The envelope does not meet the minimum required dimensions."
	msgExceedsEnvelope            = "Please check the envelope dimensions. The envelope exceeds the maximum allowed dimensions."
	msgExclusiveType              = "Van parcel type cannot be combined with other parcel types."
	msgTypeNotSupported           = "Parcel type is not supported for this service."
)

// expandParcels ports Parcels::createFromQuoteParcels: order packages → pallets → vans →
// envelopes → trucks; quantity expansion with #i suffixes; per-type defaults.
func expandParcels(req *Request, accountType string) []parcel {
	var out []parcel
	expand := func(id string, qty int, f func(i int) parcel) {
		if qty <= 1 {
			out = append(out, f(0))
			return
		}
		for i := 1; i <= qty; i++ {
			p := f(i)
			out = append(out, p)
		}
	}
	fl := func(p *float64, def float64) float64 {
		if p != nil {
			return *p
		}
		return def
	}
	qty := func(q *int) int {
		if q == nil || *q < 1 {
			return 1
		}
		return *q
	}
	expandedID := func(base string, i int) string {
		if i == 0 {
			return base
		}
		return fmt.Sprintf("%s#%d", base, i)
	}

	for _, pk := range req.Parcels.Packages {
		pk := pk
		expand(pk.ParcelID, qty(pk.Quantity), func(i int) parcel {
			return parcel{
				groupID: expandedID(pk.ParcelID, i), customerParcelID: pk.ParcelID, ptype: "package",
				weight: fl(pk.Weight, 1.0), width: fl(pk.Width, 14), height: fl(pk.Height, 14), length: fl(pk.Length, 15),
				value: pk.Value, isStackable: true,
			}
		})
	}
	for _, pl := range req.Parcels.Pallets {
		pl := pl
		stack := true
		if pl.IsStackable != nil {
			stack = *pl.IsStackable
		}
		expand(pl.ParcelID, qty(pl.Quantity), func(i int) parcel {
			return parcel{
				groupID: expandedID(pl.ParcelID, i), customerParcelID: pl.ParcelID, ptype: "pallet",
				weight: fl(pl.Weight, 50.0), width: fl(pl.Width, 11), height: fl(pl.Height, 1), length: fl(pl.Length, 15),
				value: pl.Value, isStackable: stack,
			}
		})
	}
	for _, v := range req.Parcels.Vans {
		v := v
		w, l, wd, ht := 1000.0, 380.0, 165.0, 180.0
		if v.Type == "large-van" {
			l, wd, ht = 410, 210, 230
		}
		out = append(out, parcel{
			groupID: v.ParcelID, customerParcelID: v.ParcelID, ptype: v.Type,
			weight: w, length: l, width: wd, height: ht, value: v.Value,
			optionalServices: v.OptionalServices, isStackable: true,
		})
	}
	for _, e := range req.Parcels.Envelopes {
		e := e
		expand(e.ParcelID, qty(e.Quantity), func(i int) parcel {
			return parcel{
				groupID: expandedID(e.ParcelID, i), customerParcelID: e.ParcelID, ptype: "envelope",
				weight: fl(e.Weight, 0.5), width: 28, height: 1, length: 35, value: e.Value, isStackable: true,
			}
		})
	}
	for _, t := range req.Parcels.Trucks {
		t := t
		ttype := "full-truck-load"
		if t.Type != nil && *t.Type != "" {
			ttype = *t.Type
		}
		var cargoQty *float64
		cpt := "euro-pallets"
		if t.CargoPackagingType != nil && *t.CargoPackagingType != "" {
			cpt = *t.CargoPackagingType
		}
		switch cpt {
		case "euro-pallets":
			if t.EuroPalletQuantity != nil && *t.EuroPalletQuantity != 0 {
				q := float64(*t.EuroPalletQuantity)
				cargoQty = &q
			}
		case "other-packaging":
			cargoQty = t.LoadingMeters
		default:
			z := 0.0
			cargoQty = &z
		}
		out = append(out, parcel{
			groupID: t.ParcelID, customerParcelID: t.ParcelID, ptype: ttype,
			weight: fl(t.Weight, 22000), length: 1360, width: 245, height: 245,
			value: t.Value, isStackable: true, cargoQuantity: cargoQty, cargoPackagingType: &cpt,
		})
	}
	_ = accountType
	return out
}

// dims sorted descending
func sortedDims(l, w, h float64) (d0, d1, d2 float64) {
	d0, d1, d2 = l, w, h
	if d0 < d1 {
		d0, d1 = d1, d0
	}
	if d1 < d2 {
		d1, d2 = d2, d1
	}
	if d0 < d1 {
		d0, d1 = d1, d0
	}
	return
}

func meetsMinimum(weight, l, w, h float64) bool {
	d0, d1, d2 := sortedDims(l, w, h)
	return weight >= 0.1 && d0 >= 15 && d1 >= 11 && d2 >= 1
}

func exceedsMax(weight, l, w, h float64) bool {
	d0, d1, d2 := sortedDims(l, w, h)
	return weight > 10000.0 || d0 > 1000 || d1 > 1000 || d2 > 1000
}

func exceedsEnvelope(weight, length, width float64) bool {
	e0, e1 := length, width
	if e0 < e1 {
		e0, e1 = e1, e0
	}
	return weight > 2 || e0 > 35 || e1 > 28
}

func fitsStandard(weight, l, w, h float64) bool {
	d0, d1, d2 := sortedDims(l, w, h)
	return weight <= 40 && d0 <= 200 && (d0+2*d1+2*d2) <= 300
}

func validSelectionWeight(weight float64, pickup, delivery int) bool {
	max := 30.0
	if pickup != delivery || pickup == countryCroatia || pickup == countryPortugal || pickup == countrySlovenia {
		max = 40.0
	}
	return weight <= max
}

func fitsUpgraded(weight, l, w, h float64) bool {
	d0, _, _ := sortedDims(l, w, h)
	return weight <= 70 && d0 <= 300
}

func fitsFreight(weight, l, w, h float64) bool {
	d0, d1, d2 := sortedDims(l, w, h)
	return weight <= 3000 && d0 <= 400 && d1 <= 240 && d2 <= 220
}

// detectServiceType ports ServiceTypeDetector::detect (selectedServiceType always nil here).
func detectServiceType(items []parcel, pickupCountryID, deliveryCountryID int, snap *refdata.Snapshot) (int, error) {
	serviceType := stSelection
	containsPallet := false
	route := snap.RoutesByFromTo[[2]int{pickupCountryID, deliveryCountryID}]

	validateForExpress := func() error {
		if len(items) == 0 {
			return nil
		}
		first := items[0].ptype
		for _, p := range items {
			if p.ptype != first {
				return &invalidParcelError{msgTypeNotSupported}
			}
			switch p.ptype {
			case "envelope":
				if exceedsEnvelope(p.weight, p.length, p.width) || !meetsMinimum(p.weight, p.length, p.width, 1) {
					return &invalidParcelError{msgExceedsEnvelope}
				}
			case "package":
				if !meetsMinimum(p.weight, p.length, p.width, p.height) {
					return &invalidParcelError{msgDoesNotMeetMinimum}
				}
			default:
				return &invalidParcelError{msgTypeNotSupported}
			}
		}
		return nil
	}

	for _, p := range items {
		height := p.height
		if p.ptype == "envelope" {
			height = 1 // MIN_HEIGHT override in the normalized copy
			if !meetsMinimum(p.weight, p.length, p.width, height) {
				return 0, &invalidParcelError{msgDoesNotMeetMinimumEnvelope}
			}
			if exceedsEnvelope(p.weight, p.length, p.width) {
				return 0, &invalidParcelError{msgExceedsEnvelope}
			}
		}
		switch p.ptype {
		case "full-truck-load", "less-than-truck-load":
			return stFTL, nil
		case "container":
			return stContainer, nil // validateForContainer is a php no-op (precedence bug)
		}
		if !meetsMinimum(p.weight, p.length, p.width, height) {
			return 0, &invalidParcelError{msgDoesNotMeetMinimum}
		}
		if exceedsMax(p.weight, p.length, p.width, height) {
			return 0, &invalidParcelError{msgExceedsMaximum}
		}
		switch p.ptype {
		case "envelope":
			return stExpress, nil
		case "non-standard":
			return stIndividualOffer, nil
		case "small-van", "large-van":
			for _, q := range items {
				if q.ptype != "small-van" && q.ptype != "large-van" {
					return 0, &invalidParcelError{msgExclusiveType}
				}
			}
			return stVan, nil
		}
		if p.ptype == "package" && (serviceType == stSelection || serviceType == stFlexi) {
			if !validSelectionWeight(p.weight, pickupCountryID, deliveryCountryID) || !fitsStandard(p.weight, p.length, p.width, p.height) {
				if fitsUpgraded(p.weight, p.length, p.width, p.height) {
					serviceType = stRegularPlus
				} else {
					serviceType = stFreight
				}
			}
		}
		if p.ptype == "pallet" || p.ptype == "euro-pallet" || serviceType == stFreight {
			if fitsFreight(p.weight, p.length, p.width, p.height) {
				if p.ptype == "pallet" || p.ptype == "euro-pallet" {
					containsPallet = true
				}
				serviceType = stFreight
			} else {
				return stIndividualOffer, nil
			}
		}
	}

	if !containsPallet && route != nil {
		pickup := snap.CountriesByID[pickupCountryID]
		delivery := snap.CountriesByID[deliveryCountryID]
		if pickup != nil && delivery != nil && isGlobal(pickup, delivery) {
			if pickupCountryID == countryUK && delivery.EU && serviceType == stSelection {
				return stSelection, nil
			}
			if err := validateForExpress(); err != nil {
				return 0, err
			}
			return stExpress, nil
		}
	}
	if !containsPallet && serviceType == stFreight {
		return stIndividualOffer, nil
	}
	return serviceType, nil
}

// isEuPair mirrors RouteHelper::isEu.
func isEuPair(p, d *refdata.Country) bool {
	if p.ID == countryUK && d.ID == countryUK {
		return true
	}
	if p.ID == countrySwitzerland && d.ID == countrySwitzerland {
		return true
	}
	if p.ID == countryNorway && d.ID == countryNorway {
		return true
	}
	return p.EU && d.EU
}

func isGlobal(p, d *refdata.Country) bool { return !isEuPair(p, d) }

// vatRateIDFor ports VatCalculator::getUserVatRateId for guests (USER_COUNTRY_NA, MULTI_SPLIT).
func vatRateIDFor(snap *refdata.Snapshot, pickupID, deliveryID int, vatDate time.Time) (int, error) {
	pickup := snap.CountriesByID[pickupID]
	delivery := snap.CountriesByID[deliveryID]
	if pickup == nil || delivery == nil {
		return 0, fmt.Errorf("Provided pickup or delivery countryId is not valid")
	}
	class := func(c *refdata.Country) string {
		if c.ID == countryLuxembourg {
			return "lu"
		}
		if c.EU {
			return "eu"
		}
		return "ne"
	}
	p, d := class(pickup), class(delivery)
	if p == "eu" && d == "eu" && pickupID == deliveryID {
		d = "es"
	}
	// USER_COUNTRY_NA row of the MULTI_COUNTRY_SPLIT matrix
	var vatType string
	switch p + "," + d {
	case "lu,lu", "lu,eu", "eu,lu", "eu,eu", "eu,es":
		vatType = "pickup_country"
	case "lu,ne":
		vatType = "c2c_lu_to_non_eu"
	case "eu,ne":
		vatType = "c2c_eu_except_lu_to_non_eu"
	case "ne,lu":
		vatType = "c2c_non_eu_to_lu"
	case "ne,eu", "ne,es":
		vatType = "c2c_non_eu_to_eu_except_lu"
	case "ne,ne":
		vatType = "c2c_non_eu_to_non_eu"
	}
	vatCountryCode := "ZZ"
	if vatType == "pickup_country" {
		if pickup.EU {
			vatCountryCode = vatCodeOf(pickup)
		}
		if vatCountryCode == "LU" {
			vatType = "lu_standard"
		}
	}
	id, ok := snap.VatRateIDFor(vatType, vatCountryCode, ymd(vatDate))
	if !ok {
		return 0, fmt.Errorf("No vatRateId found for %s for country %s", vatType, vatCountryCode)
	}
	return id, nil
}

// vatCodeOf: first 2 chars of country_code uppercased; GR → EL.
func vatCodeOf(c *refdata.Country) string {
	code := strings.ToUpper(c.Code)
	if len(code) > 2 {
		code = code[:2]
	}
	if code == "GR" {
		code = "EL"
	}
	return code
}

// specifyInsuranceID ports DynamicPriceDataFactory::specifyInsuranceId.
func specifyInsuranceID(provided *int, serviceTypeID int) *int {
	if provided == nil || *provided == 0 {
		switch serviceTypeID {
		case stSelection, stFlexi:
			z := 0
			return &z // Insurance::DEFAULT_FREE_INSURANCE_EXTRA_ID = 0
		case stExpress:
			return nil
		default:
			cmr := extraInsuranceCMR
			return &cmr
		}
	}
	return provided
}

// resolve builds quoteData from a decoded request (guest path).
func resolve(req *Request, snap *refdata.Snapshot, now clock) (*quoteData, error) {
	accountType := req.AccountType // guest: request value verbatim (validated person|company)
	customerType := "guest"
	if accountType == "company" {
		customerType = "business"
	}
	items := expandParcels(req, accountType)

	serviceType := 0
	detected := req.SelectedServiceTypeID == nil
	if req.SelectedServiceTypeID != nil {
		serviceType = *req.SelectedServiceTypeID
	} else {
		st, err := detectServiceType(items, req.Shipment.PickupAddress.CountryID, req.Shipment.DeliveryAddress.CountryID, snap)
		if err != nil {
			return nil, err
		}
		serviceType = st
	}

	// pickupDate: 'Y-m-d' strict, else RFC3339 → backend zone → startOfDay
	var pickupDate *time.Time
	if req.Shipment.PickupDate != nil && *req.Shipment.PickupDate != "" {
		s := *req.Shipment.PickupDate
		if t, err := time.ParseInLocation("2006-01-02", s, backendZone); err == nil {
			pickupDate = &t
		} else if t, err := time.Parse(time.RFC3339, s); err == nil {
			t = startOfDay(t.In(backendZone))
			pickupDate = &t
		} else {
			return nil, &invalidParcelError{fmt.Sprintf("Could not parse '%s'", s)}
		}
	}

	// regions (spec 02 §6.2)
	regionID := func(countryID int, name *string, serviceTypeID int) *int {
		if name == nil || *name == "" {
			return nil
		}
		hasRegions := countryID == countryIreland || countryID == countryRomania || countryID == countryItaly ||
			countryID == countryUS || countryID == countryCanada
		stHasRegions := !(serviceTypeID == stVan || serviceTypeID == stFTL || serviceTypeID == stIndividualOffer ||
			serviceTypeID == stContainer || serviceTypeID == stRail)
		if !hasRegions || !stHasRegions {
			return nil
		}
		if m := snap.RegionIDByCountryAndName[countryID]; m != nil {
			if id, ok := m[strings.ToLower(*name)]; ok {
				return &id
			}
		}
		return nil
	}

	// pickup timezone (spec 02 §6.10): request field, else countries.time_zone, else nil
	var pickupTZ *time.Location
	if tzn := req.Shipment.PickupAddress.TimeZoneName; tzn != nil && *tzn != "" && *tzn != "0" {
		l, err := time.LoadLocation(*tzn)
		if err != nil {
			return nil, &invalidParcelError{fmt.Sprintf("Invalid timezone \"%s\"", *tzn)}
		}
		pickupTZ = l
	} else if c := snap.CountriesByID[req.Shipment.PickupAddress.CountryID]; c != nil && c.TimeZone != "" {
		if l, err := time.LoadLocation(c.TimeZone); err == nil {
			pickupTZ = l
		}
	}

	// vat date: raw paymentMethod === 'deferred' → pickupDate else today (server tz)
	vatDate := startOfDay(now().In(backendZone))
	if req.PaymentMethod != nil && *req.PaymentMethod == "deferred" && pickupDate != nil {
		vatDate = *pickupDate
	}
	vatRateID, err := vatRateIDFor(snap, req.Shipment.PickupAddress.CountryID, req.Shipment.DeliveryAddress.CountryID, vatDate)
	if err != nil {
		return nil, &invalidParcelError{err.Error()}
	}

	paymentType := "credit_card"
	if req.PaymentMethod != nil && *req.PaymentMethod != "" {
		paymentType = *req.PaymentMethod
	}
	source := "website"
	if req.Source != nil && *req.Source != "" {
		source = *req.Source
	}

	// hasFlexibleBooking (guest: only via addOns; company users are auth-only, out of guest scope)
	hasFlex := false
	supported := map[int]bool{stSelection: true, stRegularPlus: true, stFlexi: true, stExpress: true,
		stFreight: true, stFreightPriority: true, stFreightPriorityExpress: true}
	if supported[serviceType] {
		for _, a := range req.Shipment.AddOns {
			if a == "flexibleChanges" {
				hasFlex = true
			}
		}
	}
	var extraIDs []int
	for _, a := range req.Shipment.AddOns {
		if a == "fedexInternationalPriorityExpress" {
			extraIDs = append(extraIDs, extraFedexIPE)
		}
	}

	excluded := map[int][]int{}
	for _, k := range excludedCouriersKeyOrder {
		excluded[k] = []int{}
	}

	pickup := snap.CountriesByID[req.Shipment.PickupAddress.CountryID]
	delivery := snap.CountriesByID[req.Shipment.DeliveryAddress.CountryID]
	globalRoute := false
	if pickup != nil && delivery != nil {
		globalRoute = isGlobal(pickup, delivery)
	}

	d := &quoteData{
		serviceType:                serviceType,
		serviceTypeIsDetected:      detected,
		serviceSubtypeRaw:          req.ServiceSubtype,
		paymentType:                paymentType,
		insuranceID:                specifyInsuranceID(req.AdditionalInsuranceID, serviceType),
		customerType:               customerType,
		accountType:                accountType,
		pickupCountryID:            req.Shipment.PickupAddress.CountryID,
		deliveryCountryID:          req.Shipment.DeliveryAddress.CountryID,
		pickupRegionID:             regionID(req.Shipment.PickupAddress.CountryID, req.Shipment.PickupAddress.Region, serviceType),
		deliveryRegionID:           regionID(req.Shipment.DeliveryAddress.CountryID, req.Shipment.DeliveryAddress.Region, serviceType),
		pickupZip:                  req.Shipment.PickupAddress.Zip,
		pickupCity:                 req.Shipment.PickupAddress.City,
		deliveryZip:                req.Shipment.DeliveryAddress.Zip,
		deliveryCity:               req.Shipment.DeliveryAddress.City,
		currencyID:                 currencyIDByCode[req.CurrencyCode],
		couponCode:                 req.CouponCode,
		hasFlexibleBooking:         hasFlex,
		extraIDs:                   extraIDs,
		items:                      items,
		pickupDate:                 pickupDate,
		pickupTimeZone:             pickupTZ,
		vatRateID:                  vatRateID,
		priceVersion:               snap.PEVersion,
		source:                     source,
		valuePerShipment:           req.Shipment.Value,
		excludedCouriersPerService: excluded,
		isGlobalRouteFlag:          globalRoute,
	}
	d.useRequestedServiceTypeOnly = source != "api" && req.SelectedServiceTypeID != nil
	return d, nil
}
