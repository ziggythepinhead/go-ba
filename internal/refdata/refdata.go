// Package refdata loads the reference tables the quote path reads into an immutable in-memory
// Snapshot — go-be's counterpart of go-pe's version snapshot. All lookups after prepare are pure
// map/slice reads; the request path performs zero MySQL operations by construction.
package refdata

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

type NonWorkingDay struct {
	ID              int
	HolidayDate     string // Y-m-d
	PickupCountryID int
	Active          bool
	BothWays        bool
}

type CourierExceptionalWorkingDate struct {
	ID        int
	Date      string // Y-m-d
	CourierID int
	Disabled  bool
}

type TermsConditions struct {
	ID        int
	Version   string
	Link      string
	Active    bool
	Languages string
}

type TermsConditionsCourier struct {
	ID        int
	CourierID int
	Version   string
	Link      string
	Active    bool
	Languages string
}

type InsurancePackage struct {
	ID                      int
	Description             string
	InsurerID               int
	MinContentValue         float64         // NULL ⇒ 0 in php arithmetic
	MaxContentValue         sql.NullFloat64 // NULL ⇒ 200 (Insurance::DEFAULT_FREE_INSURANCE_MAX_CONTENT_VALUE)
	PriceExclVat            sql.NullFloat64
	MinCost                 float64 // NULL ⇒ 0
	ShipmentValueCostFactor float64
	AbsoluteMargin          float64
	RelativeMargin          float64
}

// MaxContent mirrors InsurancePackage::getMaxContentValue (NULL ⇒ 200).
func (p *InsurancePackage) MaxContent() float64 {
	if p.MaxContentValue.Valid {
		return p.MaxContentValue.Float64
	}
	return 200
}

type Extra struct {
	ID                 int
	Name               string
	Description        string
	Value              sql.NullFloat64 // ExtraList::getNetPrice (nullable)
	NetCost            float64         // net_cost ?? 0.0
	Type               string
	InsurancePackageID sql.NullInt64
}

type VatRate struct {
	ID          int
	CountryCode string
	VatType     string
	Rate        float64
}

type VatRateDate struct {
	VatRateID int
	ValidFrom string // Y-m-d
}

type Route struct {
	ID            int
	FromCountryID int
	ToCountryID   int
	IsEU          bool
}

type Country struct {
	ID         int
	Code       string
	Name       string
	EU         bool
	TimeZone   string
	MainlandID sql.NullInt64
}

// SimplifiedRoute mirrors an enabled_simplified_routes row (admin config: which "simplified"
// FE services are offered between location patterns like Anywhere/Europe/<country code>).
type SimplifiedRoute struct {
	PickupLocation   string
	DeliveryLocation string
	Service          string // simplified_frontend_service
}

// Snapshot is the immutable, fully-indexed reference model. Build once, swap atomically.
type Snapshot struct {
	LoadedAt time.Time

	HolidaysByCountry    map[int][]NonWorkingDay    // active only, sorted by date
	HolidaysByDate       map[string][]NonWorkingDay // active only
	ExceptionsByCourier  map[int]map[string]bool    // courierID -> date -> working (enabled rows)
	ExceptionsAnyCourier map[string]bool            // date -> some courier works

	ActiveTerms          *TermsConditions // newest active
	ActiveTermsByCourier map[int]*TermsConditionsCourier

	InsuranceByID      map[int]*InsurancePackage
	InsuranceByInsurer map[int][]*InsurancePackage // sorted by MinContentValue
	Extras             []Extra
	ExtrasByID         map[int]*Extra
	// PackageByExtraID mirrors InsurancePackageDAO::findOneByExtraId (join through
	// ns_catalog_order_extras_list.insurance_package_id).
	PackageByExtraID map[int]*InsurancePackage
	// FreeInsuranceExtraByCourier: extra id of the courier's zero-price insurance package
	// (insurer_id = courierId AND price_excl_vat = 0, lowest-id row) — basic-insurance default branch.
	FreeInsuranceExtraByCourier map[int]int

	vatRatesByTypeCountry map[string][]vatRateWindow // "type|CC" -> windows sorted by ValidFrom desc

	RoutesByFromTo map[[2]int]*Route
	CountriesByID  map[int]*Country

	PEVersion string // latest by date_created

	// CourierCountryIDs mirrors CourierDAO::getAllCourierCountryIds (couriers joined to countries
	// by country_code) — DefaultPickupDateProvider steps the friendly pickup date past holidays in
	// ANY of these countries.
	CourierCountryIDs []int

	// CourierCountryByID mirrors CourierDAO::getCourierCountryId (Q6a).
	CourierCountryByID map[int]int

	// CutoffByCourierServiceType mirrors courier_to_service_type.internal_cut_off_time keyed
	// (courier_id, service_type_id); first row per pair wins (php FIND first-row semantics).
	// Missing pair -> php DEFAULT_MAX_ORDERING_TIME "14:00".
	CutoffByCourierServiceType map[[2]int]string

	// VatRateByID mirrors vat_rate.rate lookups (VatCalculator::applyVat, options.vatRate).
	VatRateByID map[int]float64

	// RegionIDByCountryAndName mirrors RegionDAO::findOneByCountryIdAndName — MySQL CI collation,
	// so keys are lowercased names.
	RegionIDByCountryAndName map[int]map[string]int

	// ExchangeRateByCode: latest currency_exchange_rate row column per code (Q9), multiplied at
	// use-site by ns_catalog_currencies.exchange_rate_percentage (ExchangePercentageByCode).
	ExchangeRateByCode       map[string]float64
	ExchangePercentageByCode map[string]float64

	// PickupBlockedByCountryCourier mirrors courier_limited_service_country rows with
	// service='Pickup Request' AND blocked_for_pickup=1, keyed (country_id, courier_id).
	PickupBlockedByCountryCourier map[[2]int]bool

	// SimplifiedRoutes mirrors enabled_simplified_routes (admin config for the simplified half of
	// /countries/blocked-routes): pickup/delivery location patterns per simplified FE service.
	SimplifiedRoutes []SimplifiedRoute

	Rows map[string]int // table -> row count (for /metrics and reload-change logging)
}

type vatRateWindow struct {
	ValidFrom string
	RateID    int
	Rate      float64
}

// VatRateIDFor mirrors VatRateDao::findOneIdByVatTypeAndVatCountry (join vat_rate_dates,
// valid_from <= date, newest first).
func (s *Snapshot) VatRateIDFor(vatType, countryCode, dateYMD string) (int, bool) {
	for _, w := range s.vatRatesByTypeCountry[vatType+"|"+countryCode] {
		if w.ValidFrom <= dateYMD {
			return w.RateID, true
		}
	}
	return 0, false
}

// Load reads all reference tables in one pass and builds the indexed snapshot.
func Load(ctx context.Context, db *sql.DB) (*Snapshot, error) {
	s := &Snapshot{
		LoadedAt:                      time.Now(),
		HolidaysByCountry:             map[int][]NonWorkingDay{},
		HolidaysByDate:                map[string][]NonWorkingDay{},
		ExceptionsByCourier:           map[int]map[string]bool{},
		ExceptionsAnyCourier:          map[string]bool{},
		ActiveTermsByCourier:          map[int]*TermsConditionsCourier{},
		InsuranceByID:                 map[int]*InsurancePackage{},
		InsuranceByInsurer:            map[int][]*InsurancePackage{},
		ExtrasByID:                    map[int]*Extra{},
		PackageByExtraID:              map[int]*InsurancePackage{},
		FreeInsuranceExtraByCourier:   map[int]int{},
		vatRatesByTypeCountry:         map[string][]vatRateWindow{},
		RoutesByFromTo:                map[[2]int]*Route{},
		CountriesByID:                 map[int]*Country{},
		CourierCountryByID:            map[int]int{},
		CutoffByCourierServiceType:    map[[2]int]string{},
		VatRateByID:                   map[int]float64{},
		RegionIDByCountryAndName:      map[int]map[string]int{},
		ExchangeRateByCode:            map[string]float64{},
		ExchangePercentageByCode:      map[string]float64{},
		PickupBlockedByCountryCourier: map[[2]int]bool{},
		Rows:                          map[string]int{},
	}

	if err := s.loadHolidays(ctx, db); err != nil {
		return nil, fmt.Errorf("non_working_days: %w", err)
	}
	if err := s.loadCourierExceptions(ctx, db); err != nil {
		return nil, fmt.Errorf("courier_exceptional_working_date: %w", err)
	}
	if err := s.loadTerms(ctx, db); err != nil {
		return nil, fmt.Errorf("terms_conditions: %w", err)
	}
	if err := s.loadInsurance(ctx, db); err != nil {
		return nil, fmt.Errorf("insurance_package: %w", err)
	}
	if err := s.loadExtras(ctx, db); err != nil {
		return nil, fmt.Errorf("extras: %w", err)
	}
	if err := s.loadVatRates(ctx, db); err != nil {
		return nil, fmt.Errorf("vat_rate: %w", err)
	}
	if err := s.loadRoutes(ctx, db); err != nil {
		return nil, fmt.Errorf("price_engine_route: %w", err)
	}
	if err := s.loadCountries(ctx, db); err != nil {
		return nil, fmt.Errorf("countries: %w", err)
	}
	if err := s.loadPEVersion(ctx, db); err != nil {
		return nil, fmt.Errorf("price_engine_version: %w", err)
	}
	if err := s.loadCourierCountryIDs(ctx, db); err != nil {
		return nil, fmt.Errorf("courier countries: %w", err)
	}
	if err := s.loadCutoffs(ctx, db); err != nil {
		return nil, fmt.Errorf("courier_to_service_type: %w", err)
	}
	if err := s.loadRegions(ctx, db); err != nil {
		return nil, fmt.Errorf("region: %w", err)
	}
	if err := s.loadExchangeRates(ctx, db); err != nil {
		return nil, fmt.Errorf("currency_exchange_rate: %w", err)
	}
	if err := s.loadPickupBlocked(ctx, db); err != nil {
		return nil, fmt.Errorf("courier_limited_service_country: %w", err)
	}
	if err := s.loadSimplifiedRoutes(ctx, db); err != nil {
		return nil, fmt.Errorf("enabled_simplified_routes: %w", err)
	}
	return s, nil
}

func (s *Snapshot) loadSimplifiedRoutes(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx,
		`SELECT COALESCE(pickup_location,''), COALESCE(delivery_location,''), COALESCE(simplified_frontend_service,'')
		 FROM enabled_simplified_routes`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r SimplifiedRoute
		if err := rows.Scan(&r.PickupLocation, &r.DeliveryLocation, &r.Service); err != nil {
			return err
		}
		s.Rows["enabled_simplified_routes"]++
		s.SimplifiedRoutes = append(s.SimplifiedRoutes, r)
	}
	return rows.Err()
}

func (s *Snapshot) loadPickupBlocked(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx,
		`SELECT country_id, courier_id FROM courier_limited_service_country
		 WHERE service = 'Pickup Request' AND blocked_for_pickup = 1`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var countryID, courierID int
		if err := rows.Scan(&countryID, &courierID); err != nil {
			return err
		}
		s.PickupBlockedByCountryCourier[[2]int{countryID, courierID}] = true
		s.Rows["courier_limited_service_country"]++
	}
	return rows.Err()
}

func (s *Snapshot) loadCutoffs(ctx context.Context, db *sql.DB) error {
	// No ORDER BY (the table has no id column): natural order = table order, first row per
	// (courier, serviceType) pair wins, matching the php DAO's GetRow semantics.
	rows, err := db.QueryContext(ctx,
		`SELECT courier_id, service_type_id, COALESCE(TIME_FORMAT(internal_cut_off_time, '%H:%i:%s'), '')
		 FROM courier_to_service_type`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var courierID, serviceTypeID int
		var cutoff string
		if err := rows.Scan(&courierID, &serviceTypeID, &cutoff); err != nil {
			return err
		}
		s.Rows["courier_to_service_type"]++
		key := [2]int{courierID, serviceTypeID}
		if _, ok := s.CutoffByCourierServiceType[key]; !ok && cutoff != "" {
			s.CutoffByCourierServiceType[key] = cutoff
		}
	}
	return rows.Err()
}

func (s *Snapshot) loadRegions(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `SELECT id, country_id, COALESCE(name,'') FROM region`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, countryID int
		var name string
		if err := rows.Scan(&id, &countryID, &name); err != nil {
			return err
		}
		s.Rows["region"]++
		if s.RegionIDByCountryAndName[countryID] == nil {
			s.RegionIDByCountryAndName[countryID] = map[string]int{}
		}
		lower := strings.ToLower(name)
		if _, ok := s.RegionIDByCountryAndName[countryID][lower]; !ok {
			s.RegionIDByCountryAndName[countryID][lower] = id
		}
	}
	return rows.Err()
}

func (s *Snapshot) loadExchangeRates(ctx context.Context, db *sql.DB) error {
	// Latest currency_exchange_rate row: one column per currency code (Q9).
	rows, err := db.QueryContext(ctx, `SELECT * FROM currency_exchange_rate ORDER BY created DESC LIMIT 1`)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	if rows.Next() {
		vals := make([]any, len(cols))
		for i := range vals {
			var v sql.NullFloat64
			vals[i] = &v
		}
		// created/id columns fail float scan; use RawBytes-tolerant scan instead
		raw := make([]sql.RawBytes, len(cols))
		ptrs := make([]any, len(cols))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		for i, c := range cols {
			if c == "id" || c == "created" {
				continue
			}
			if f, err := strconv.ParseFloat(string(raw[i]), 64); err == nil {
				s.ExchangeRateByCode[strings.ToUpper(c)] = f
			}
		}
		s.Rows["currency_exchange_rate"] = 1
	}
	if err := rows.Err(); err != nil {
		return err
	}

	crows, err := db.QueryContext(ctx,
		`SELECT COALESCE(code,''), COALESCE(exchange_rate_percentage,0) FROM ns_catalog_currencies`)
	if err != nil {
		return err
	}
	defer crows.Close()
	for crows.Next() {
		var code string
		var pct float64
		if err := crows.Scan(&code, &pct); err != nil {
			return err
		}
		s.Rows["ns_catalog_currencies"]++
		s.ExchangePercentageByCode[strings.ToUpper(code)] = pct
	}
	return crows.Err()
}

func (s *Snapshot) loadCourierCountryIDs(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx,
		`SELECT courier.id, country.id FROM ns_catalog_couriers courier
		 INNER JOIN countries country ON courier.country_code = country.country_code`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var courierID, countryID int
		if err := rows.Scan(&courierID, &countryID); err != nil {
			return err
		}
		s.CourierCountryIDs = append(s.CourierCountryIDs, countryID)
		s.CourierCountryByID[courierID] = countryID
		s.Rows["courier_country_ids"]++
	}
	return rows.Err()
}

func (s *Snapshot) loadHolidays(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx,
		`SELECT id, DATE_FORMAT(holiday_date, '%Y-%m-%d'), pickup_country_id, active, both_ways FROM non_working_days`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var h NonWorkingDay
		var active, both int
		if err := rows.Scan(&h.ID, &h.HolidayDate, &h.PickupCountryID, &active, &both); err != nil {
			return err
		}
		h.Active, h.BothWays = active == 1, both == 1
		s.Rows["non_working_days"]++
		if !h.Active {
			continue
		}
		s.HolidaysByCountry[h.PickupCountryID] = append(s.HolidaysByCountry[h.PickupCountryID], h)
		s.HolidaysByDate[h.HolidayDate] = append(s.HolidaysByDate[h.HolidayDate], h)
	}
	for _, hs := range s.HolidaysByCountry {
		sort.Slice(hs, func(i, j int) bool { return hs[i].HolidayDate < hs[j].HolidayDate })
	}
	return rows.Err()
}

func (s *Snapshot) loadCourierExceptions(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx,
		`SELECT id, DATE_FORMAT(date, '%Y-%m-%d'), courier_id, disabled FROM courier_exceptional_working_date`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var e CourierExceptionalWorkingDate
		var disabled int
		if err := rows.Scan(&e.ID, &e.Date, &e.CourierID, &disabled); err != nil {
			return err
		}
		e.Disabled = disabled == 1
		s.Rows["courier_exceptional_working_date"]++
		if e.Disabled {
			continue
		}
		if s.ExceptionsByCourier[e.CourierID] == nil {
			s.ExceptionsByCourier[e.CourierID] = map[string]bool{}
		}
		s.ExceptionsByCourier[e.CourierID][e.Date] = true
		s.ExceptionsAnyCourier[e.Date] = true
	}
	return rows.Err()
}

func (s *Snapshot) loadTerms(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx,
		`SELECT id, tc_version, tc_link, tc_active, COALESCE(languages,'') FROM terms_conditions`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var t TermsConditions
		var active int
		if err := rows.Scan(&t.ID, &t.Version, &t.Link, &active, &t.Languages); err != nil {
			return err
		}
		t.Active = active == 1
		s.Rows["terms_conditions"]++
		if t.Active && (s.ActiveTerms == nil || t.ID > s.ActiveTerms.ID) {
			tt := t
			s.ActiveTerms = &tt
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	crows, err := db.QueryContext(ctx,
		`SELECT id, tcc_courier_id, tcc_version, tcc_link, tcc_active, COALESCE(languages,'') FROM terms_conditions_courier`)
	if err != nil {
		return err
	}
	defer crows.Close()
	for crows.Next() {
		var t TermsConditionsCourier
		var active int
		if err := crows.Scan(&t.ID, &t.CourierID, &t.Version, &t.Link, &active, &t.Languages); err != nil {
			return err
		}
		t.Active = active == 1
		s.Rows["terms_conditions_courier"]++
		if t.Active {
			if cur := s.ActiveTermsByCourier[t.CourierID]; cur == nil || t.ID > cur.ID {
				tt := t
				s.ActiveTermsByCourier[t.CourierID] = &tt
			}
		}
	}
	return crows.Err()
}

func (s *Snapshot) loadInsurance(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx,
		`SELECT id, COALESCE(description,''), insurer_id, COALESCE(min_content_value,0), max_content_value,
		        price_excl_vat, COALESCE(min_cost,0), COALESCE(shipment_value_cost_factor,0),
		        COALESCE(absolute_margin,0), COALESCE(relative_margin,0)
		 FROM insurance_package ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var p InsurancePackage
		if err := rows.Scan(&p.ID, &p.Description, &p.InsurerID, &p.MinContentValue, &p.MaxContentValue,
			&p.PriceExclVat, &p.MinCost, &p.ShipmentValueCostFactor, &p.AbsoluteMargin, &p.RelativeMargin); err != nil {
			return err
		}
		s.Rows["insurance_package"]++
		pp := p
		s.InsuranceByID[p.ID] = &pp
		s.InsuranceByInsurer[p.InsurerID] = append(s.InsuranceByInsurer[p.InsurerID], &pp)
	}
	for _, ps := range s.InsuranceByInsurer {
		sort.Slice(ps, func(i, j int) bool { return ps[i].MinContentValue < ps[j].MinContentValue })
	}
	return rows.Err()
}

func (s *Snapshot) loadExtras(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx,
		`SELECT id, COALESCE(name,''), COALESCE(description,''), value, COALESCE(net_cost,0),
		        COALESCE(type,''), insurance_package_id
		 FROM ns_catalog_order_extras_list ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var e Extra
		if err := rows.Scan(&e.ID, &e.Name, &e.Description, &e.Value, &e.NetCost, &e.Type, &e.InsurancePackageID); err != nil {
			return err
		}
		s.Rows["ns_catalog_order_extras_list"]++
		s.Extras = append(s.Extras, e)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range s.Extras {
		e := &s.Extras[i]
		s.ExtrasByID[e.ID] = e
		// findOneByExtraId join: extra → its insurance package (first extra per package wins the
		// reverse map below via the zero-price scan)
		if e.InsurancePackageID.Valid {
			if p, ok := s.InsuranceByID[int(e.InsurancePackageID.Int64)]; ok {
				if _, dup := s.PackageByExtraID[e.ID]; !dup {
					s.PackageByExtraID[e.ID] = p
				}
			}
		}
	}
	// FreeInsuranceExtraByCourier: for each insurer, the lowest-id package with price_excl_vat = 0,
	// then the lowest-id extra pointing at it (php first-row semantics).
	for insurer, pkgs := range s.InsuranceByInsurer {
		var zero *InsurancePackage
		for _, p := range pkgs {
			if p.PriceExclVat.Valid && p.PriceExclVat.Float64 == 0.0 {
				if zero == nil || p.ID < zero.ID {
					zero = p
				}
			}
		}
		if zero == nil {
			continue
		}
		for i := range s.Extras {
			e := &s.Extras[i]
			if e.InsurancePackageID.Valid && int(e.InsurancePackageID.Int64) == zero.ID {
				s.FreeInsuranceExtraByCourier[insurer] = e.ID
				break
			}
		}
	}
	return nil
}

func (s *Snapshot) loadVatRates(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx,
		`SELECT vr.id, vr.vat_country_code, vr.vat_type, COALESCE(vr.rate,0), DATE_FORMAT(vrd.valid_from, '%Y-%m-%d')
		 FROM vat_rate vr JOIN vat_rate_dates vrd ON vr.id = vrd.vat_rate_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r VatRate
		var validFrom string
		if err := rows.Scan(&r.ID, &r.CountryCode, &r.VatType, &r.Rate, &validFrom); err != nil {
			return err
		}
		s.Rows["vat_rate_windows"]++
		key := r.VatType + "|" + r.CountryCode
		s.vatRatesByTypeCountry[key] = append(s.vatRatesByTypeCountry[key],
			vatRateWindow{ValidFrom: validFrom, RateID: r.ID, Rate: r.Rate})
		s.VatRateByID[r.ID] = r.Rate
	}
	for _, ws := range s.vatRatesByTypeCountry {
		sort.Slice(ws, func(i, j int) bool { return ws[i].ValidFrom > ws[j].ValidFrom })
	}
	return rows.Err()
}

func (s *Snapshot) loadRoutes(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `SELECT id, from_country_id, to_country_id, COALESCE(is_eu,0) FROM price_engine_route`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r Route
		var isEU int
		if err := rows.Scan(&r.ID, &r.FromCountryID, &r.ToCountryID, &isEU); err != nil {
			return err
		}
		r.IsEU = isEU == 1
		s.Rows["price_engine_route"]++
		rr := r
		s.RoutesByFromTo[[2]int{r.FromCountryID, r.ToCountryID}] = &rr
	}
	return rows.Err()
}

func (s *Snapshot) loadCountries(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx,
		`SELECT id, COALESCE(country_code,''), COALESCE(country_name,''), COALESCE(eu,0), COALESCE(time_zone,''), mainland_country_id FROM countries`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var c Country
		var eu int
		if err := rows.Scan(&c.ID, &c.Code, &c.Name, &eu, &c.TimeZone, &c.MainlandID); err != nil {
			return err
		}
		c.EU = eu == 1
		s.Rows["countries"]++
		cc := c
		s.CountriesByID[c.ID] = &cc
	}
	return rows.Err()
}

func (s *Snapshot) loadPEVersion(ctx context.Context, db *sql.DB) error {
	// mirrors PriceEngineVersionDAO::findLastVersion (latest by date_created)
	err := db.QueryRowContext(ctx,
		`SELECT version FROM price_engine_version ORDER BY date_created DESC LIMIT 1`).Scan(&s.PEVersion)
	if err == sql.ErrNoRows {
		return nil
	}
	if err == nil {
		s.Rows["price_engine_version"] = 1
	}
	return err
}
