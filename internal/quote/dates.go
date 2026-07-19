// Date engine: Carbon-parity weekday arithmetic, DefaultPickupDateProvider, PickupDateService
// (min pickup date per serviceType×courier), OrderingDateCalculator (SELECTION), excluded dates
// (PickupExcludedDaysProvider), IO min pickup, edt dates. Specs: 03 §6, 07 §1, 08 §6.3.
package quote

import (
	"fmt"
	"sort"
	"time"

	"github.com/eurosender/go-be/internal/refdata"
)

var backendZone = mustZone("Europe/Ljubljana")

func mustZone(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return l
}

// addWeekdays mirrors Carbon::addWeekdays / php "+N weekdays": step forward N times skipping
// Sat/Sun. From Sat/Sun the first step lands on Monday. n=0 is a no-op.
func addWeekdays(t time.Time, n int) time.Time {
	for i := 0; i < n; i++ {
		t = t.AddDate(0, 0, 1)
		for t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
			t = t.AddDate(0, 0, 1)
		}
	}
	return t
}

// subWeekdays mirrors Carbon::subWeekdays: step backward skipping Sat/Sun (Mon-1wd=Fri, Sun-1wd=Fri).
func subWeekdays(t time.Time, n int) time.Time {
	for i := 0; i < n; i++ {
		t = t.AddDate(0, 0, -1)
		for t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
			t = t.AddDate(0, 0, -1)
		}
	}
	return t
}

func isWeekend(t time.Time) bool {
	return t.Weekday() == time.Saturday || t.Weekday() == time.Sunday
}

func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func ymd(t time.Time) string { return t.Format("2006-01-02") }

// clock abstracts "now" so tests can pin it; production uses time.Now.
type clock func() time.Time

// holidayCtx bundles the snapshot lookups the date engine needs.
type holidayCtx struct {
	snap *refdata.Snapshot
	now  clock
}

// isHolidayOn: active holiday row for date in ANY of countryIDs (nil/0 entries dropped — php
// array_filter). Mirrors PickupDateService::isHoliday over the Q1 per-country sets.
func (h holidayCtx) isHolidayOn(t time.Time, countryIDs []int) bool {
	date := ymd(t)
	seen := map[int]bool{}
	for _, id := range countryIDs {
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		for _, hd := range h.snap.HolidaysByDate[date] {
			if hd.PickupCountryID == id {
				return true
			}
		}
	}
	return false
}

// holidayWithCourierException mirrors AggregateCourierNonWorkingDayProvider::
// findOneActiveByDateCountryIdAndCourierId: nil result ("working day") when the courier has an
// active exceptional working date row on that date; else the active holiday row for (date, country).
func (h holidayCtx) holidayWithCourierException(date string, countryID int, courierID int) bool {
	if h.snap.ExceptionsByCourier[courierID] != nil && h.snap.ExceptionsByCourier[courierID][date] {
		return false
	}
	for _, hd := range h.snap.HolidaysByDate[date] {
		if hd.PickupCountryID == countryID {
			return true
		}
	}
	return false
}

// getEarliestNonHolidayOrWeekend: while weekend or holiday in any country, +1 weekday.
func (h holidayCtx) earliestNonHolidayOrWeekend(t time.Time, countryIDs []int) time.Time {
	for isWeekend(t) || h.isHolidayOn(t, countryIDs) {
		t = addWeekdays(t, 1)
	}
	return t
}

// defaultPickupDate ports DefaultPickupDateProvider::getForPickupCountryId (spec 03 §3.5.1).
func (h holidayCtx) defaultPickupDate(pickupCountryID int) time.Time {
	countries := append([]int{}, h.snap.CourierCountryIDs...)
	countries = append(countries, pickupCountryID)
	today := startOfDay(h.now().In(backendZone))
	pickupDate := addWeekdays(today, 2)
	orderingDate := addWeekdays(today, 1)
	for h.isHolidayOn(pickupDate, countries) || h.isHolidayOn(orderingDate, countries) {
		pickupDate = addWeekdays(pickupDate, 1)
		orderingDate = addWeekdays(orderingDate, 1)
	}
	return pickupDate
}

// cutoffTime returns the courier's internal cut-off time string for a serviceType.
// VAN → 14:00:00, FTL → 15:00:00, else courier_to_service_type row or '14:00' default.
func (h holidayCtx) cutoffTime(serviceTypeID, courierID int) string {
	switch serviceTypeID {
	case stVan:
		return "14:00:00"
	case stFTL:
		return "15:00:00"
	}
	if c, ok := h.snap.CutoffByCourierServiceType[[2]int{courierID, serviceTypeID}]; ok {
		return c
	}
	return "14:00:00"
}

// setTimeFromString mirrors Carbon::setTimeFromTimeString for "HH:MM" / "HH:MM:SS".
func setTimeFromString(t time.Time, hms string) time.Time {
	var hh, mm, ss int
	fmt.Sscanf(hms, "%d:%d:%d", &hh, &mm, &ss)
	return time.Date(t.Year(), t.Month(), t.Day(), hh, mm, ss, 0, t.Location())
}

// orderingDayRules ports OrderingDateRulesFactory::create(courierId)->getDays(pickupCountryId).
func orderingDayRules(courierID, pickupCountryID int) int {
	switch courierID {
	case 2: // GLS_SLOVENIA
		if pickupCountryID == countryIreland {
			return 2
		}
	case 99: // GLS_IRELAND
		if pickupCountryID == countryIreland || pickupCountryID == countrySpain || pickupCountryID == countryPortugal {
			return 1
		}
		return 2
	}
	if pickupCountryID == countrySweden {
		return 2
	}
	return 1
}

// orderingDate is the OrderingDateCalculator result.
type orderingDate struct {
	date                *time.Time
	wasTouchedByHoliday bool
}

func (o orderingDate) eligible(now time.Time) bool {
	// isEligibleForPlacingOrder: hasOrderingDate && !global && date-30min is strictly future
	return o.date != nil && o.date.Add(-30*time.Minute).After(now)
}

func (o orderingDate) canBeUsedForMinimumPickupDateValidation() bool {
	return o.date != nil && o.wasTouchedByHoliday
}

// orderingDateCalc ports OrderingDateCalculator::get for SELECTION (spec 03 §6.3, 07 §1.3).
// pickupDate carries time 00:00:00 (or H:i:s); courierID may be courierNotSet.
func (h holidayCtx) orderingDateCalc(courierID, pickupCountryID int, pickupDate time.Time) orderingDate {
	if isWeekend(pickupDate) {
		return orderingDate{}
	}
	if h.holidayWithCourierException(ymd(pickupDate), pickupCountryID, courierID) {
		return orderingDate{}
	}
	cutoff := h.cutoffTime(stSelection, courierID)
	od := setTimeFromString(pickupDate, cutoff).Add(30 * time.Minute)
	today := startOfDay(h.now().In(backendZone))
	if !ymdEqual(pickupDate, today) {
		od = subWeekdays(od, orderingDayRules(courierID, pickupCountryID))
	}
	courierCountryID := h.snap.CourierCountryByID[courierID]
	isHolidayInCourierCountry := courierCountryID != 0 && h.holidayWithCourierException(ymd(od), courierCountryID, courierID)
	isAbroadOrder := courierCountryID != 0 && pickupCountryID != courierCountryID
	pickupHolidayCheck := isAbroadOrder && orderingDateHolidayCheckRequired[courierID]
	isHolidayInPickupCountry := pickupHolidayCheck && !isHolidayInCourierCountry &&
		h.holidayWithCourierException(ymd(od), pickupCountryID, courierID)
	touched := isHolidayInCourierCountry || isHolidayInPickupCountry
	if touched {
		if !canBeOrderedBeforeHoliday[courierID] {
			return orderingDate{}
		}
		for {
			od = subWeekdays(od, 1)
			blocked := h.holidayWithCourierException(ymd(od), courierCountryID, courierID) ||
				(pickupHolidayCheck && h.holidayWithCourierException(ymd(od), pickupCountryID, courierID))
			if !blocked {
				break
			}
		}
	}
	return orderingDate{date: &od, wasTouchedByHoliday: touched}
}

func ymdEqual(a, b time.Time) bool { return ymd(a) == ymd(b) }

// minPickupDate ports PickupDateService::getMinPickupDate (spec 03 §6.1-6.5).
// currentTime = now in pickup TZ (or server TZ when nil), tz = pickup timezone (may be nil).
func (h holidayCtx) minPickupDate(currentTime time.Time, tz *time.Location, serviceTypeID, pickupCountryID int, courierID *int, isConfirmedBusiness bool) time.Time {
	currentDate := startOfDay(currentTime)
	var affected []int
	if courierID != nil && !acceptsOrdersOnHolidayInCourierCountry(*courierID) {
		affected = []int{h.snap.CourierCountryByID[*courierID], pickupCountryID}
	} else {
		affected = []int{pickupCountryID}
	}
	var earliest time.Time
	switch serviceTypeID {
	case stFlexi, stExpress, stRegularPlus:
		earliest = h.minPickupSameDay(currentTime, tz, courierID, serviceTypeID)
	case stSelection:
		od, pickup := h.orderingAndPickupDate(currentDate, pickupCountryID, courierID)
		if od.canBeUsedForMinimumPickupDateValidation() {
			return pickup // NOTE: skips the final holiday/weekend walk
		}
		earliest = h.minPickupSelection(currentTime, pickupCountryID, courierID)
	case stFreight, stFreightPriority, stFreightPriorityExpress:
		cid := 0
		if courierID != nil {
			cid = *courierID
		}
		earliest = h.minPickupFreight(serviceTypeID, currentTime, cid, affected, isConfirmedBusiness)
	case stVan, stFTL:
		return h.minPickupDistanceBased(currentTime, pickupCountryID, 2)
	case stContainer:
		return h.minPickupDistanceBased(currentTime, pickupCountryID, 3)
	default: // INDIVIDUAL_OFFER etc.
		earliest = addWeekdays(currentDate, 2)
	}
	return startOfDay(h.earliestNonHolidayOrWeekend(earliest, affected))
}

func (h holidayCtx) minPickupDistanceBased(currentTime time.Time, pickupCountryID, days int) time.Time {
	earliest := addWeekdays(startOfDay(currentTime), days)
	return startOfDay(h.earliestNonHolidayOrWeekend(earliest, []int{pickupCountryID}))
}

// minPickupSameDay ports getMinPickupDateForSameDayServices (spec 03 §6.2).
func (h holidayCtx) minPickupSameDay(currentTime time.Time, tz *time.Location, courierID *int, serviceTypeID int) time.Time {
	cid := courierID
	if cid != nil && *cid == courierNotSet {
		cid = nil
	}
	loc := currentTime
	if cid == nil {
		return startOfDay(addWeekdays(loc, 2))
	}
	if tz == nil {
		return startOfDay(addWeekdays(loc, 1))
	}
	loc = loc.In(tz)
	cutoff := setTimeFromString(loc, h.cutoffTime(serviceTypeID, *cid))
	if !sameDayServiceTypes[serviceTypeID] || sameDayDeniedCouriers[*cid] {
		if loc.Before(cutoff) {
			return startOfDay(addWeekdays(loc, 1))
		}
		return startOfDay(addWeekdays(loc, 2))
	}
	if isWeekend(loc) {
		return startOfDay(addWeekdays(loc, 1))
	}
	if !loc.After(cutoff) { // lessThanOrEqualTo
		return startOfDay(loc)
	}
	return startOfDay(addWeekdays(loc, 1))
}

// orderingAndPickupDate ports getOrderingDateAndPickupDate (spec 03 §6.3).
func (h holidayCtx) orderingAndPickupDate(currentDate time.Time, pickupCountryID int, courierID *int) (orderingDate, time.Time) {
	pickup := startOfDay(currentDate.AddDate(0, 0, -1))
	cid := courierNotSet
	if courierID != nil {
		cid = *courierID
	}
	now := h.now().In(backendZone)
	skips := 0
	var od orderingDate
	for {
		pickup = pickup.AddDate(0, 0, 1) // calendar days
		od = h.orderingDateCalc(cid, pickupCountryID, pickup)
		skips++
		if cid == courierNotSet || od.eligible(now) {
			break
		}
	}
	if skips > 1 {
		od.wasTouchedByHoliday = true
	}
	return od, pickup
}

// isNextDayPickupAllowed: office-TZ cutoff not yet past (spec 03 §6.4/6.5).
func (h holidayCtx) isNextDayPickupAllowed(serviceTypeID, courierID int, isConfirmedBusiness bool) bool {
	if serviceTypeID == stFreight {
		if courierID == 28 { // EURO_PALLET_COURIER
			return false
		}
		if courierID == 4 && !isConfirmedBusiness { // DHL_FREIGHT
			return false
		}
	}
	nowOffice := h.now().In(backendZone)
	cutoff := setTimeFromString(startOfDay(nowOffice), h.cutoffTime(serviceTypeID, courierID))
	return !nowOffice.After(cutoff) // !isPast on today's cutoff
}

// minPickupSelection ports getMinPickupDateSelection (spec 03 §6.4).
func (h holidayCtx) minPickupSelection(currentTime time.Time, pickupCountryID int, courierID *int) time.Time {
	cid := courierID
	if cid != nil && *cid == courierNotSet {
		cid = nil
	}
	currentDate := startOfDay(currentTime)
	if cid == nil {
		return addWeekdays(currentDate, 2)
	}
	daysToAdd := orderingDayRules(*cid, pickupCountryID)
	if !h.isNextDayPickupAllowed(stSelection, *cid, false) {
		daysToAdd++
	}
	if daysToAdd == 1 && isWeekend(currentDate) {
		daysToAdd++
	}
	var firstPossible time.Time
	for {
		currentDate = addWeekdays(currentDate, daysToAdd) // mutates each pass (php quirk)
		firstPossible = currentDate
		od := h.orderingDateCalc(*cid, pickupCountryID, firstPossible)
		daysToAdd = 1
		if od.date != nil {
			break
		}
	}
	return firstPossible
}

// minPickupFreight ports getMinPickupDateFreight (spec 03 §6.5). courierID 0 = "has courier" quirk.
func (h holidayCtx) minPickupFreight(serviceTypeID int, currentTime time.Time, courierID int, affected []int, isConfirmedBusiness bool) time.Time {
	available := startOfDay(currentTime)
	if courierID == courierNotSet {
		// php: ($courierId === 10) ? null : $courierId — 10 → null → +2 weekdays
		return addWeekdays(available, 2)
	}
	if serviceTypeID == stFreightPriority || serviceTypeID == stFreightPriorityExpress {
		if isWeekend(available) {
			return startOfDay(addWeekdays(available, 1))
		}
		cutoff := setTimeFromString(currentTime, h.cutoffTime(serviceTypeID, courierID))
		if !currentTime.After(cutoff) {
			return available
		}
		return startOfDay(addWeekdays(available, 1))
	}
	// FREIGHT
	if h.isNextDayPickupAllowed(serviceTypeID, courierID, isConfirmedBusiness) {
		if isWeekend(available) {
			available = addWeekdays(available, 2)
		} else {
			available = addWeekdays(available, 1)
		}
	} else {
		available = addWeekdays(available, 2)
	}
	// findAvailablePickupDateByCheckingOrderingDateOneDayBefore
	for h.isHolidayOn(startOfDay(subWeekdays(available, 1)), affected) {
		available = addWeekdays(available, 1)
	}
	return available
}

// minPickupIO ports getMinPickupDateForIO: server-tz now +2 weekdays stepped past pickup-country
// holidays/weekends (spec 08 §6.3).
func (h holidayCtx) minPickupIO(pickupCountryID int) time.Time {
	earliest := addWeekdays(startOfDay(h.now().In(backendZone)), 2)
	return startOfDay(h.earliestNonHolidayOrWeekend(earliest, []int{pickupCountryID}))
}

// excludedDates ports PickupExcludedDaysProvider::getExcludedDates (spec 07 §1.2).
// courierID/courierCountryID nil for the IO fill-in call. Result: ascending unique midnights.
func (h holidayCtx) excludedDates(pickupCountryID int, courierID, courierCountryID *int, serviceTypeID int, minPickupDate time.Time) []time.Time {
	now := h.now().In(backendZone)
	today := ymd(now)

	// (A) days before minPickupDate: iterate from NOW (with time-of-day) while < minPickupDate
	var out []time.Time
	for d := now; d.Before(minPickupDate); d = d.AddDate(0, 0, 1) {
		out = append(out, startOfDay(d))
	}

	// (B) future holidays of pickup (+courier) country minus exceptional working dates
	var countryIDs []int
	if courierID != nil && !acceptsOrdersOnHolidayInCourierCountry(*courierID) {
		countryIDs = []int{pickupCountryID}
		if courierCountryID != nil && *courierCountryID != 0 {
			countryIDs = append(countryIDs, *courierCountryID)
		}
	} else {
		countryIDs = []int{pickupCountryID}
	}
	var holidays []time.Time
	seen := map[int]bool{}
	for _, cid := range countryIDs {
		if seen[cid] {
			continue
		}
		seen[cid] = true
		for _, hd := range h.snap.HolidaysByCountry[cid] {
			if hd.HolidayDate < today { // holiday_date >= CURDATE()
				continue
			}
			if h.hasExceptionalWorkingDate(hd.HolidayDate, courierID) {
				continue
			}
			t, err := time.ParseInLocation("2006-01-02", hd.HolidayDate, backendZone)
			if err != nil {
				continue
			}
			holidays = append(holidays, t)
		}
	}
	sort.Slice(holidays, func(i, j int) bool { return holidays[i].Before(holidays[j]) })

	// (C) SELECTION only: next weekday after each holiday whose ordering date is not eligible
	var invalidOrdering []time.Time
	if serviceTypeID == stSelection && courierID != nil {
		for _, hd := range holidays {
			next := addWeekdays(hd, 1)
			od := h.orderingDateCalc(*courierID, pickupCountryID, next)
			if !od.eligible(now) {
				invalidOrdering = append(invalidOrdering, next)
			}
		}
	}

	// (D) weekends from now to max(last day of next month, latest holiday)
	lastNextMonth := endOfDay(lastOfMonth(addMonthOverflow(now)))
	bound := lastNextMonth
	if len(holidays) > 0 {
		last := holidays[len(holidays)-1]
		if last.After(lastNextMonth) {
			bound = last
		}
	}
	var weekends []time.Time
	for d := now; d.Before(bound); d = d.AddDate(0, 0, 1) {
		if isWeekend(d) {
			weekends = append(weekends, startOfDay(d))
		}
	}

	merged := append(append(append(out, holidays...), invalidOrdering...), weekends...)
	sort.Slice(merged, func(i, j int) bool { return merged[i].Before(merged[j]) })
	var uniq []time.Time
	var prev string
	for _, t := range merged {
		k := t.Format("2006-01-02 15:04:05")
		if k != prev {
			uniq = append(uniq, t)
			prev = k
		}
	}
	return uniq
}

// hasExceptionalWorkingDate: courierID nil ⇒ ANY courier's active exceptional working date on
// that date cancels the holiday (with date >= today per query B); else exact courier match.
func (h holidayCtx) hasExceptionalWorkingDate(date string, courierID *int) bool {
	if courierID == nil {
		if h.snap.ExceptionsAnyCourier[date] {
			return date >= ymd(h.now().In(backendZone))
		}
		return false
	}
	m := h.snap.ExceptionsByCourier[*courierID]
	return m != nil && m[date]
}

// addMonthOverflow mirrors Carbon addMonth() overflow semantics (= Go AddDate(0,1,0)).
func addMonthOverflow(t time.Time) time.Time { return t.AddDate(0, 1, 0) }

func lastOfMonth(t time.Time) time.Time {
	firstNext := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location()).AddDate(0, 1, 0)
	return firstNext.AddDate(0, 0, -1)
}

func endOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 999999999, t.Location())
}

// edtDates ports QuoteResponseServiceType::setEdtDates: anchored to TODAY in minPickupDate's tz.
func edtDates(now time.Time, edt string, tz *time.Location) (from time.Time, to *time.Time) {
	fromN, toN, hasRange := parseEdtRange(edt)
	base := startOfDay(now.In(tz))
	if hasRange {
		t := addWeekdays(base, toN)
		to = &t
	}
	from = addWeekdays(base, fromN)
	return from, to
}

// parseEdtRange: php explode('-') + (int) casts.
func parseEdtRange(edt string) (from, to int, hasRange bool) {
	var a, b int
	n, _ := fmt.Sscanf(edt, "%d-%d", &a, &b)
	if n >= 2 {
		return a, b, true
	}
	fmt.Sscanf(edt, "%d", &a)
	return a, 0, false
}

// formatFreightEDT ports formatEstimatedDeliveryTime: "4-4" → "4".
func formatFreightEDT(edt string) string {
	var a, b int
	if n, _ := fmt.Sscanf(edt, "%d-%d", &a, &b); n >= 2 && a == b {
		return fmt.Sprintf("%d", a)
	}
	return edt
}

// ftlEDTBands ports FTLEstimatedDeliveryTimeProvider (spec 04 §3.2).
func ftlEDTBands(distanceKm float64) string {
	switch {
	case distanceKm < 500:
		return "1"
	case distanceKm < 1000:
		return "1-2"
	case distanceKm < 1500:
		return "2"
	case distanceKm < 2000:
		return "3"
	case distanceKm < 2500:
		return "3-4"
	case distanceKm < 3000:
		return "4-5"
	default:
		return "4-6"
	}
}
