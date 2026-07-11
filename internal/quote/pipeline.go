// Pipeline: the aggregate dynamic-price provider. Ports DynamicPriceProvider +
// AbstractAggregateDynamicPriceProvider + the per-price calculators (batch mode). Spec: 03.
package quote

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/eurosender/go-ba/internal/pe"
	"github.com/eurosender/go-ba/internal/refdata"
)

// round2 mirrors php 8.4 round($x, 2): decimal-correct rounding of the value's shortest decimal
// representation, half away from zero. A naive math.Round(x*100)/100 diverges on boundaries like
// 8.54*1.25 = 10.674999999999999 (php: 10.67; naive Go: 10.68 because x*100 re-rounds to 1067.5).
func round2(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return x
	}
	s := strconv.FormatFloat(x, 'f', -1, 64)
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	dot := strings.IndexByte(s, '.')
	var cents int64
	if dot < 0 {
		v, _ := strconv.ParseInt(s, 10, 64)
		cents = v * 100
	} else {
		frac := s[dot+1:]
		for len(frac) < 2 {
			frac += "0"
		}
		whole, _ := strconv.ParseInt(s[:dot], 10, 64)
		f2, _ := strconv.ParseInt(frac[:2], 10, 64)
		cents = whole*100 + f2
		if len(frac) > 2 && frac[2] >= '5' {
			cents++
		}
	}
	out := float64(cents) / 100
	if neg {
		out = -out
	}
	return out
}

// applyVat ports VatCalculator::applyVat: 0 if |net|<1e-5 else round2(net*rate).
func applyVat(snap *refdata.Snapshot, net float64, vatRateID int) float64 {
	if math.Abs(net) < 1e-5 {
		return 0.0
	}
	return round2(net * snap.VatRateByID[vatRateID])
}

// respExtra mirrors the php Extras rows attached to a PriceEngineResponse (pickup fees etc).
type respExtra struct {
	extraID int
	net     float64 // value / price_net = sellingPrice
	netCost float64 // courierPrice
}

// peResp is the transformed per-price model (php PriceEngineResponse magic bag, quote scope).
type peResp struct {
	success          bool
	msg              string
	itemsNet         float64
	itemsGross       float64
	itemsCourier     float64
	courierID        *int
	courierCountryID int
	numberOfItems    int
	vatRateID        int
	edt              string
	minPickupDate    string // 'Y-m-d'
	usedPickupDate   *time.Time
	orderingDate     *time.Time
	serviceTypeID    int
	serviceSubtype   string // "" reads as door_to_door
	totalNet         float64
	totalGross       float64
	extras           []respExtra
	flexApplied      bool
	flexNet          float64
	flexGross        float64
	insuranceID      *int
	insuranceNet     float64
	alternatives     []altService
}

func (r *peResp) getSubtype() string {
	if r.serviceSubtype == "" {
		return subD2D
	}
	return r.serviceSubtype
}

func (r *peResp) key() string {
	return fmt.Sprintf("%02d-%s", r.serviceTypeID, r.getSubtype())
}

func (r *peResp) isOnRequest() bool { return !r.success && r.msg == "On request" }

// minPickupCarbon re-parses the 'Y-m-d' string at midnight server tz (php getMinPickupDate()).
func (r *peResp) minPickupCarbon() time.Time {
	t, _ := time.ParseInLocation("2006-01-02", r.minPickupDate, backendZone)
	return t
}

// altService mirrors AlternativeService.
type altService struct {
	serviceTypeID  int
	subtype        string
	priceGross     float64
	priceNet       float64
	courierID      *int
	edt            string
	minPickupDate  time.Time // midnight server tz
	usedPickupDate *time.Time
}

func (a altService) key() string { return fmt.Sprintf("%02d-%s", a.serviceTypeID, a.subtype) }

func altFromResp(r *peResp) altService {
	return altService{
		serviceTypeID: r.serviceTypeID, subtype: r.getSubtype(),
		priceGross: r.itemsGross, priceNet: r.itemsNet,
		courierID: r.courierID, edt: r.edt,
		minPickupDate: r.minPickupCarbon(), usedPickupDate: r.usedPickupDate,
	}
}

// engine bundles everything the pipeline needs.
type engine struct {
	snap            *refdata.Snapshot
	pe              *pe.Client
	now             clock
	versionOverride string
	h               holidayCtx
}

func newEngine(snap *refdata.Snapshot, peClient *pe.Client, now clock, versionOverride string) *engine {
	return &engine{snap: snap, pe: peClient, now: now, versionOverride: versionOverride,
		h: holidayCtx{snap: snap, now: now}}
}

// getDynamicPrice ports DynamicPriceProvider::getDynamicPrice (spec 03 §1).
func (e *engine) getDynamicPrice(ctx context.Context, d *quoteData) *peResp {
	var resp *peResp
	var err error
	if inIntList(packagesSupportedTypes, d.serviceType) {
		resp, err = e.aggregate(ctx, d, packagesSupportedTypes, packagesSupportedPETypes, packagesHierarchy, true)
	} else if inIntList(palletsSupportedTypes, d.serviceType) {
		resp, err = e.aggregate(ctx, d, palletsSupportedTypes, palletsSupportedPETypes, palletsHierarchy, false)
	} else {
		err = fmt.Errorf("service type not supported")
	}
	if err != nil {
		// php: fallback single calculator; VAN/FTL/IO/CONTAINER need HERE/fixed-price data — out of
		// scope: mirror the double-catch result (success=false, msg="On request").
		resp = &peResp{success: false, msg: "On request", vatRateID: d.vatRateID}
	}

	if d.source != "api" && !d.useRequestedServiceTypeOnly {
		resp.alternatives = append(resp.alternatives, e.validCompatibleServices(ctx, resp, d)...)
	}
	return resp
}

func inIntList(l []int, v int) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

// aggregate ports AbstractAggregateDynamicPriceProvider::getDynamicPrice (spec 03 §2).
func (e *engine) aggregate(ctx context.Context, d *quoteData, supported []int, supportedPE map[int]bool, hierarchy []hierarchyEntry, isPackages bool) (*peResp, error) {
	if !inIntList(supported, d.serviceType) {
		return nil, fmt.Errorf("service type not supported")
	}
	prices := e.multipleAvailablePrices(ctx, d, supported, supportedPE, isPackages)
	if len(prices) == 0 {
		return &peResp{success: false, vatRateID: d.vatRateID}, nil
	}
	return e.filterAndSelect(ctx, d, prices, hierarchy), nil
}

// datasetItem is one clone of the multi-price dataset.
type datasetItem struct {
	serviceType int
	optionals   []int // ES types
	pickupDate  *time.Time
}

// multipleAvailablePrices ports getMultipleAvailablePrices (spec 03 §2.4).
func (e *engine) multipleAvailablePrices(ctx context.Context, d *quoteData, supported []int, supportedPE map[int]bool, isPackages bool) []*peResp {
	var specific []int
	if d.pickupDate != nil {
		if d.source == "api" {
			specific = append(specific, supported...)
		} else {
			specific = []int{d.serviceType}
			if isPackages && (d.serviceType == stExpress || d.serviceType == stRegularPlus) {
				specific = append(specific, stExpress, stRegularPlus)
				specific = uniqueInts(specific)
			}
		}
	}
	var def []int
	for _, t := range supported {
		if !inIntList(specific, t) {
			def = append(def, t)
		}
	}
	if !d.serviceTypeIsDetected && d.useRequestedServiceTypeOnly {
		def = intersectInts(def, []int{d.serviceType})
		specific = intersectInts(specific, []int{d.serviceType})
	}

	var items []datasetItem
	if len(def) > 0 {
		items = append(items, datasetItem{serviceType: def[0], optionals: def[1:]})
	}
	if len(specific) > 0 {
		pd := *d.pickupDate
		tz := d.pickupTimeZone
		if tz == nil {
			tz = backendZone
		}
		todayTZ := startOfDay(e.now().In(tz))
		if pd.Before(todayTZ) {
			pd = todayTZ
		}
		items = append(items, datasetItem{serviceType: specific[0], optionals: specific[1:], pickupDate: &pd})
	}

	// One batch POST per item; item isolation on errors.
	var raw []pe.Price
	for _, it := range items {
		req := e.buildPERequest(d, it.serviceType, it.optionals, it.pickupDate, nil, false)
		resp, err := e.pe.QuoteServices(ctx, req)
		if err != nil {
			continue // NoPriceError or transport: item contributes nothing
		}
		raw = append(raw, resp.Prices...)
	}
	if len(raw) == 0 {
		return nil
	}

	// addMinPickupDateToPrices (§6): memo per esType|courier
	type withInfo struct {
		price     pe.Price
		minPickup time.Time
	}
	memo := map[string]time.Time{}
	var priced []withInfo
	for _, p := range raw {
		esType := mapPEToESType(p.ServiceTypeID)
		key := fmt.Sprintf("%d|%d", esType, p.CourierID)
		mp, ok := memo[key]
		if !ok {
			tz := d.pickupTimeZone
			nowIn := e.now().In(backendZone)
			if tz != nil {
				nowIn = e.now().In(tz)
			}
			cid := p.CourierID
			mp = e.h.minPickupDate(nowIn, d.pickupTimeZone, esType, d.pickupCountryID, &cid, false)
			memo[key] = mp
		}
		priced = append(priced, withInfo{price: p, minPickup: mp})
	}

	// ensurePricesAreNotBeforeMinPickupDate (§7): re-query prices whose PE date < min pickup
	var kept []withInfo
	type requeryKey struct{ peType, courier int }
	requeries := map[requeryKey]time.Time{}
	var requeryOrder []requeryKey
	for _, wi := range priced {
		peDate := parsePEDate(wi.price.PickupDate)
		if peDate != nil && !wi.minPickup.After(*peDate) {
			kept = append(kept, wi)
			continue
		}
		k := requeryKey{wi.price.ServiceTypeID, wi.price.CourierID}
		if _, ok := requeries[k]; !ok {
			requeries[k] = wi.minPickup
			requeryOrder = append(requeryOrder, k)
		}
	}
	for _, k := range requeryOrder {
		mp := requeries[k]
		req := e.buildPERequestForPEType(d, k.peType, nil, &mp, []int{k.courier})
		resp, err := e.pe.QuoteServices(ctx, req)
		if err != nil {
			continue
		}
		for _, p := range resp.Prices {
			if p.ServiceTypeID == k.peType && p.CourierID == k.courier {
				kept = append(kept, withInfo{price: p, minPickup: mp})
			}
		}
	}

	// Transform (§8), keeping success && !onRequest, PE order preserved.
	var out []*peResp
	for _, wi := range kept {
		if !supportedPE[wi.price.ServiceTypeID] {
			continue
		}
		r := e.transform(ctx, d, wi.price, wi.minPickup)
		if r != nil && r.success && !r.isOnRequest() {
			out = append(out, r)
		}
	}
	return out
}

func parsePEDate(s string) *time.Time {
	if s == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return &t
	}
	return nil
}

func uniqueInts(in []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func intersectInts(a, b []int) []int {
	var out []int
	for _, v := range a {
		if inIntList(b, v) {
			out = append(out, v)
		}
	}
	return out
}

// transform ports the per-price calculators (§8). minPickup = the AdditionalInfo date.
func (e *engine) transform(ctx context.Context, d *quoteData, dto pe.Price, minPickup time.Time) *peResp {
	esType := mapPEToESType(dto.ServiceTypeID)
	subtype := mapPEToESSubtype(dto.ServiceTypeID)

	// validateParameters (§8.1) — express-with-pallets short-circuit
	if esType == stExpress && d.containsPallets() {
		return &peResp{success: false, msg: "Express with pallets will always return no price", vatRateID: d.vatRateID}
	}
	freightFamily := esType == stFreight || esType == stFreightPriority || esType == stFreightPriorityExpress
	if freightFamily && dto.CourierPrice.Price.TotalPrice == 0 && dto.CourierPrice.CourierName == "" {
		return &peResp{success: false, msg: "On request", vatRateID: d.vatRateID}
	}

	cid := dto.CourierID
	r := &peResp{
		success:          true,
		courierID:        &cid,
		courierCountryID: e.snap.CourierCountryByID[cid],
		numberOfItems:    len(d.items),
		vatRateID:        d.vatRateID,
		serviceTypeID:    esType,
		serviceSubtype:   subtype,
		minPickupDate:    ymd(minPickup),
		usedPickupDate:   parsePEDate(dto.PickupDate),
		itemsCourier:     dto.CourierPrice.Price.TotalCost,
	}

	// EDT
	if freightFamily {
		edt := dto.EstimatedDeliveryTime
		if edt == "" {
			edt = "5-8"
		}
		r.edt = formatFreightEDT(edt)
	} else {
		r.edt = dto.EstimatedDeliveryTime
	}

	// itemsNet
	base := dto.CourierPrice.Price.TotalPrice
	switch {
	case esType == stSelection:
		// SELECTION: ordering-date eligibility (§8.3)
		od := e.h.orderingDateCalc(cid, d.pickupCountryID, startOfDay(derefPEDate(dto.PickupDate)))
		if !od.eligible(e.now().In(backendZone)) {
			// exclude courier for SELECTION and re-query once
			return e.selectionRequery(ctx, d, dto, minPickup)
		}
		r.orderingDate = od.date
		r.itemsNet = base
	case freightFamily:
		r.itemsNet = base
	default: // FLEXI / REG+ / EXPRESS packages
		feeSum := 0.0
		for _, ec := range dto.ExtraCharges {
			if (ec.Name == "sameDayPickupFee" || ec.Name == "nextDayPickupFee") && e.autoFeeApplies(dto.PickupDate, ec.Name) {
				feeSum += ec.SellingPrice
			}
		}
		r.itemsNet = round2(base + round2(feeSum))
	}
	r.itemsGross = applyVat(e.snap, r.itemsNet, d.vatRateID)

	// insurance (§8.4.2): guest quotes with value-less parcels → free/no insurance, net 0.
	r.insuranceID = d.insuranceID

	// flexible booking (§8.4.3)
	if d.hasFlexibleBooking {
		net := round2(math.Max(base*0.25, 8.54))
		if d.accountType == "company" {
			net = 0 // company users pay 0 (guest company accountType has user==nil in php: net stays computed — but company accounts are auth-only; corpus is guests)
		}
		r.flexApplied = true
		r.flexNet = net
		r.flexGross = applyVat(e.snap, net, d.vatRateID)
	}

	// pickup-fee extras (§8.4.5) — packages incl. SELECTION
	if !freightFamily {
		for _, ec := range dto.ExtraCharges {
			if ec.SellingPrice > 0 {
				switch ec.Name {
				case "sameDayPickupFee":
					r.extras = append(r.extras, respExtra{extraID: extraSameDayFee, net: ec.SellingPrice, netCost: ec.CourierPrice})
				case "nextDayPickupFee":
					r.extras = append(r.extras, respExtra{extraID: extraNextDayFee, net: ec.SellingPrice, netCost: ec.CourierPrice})
				}
			}
		}
	}

	// totals (§8.7)
	total := base
	if r.flexApplied {
		total += r.flexNet
	}
	countByID := map[int]int{}
	for _, id := range d.extraIDs {
		countByID[id]++
	}
	for _, ex := range r.extras {
		if n, ok := countByID[ex.extraID]; ok {
			total += ex.net * float64(n)
		}
		if e.autoFeeAppliesByID(r.usedPickupDate, ex.extraID) {
			total += ex.net
		}
	}
	total = round2(total)
	r.totalNet = total
	r.totalGross = applyVat(e.snap, total, d.vatRateID)
	return r
}

func derefPEDate(s string) time.Time {
	if t := parsePEDate(s); t != nil {
		return *t
	}
	return time.Time{}
}

// autoFeeApplies ports ExtraPickupFeeDecisionMaker::shouldBeAutomaticallyAppliedBasedOnExtraName.
func (e *engine) autoFeeApplies(peDate string, name string) bool {
	t := parsePEDate(peDate)
	if t == nil {
		return false
	}
	now := e.now().In(t.Location())
	switch name {
	case "sameDayPickupFee":
		return ymdEqual(*t, now)
	case "nextDayPickupFee":
		wd := now.Weekday()
		if (wd == time.Friday || wd == time.Saturday || wd == time.Sunday) && t.Weekday() == time.Monday {
			return true
		}
		return ymdEqual(*t, now.AddDate(0, 0, 1))
	}
	return false
}

func (e *engine) autoFeeAppliesByID(used *time.Time, extraID int) bool {
	if used == nil {
		return false
	}
	switch extraID {
	case extraSameDayFee:
		return e.autoFeeApplies(used.Format(time.RFC3339), "sameDayPickupFee")
	case extraNextDayFee:
		return e.autoFeeApplies(used.Format(time.RFC3339), "nextDayPickupFee")
	}
	return false
}

// selectionRequery ports the §8.3 SELECTION recursion: exclude the courier for SELECTION and
// re-query the PE for the same (type, subtype); recursion continues via transform.
func (e *engine) selectionRequery(ctx context.Context, d *quoteData, dto pe.Price, minPickup time.Time) *peResp {
	d2 := *d
	d2.excludedCouriersPerService = copyExcluded(d.excludedCouriersPerService)
	d2.excludedCouriersPerService[stSelection] = append(append([]int{}, d.excludedCouriersPerService[stSelection]...), dto.CourierID)
	selectedPE := dto.ServiceTypeID // same SELECTION subtype variant
	req := e.buildPERequestForPEType(&d2, selectedPE, []int{}, nil, nil)
	resp, err := e.pe.QuoteServices(ctx, req)
	if err != nil {
		return &peResp{success: false, msg: "On request", vatRateID: d.vatRateID}
	}
	for _, p := range resp.Prices {
		if p.ServiceTypeID == selectedPE {
			return e.transform(ctx, &d2, p, minPickup)
		}
	}
	return &peResp{success: false, msg: "On request", vatRateID: d.vatRateID}
}

func copyExcluded(m map[int][]int) map[int][]int {
	out := make(map[int][]int, len(m))
	for k, v := range m {
		out[k] = append([]int{}, v...)
	}
	return out
}

// filterAndSelect ports getPriceEngineResponseWithFilteredAlternativeServices (§9).
// Courier filtering (§10) is a php no-op for zip/city-less quotes; FedEx live checks are not
// portable — treated as all-supported (documented divergence for fully-addressed FedEx cases).
func (e *engine) filterAndSelect(ctx context.Context, d *quoteData, prices []*peResp, hierarchy []hierarchyEntry) *peResp {
	filtered := priceFilter(prices, hierarchy)
	selected := e.selectedResponse(ctx, d, filtered)
	if selected == nil {
		selected = &peResp{success: false, vatRateID: d.vatRateID} // key() = "00-door_to_door", matches nothing
	}
	var alts []altService
	for _, r := range filtered {
		if r.key() != selected.key() {
			alts = append(alts, altFromResp(r))
		}
	}
	selected.alternatives = alts
	return selected
}

// priceFilter ports ServiceTypePriceFilter::getFilteredPrices (§9.1).
func priceFilter(prices []*peResp, hierarchy []hierarchyEntry) []*peResp {
	// group by subtype, first-encounter order
	var groupOrder []string
	groups := map[string][]*peResp{}
	for _, r := range prices {
		st := r.getSubtype()
		if _, ok := groups[st]; !ok {
			groupOrder = append(groupOrder, st)
		}
		groups[st] = append(groups[st], r)
	}
	var out []*peResp
	for _, st := range groupOrder {
		group := groups[st]
		priceByType := map[int]float64{}
		for _, r := range group {
			priceByType[r.serviceTypeID] = r.itemsGross // last wins
		}
		surviving := map[int]bool{}
		for t := range priceByType {
			surviving[t] = true
		}
		for _, h := range hierarchy {
			comparing, hasComparing := priceByType[h.important]
			if !hasComparing || comparing == 0.0 || !surviving[h.important] {
				// php: $comparingPrice truthy check — 0.0/missing disables; note deleted types
				// keep participating as "comparing" only via the map (deletion removes them)
				if !surviving[h.important] {
					continue
				}
				if !hasComparing || comparing == 0.0 {
					continue
				}
			}
			for _, cheaper := range h.cheaper {
				sp := 0.0
				if surviving[cheaper] {
					sp = priceByType[cheaper]
				}
				if comparing < sp {
					delete(surviving, cheaper)
					delete(priceByType, cheaper)
				}
			}
		}
		for _, r := range group {
			if surviving[r.serviceTypeID] {
				out = append(out, r)
			}
		}
	}
	return out
}

// selectedResponse ports getSelectedPriceEngineResponse (§9.2).
func (e *engine) selectedResponse(ctx context.Context, d *quoteData, filtered []*peResp) *peResp {
	for _, r := range filtered {
		if r.serviceTypeID == d.serviceType && r.getSubtype() == d.getServiceSubtype() {
			return r
		}
	}
	if !d.serviceTypeIsDetected {
		return nil
	}
	if d.pickupDate == nil {
		if len(filtered) > 0 {
			return filtered[0]
		}
		return nil
	}
	// pickupDate + detected: re-price candidates one at a time
	for _, r := range filtered {
		req := e.buildPERequest(d, r.serviceTypeID, nil, d.pickupDate, nil, true)
		req.SelectedServiceType = mapESToPEType(r.serviceTypeID, r.getSubtype())
		resp, err := e.pe.QuoteServices(ctx, req)
		if err != nil {
			continue
		}
		for _, p := range resp.Prices {
			if p.ServiceTypeID == req.SelectedServiceType {
				tz := d.pickupTimeZone
				nowIn := e.now().In(backendZone)
				if tz != nil {
					nowIn = e.now().In(tz)
				}
				cid := p.CourierID
				mp := e.h.minPickupDate(nowIn, tz, r.serviceTypeID, d.pickupCountryID, &cid, false)
				cand := e.transform(ctx, d, p, mp)
				if cand != nil && cand.success {
					return cand
				}
			}
		}
	}
	return nil
}

// validCompatibleServices ports getValidCompatibleServices (§11.1).
func (e *engine) validCompatibleServices(ctx context.Context, main *peResp, d *quoteData) []altService {
	var compatible []altService
	switch {
	case d.containsOnlyPallets():
		compatible = e.compatibleFor(ctx, d, stSelection, "package")
	case d.containsOnlyPackages() && anyFreightBillable(d.items):
		compatible = e.compatibleFor(ctx, d, stFreight, "pallet")
	default:
		return nil
	}
	if len(compatible) == 0 {
		return nil
	}
	if !main.success {
		return compatible
	}
	if d.containsOnlyPackages() {
		return nil // package→pallet cross-sell suppressed on success
	}
	// prepareMaxNetPriceForServices: max(0, main total-net if d2d, alt items-nets with d2d)
	maxNet := 0.0
	if main.getSubtype() == subD2D && main.totalNet > maxNet {
		maxNet = main.totalNet
	}
	for _, a := range main.alternatives {
		if a.subtype == subD2D && a.priceNet > maxNet {
			maxNet = a.priceNet
		}
	}
	var out []altService
	for _, c := range compatible {
		if c.priceNet < maxNet {
			out = append(out, c)
		}
	}
	return out
}

func anyFreightBillable(items []parcel) bool {
	for _, p := range items {
		if (p.width*p.length*p.height)/4000 > 30 {
			return true
		}
	}
	return false
}

// compatibleFor ports getCompatibleServicesFor: retype parcels, run the matching aggregate fully.
func (e *engine) compatibleFor(ctx context.Context, d *quoteData, serviceType int, parcelType string) []altService {
	d2 := *d
	d2.pickupDate = nil
	d2.serviceType = serviceType
	sub := subD2D
	d2.serviceSubtypeRaw = &sub
	d2.serviceTypeIsDetected = true
	d2.useRequestedServiceTypeOnly = false
	items := make([]parcel, len(d.items))
	copy(items, d.items)
	for i := range items {
		items[i].ptype = parcelType
	}
	d2.items = items
	var resp *peResp
	var err error
	if inIntList(packagesSupportedTypes, serviceType) {
		resp, err = e.aggregate(ctx, &d2, packagesSupportedTypes, packagesSupportedPETypes, packagesHierarchy, true)
	} else {
		resp, err = e.aggregate(ctx, &d2, palletsSupportedTypes, palletsSupportedPETypes, palletsHierarchy, false)
	}
	if err != nil {
		return nil
	}
	if resp.success {
		return append([]altService{altFromResp(resp)}, resp.alternatives...)
	}
	return resp.alternatives
}

// buildPERequest builds the canonical batch request DTO for a dataset item (spec 03 §3).
// selectedES = the item's serviceType (ES); optionals = ES types; pickupDate nil = default;
// specificCouriers for the §7 re-query; subtypeOnly toggles the §9.2 non-exploding mapping.
func (e *engine) buildPERequest(d *quoteData, selectedES int, optionals []int, pickupDate *time.Time, specificCouriers []int, subtypeOnly bool) *pe.Request {
	var optionalPE []int
	if subtypeOnly {
		optionalPE = mapESTypesToPETypes(optionals, d.getServiceSubtype())
	} else {
		optionalPE = explodeESTypesToPETypes(optionals, selectedES, d.getServiceSubtype())
	}
	if optionalPE == nil {
		optionalPE = []int{}
	}
	req := e.buildPERequestForPEType(d, mapESToPEType(selectedES, d.getServiceSubtype()), optionalPE, pickupDate, specificCouriers)
	return req
}

// buildPERequestForPEType: selected already PE-typed.
func (e *engine) buildPERequestForPEType(d *quoteData, selectedPE int, optionalPE []int, pickupDate *time.Time, specificCouriers []int) *pe.Request {
	version := d.priceVersion
	if e.versionOverride != "" {
		version = e.versionOverride
	}
	if optionalPE == nil {
		optionalPE = []int{}
	}
	if specificCouriers == nil {
		specificCouriers = []int{}
	}

	// parcels buckets (spec 03 §3.1)
	parcels := pe.Parcels{
		AllParcels: []map[string]any{}, Envelopes: []map[string]any{}, Packages: []map[string]any{},
		Pallets: []map[string]any{}, Vans: []map[string]any{}, Trucks: []map[string]any{},
		NonStandard: []map[string]any{}, Containers: []map[string]any{},
	}
	for _, p := range d.items {
		switch p.ptype {
		case "package":
			m := map[string]any{
				"type": "package", "optionalServices": nil, "weight": p.weight,
				"length": p.length, "width": p.width, "height": p.height,
				"groupId": p.groupID, "isStackable": true, "quantity": 1,
			}
			parcels.Packages = append(parcels.Packages, m)
		case "pallet", "euro-pallet":
			m := map[string]any{
				"type": "pallet", "optionalServices": nil, "weight": p.weight,
				"length": p.length, "width": p.width, "height": p.height,
				"groupId": p.groupID, "isStackable": p.isStackable, "quantity": 1,
			}
			parcels.Pallets = append(parcels.Pallets, m)
		case "envelope":
			m := map[string]any{
				"type": "envelope", "optionalServices": nil, "weight": p.weight,
				"groupId": p.groupID, "isStackable": true, "quantity": 1,
			}
			parcels.Envelopes = append(parcels.Envelopes, m)
		case "small-van", "large-van":
			m := map[string]any{"type": p.ptype, "optionalServices": []any{}, "groupId": p.groupID}
			parcels.Vans = append(parcels.Vans, m)
		case "full-truck-load", "less-than-truck-load":
			m := map[string]any{"type": p.ptype, "ldm": p.cargoQuantity, "weight": p.weight, "groupId": p.groupID}
			parcels.Trucks = append(parcels.Trucks, m)
		}
	}
	parcels.AllParcels = concatParcels(parcels.Envelopes, parcels.Packages, parcels.Pallets, parcels.Vans, parcels.Trucks, parcels.NonStandard, parcels.Containers)

	// route (§3.2)
	pickup := e.snap.CountriesByID[d.pickupCountryID]
	delivery := e.snap.CountriesByID[d.deliveryCountryID]
	pickupCode, deliveryCode := "", ""
	isEU := false
	if pickup != nil {
		pickupCode = pickup.Code
	}
	if delivery != nil {
		deliveryCode = delivery.Code
	}
	if pickup != nil && delivery != nil {
		isEU = isEuPair(pickup, delivery)
	}
	tzName := ""
	if d.pickupTimeZone != nil {
		tzName = d.pickupTimeZone.String()
	}

	// client (§3.3): guest path
	accountType := "guest"
	switch d.customerType {
	case "business":
		accountType = "company"
	case "individual":
		accountType = "person"
	}

	// pickup date + holiday context (§3.5)
	calc := d.pickupDate
	if pickupDate != nil {
		calc = pickupDate
	}
	var calcDate time.Time
	if calc != nil {
		calcDate = *calc
	} else {
		calcDate = e.h.defaultPickupDate(d.pickupCountryID)
	}
	dateYMD := ymd(calcDate)
	onHoliday := []string{}
	for _, h := range e.snap.HolidaysByDate[dateYMD] {
		if c := e.snap.CountriesByID[h.PickupCountryID]; c != nil {
			onHoliday = append(onHoliday, c.Code)
		}
	}
	now := e.now().In(backendZone)
	year := now.Format("2006")
	today := ymd(now)
	holidaysOnPickup := []string{}
	for _, h := range e.snap.HolidaysByCountry[d.pickupCountryID] {
		if h.HolidayDate >= today && h.HolidayDate[:4] == year {
			holidaysOnPickup = append(holidaysOnPickup, h.HolidayDate)
		}
	}

	// excluded couriers distributed to PE types (§3.6)
	excluded := map[string][]int{}
	for _, esType := range excludedCouriersKeyOrder {
		ids := d.excludedCouriersPerService[esType]
		if ids == nil {
			ids = []int{}
		}
		for _, peType := range explodeESTypesToPETypes([]int{esType}, 0, "") {
			excluded[fmt.Sprintf("%d", peType)] = ids
		}
	}

	return &pe.Request{
		Version:              version,
		Parcels:              parcels,
		Client:               pe.ClientInfo{AccountType: accountType},
		ExcludedCourierIDs:   excluded,
		PickupDate:           calcDate.Format("2006-01-02T15:04:05-07:00"),
		SelectedServiceType:  selectedPE,
		OptionalServiceTypes: optionalPE,
		Route: pe.Route{
			PickupAddress: pe.Address{Zip: d.pickupZip, City: d.pickupCity, RegionID: d.pickupRegionID,
				CountryID: d.pickupCountryID, TimeZoneName: tzName, Country2IsoCode: pickupCode},
			DeliveryAddress: pe.Address{Zip: d.deliveryZip, City: d.deliveryCity, RegionID: d.deliveryRegionID,
				CountryID: d.deliveryCountryID, Country2IsoCode: deliveryCode},
			IsEU: isEU,
		},
		CountriesOnHolidayForPickup: onHoliday,
		HolidaysOnPickupCountry:     holidaysOnPickup,
		Tags:                        []string{},
		SpecificCourierIDs:          specificCouriers,
	}
}

func concatParcels(lists ...[]map[string]any) []map[string]any {
	out := []map[string]any{}
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}
