// Package refdata loads the reference tables the quote path reads into an immutable in-memory
// Snapshot — go-ba's counterpart of go-pe's version snapshot. All lookups after prepare are pure
// map/slice reads; the request path performs zero MySQL operations by construction.
package refdata

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
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
	ID                       int
	Description              string
	InsurerID                int
	MinContentValue          float64
	MaxContentValue          float64
	PriceExclVat             float64
	MinCost                  float64
	ShipmentValueCostFactor  float64
	AbsoluteMargin           float64
	RelativeMargin           float64
}

type Extra struct {
	ID                 int
	Name               string
	Description        string
	Value              string
	NetCost            float64
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
	ID          int
	Code        string
	Name        string
	EU          bool
	TimeZone    string
	MainlandID  sql.NullInt64
}

// Snapshot is the immutable, fully-indexed reference model. Build once, swap atomically.
type Snapshot struct {
	LoadedAt time.Time

	HolidaysByCountry map[int][]NonWorkingDay          // active only, sorted by date
	HolidaysByDate    map[string][]NonWorkingDay       // active only
	ExceptionsByCourier map[int]map[string]bool        // courierID -> date -> working (enabled rows)
	ExceptionsAnyCourier map[string]bool               // date -> some courier works

	ActiveTerms          *TermsConditions              // newest active
	ActiveTermsByCourier map[int]*TermsConditionsCourier

	InsuranceByID          map[int]*InsurancePackage
	InsuranceByInsurer     map[int][]*InsurancePackage // sorted by MinContentValue
	Extras                 []Extra
	ExtrasByID             map[int]*Extra

	vatRatesByTypeCountry map[string][]vatRateWindow // "type|CC" -> windows sorted by ValidFrom desc

	RoutesByFromTo map[[2]int]*Route
	CountriesByID  map[int]*Country

	PEVersion string // latest by date_created

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
		LoadedAt:             time.Now(),
		HolidaysByCountry:    map[int][]NonWorkingDay{},
		HolidaysByDate:       map[string][]NonWorkingDay{},
		ExceptionsByCourier:  map[int]map[string]bool{},
		ExceptionsAnyCourier: map[string]bool{},
		ActiveTermsByCourier: map[int]*TermsConditionsCourier{},
		InsuranceByID:        map[int]*InsurancePackage{},
		InsuranceByInsurer:   map[int][]*InsurancePackage{},
		ExtrasByID:           map[int]*Extra{},
		vatRatesByTypeCountry: map[string][]vatRateWindow{},
		RoutesByFromTo:       map[[2]int]*Route{},
		CountriesByID:        map[int]*Country{},
		Rows:                 map[string]int{},
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
	return s, nil
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
		`SELECT id, COALESCE(description,''), insurer_id, COALESCE(min_content_value,0), COALESCE(max_content_value,0),
		        COALESCE(price_excl_vat,0), COALESCE(min_cost,0), COALESCE(shipment_value_cost_factor,0),
		        COALESCE(absolute_margin,0), COALESCE(relative_margin,0)
		 FROM insurance_package`)
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
		`SELECT id, COALESCE(name,''), COALESCE(description,''), COALESCE(value,''), COALESCE(net_cost,0),
		        COALESCE(type,''), insurance_package_id
		 FROM ns_catalog_order_extras_list`)
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
	for i := range s.Extras {
		s.ExtrasByID[s.Extras[i].ID] = &s.Extras[i]
	}
	return rows.Err()
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
