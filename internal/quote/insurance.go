// Insurance maps: basic (free), additional (paid), recommendation. Ports
// QuoteResponseInsuranceHandler + BasicInsuranceProvider + AdditionalInsuranceProvider.
// Spec: 05. Guest scope note: courier-specific paid insurances require shipment value > 0 —
// value-less corpus quotes never produce them; the value-carrying branch is TODO (spec 05 §5.3).
package quote

import (
	"fmt"
	"sort"
	"strings"
)

// insurance mirrors the php Insurance value object (quote-relevant fields).
type insurance struct {
	extraID    int
	text       string
	minValue   float64
	coverage   float64 // maxValueOfShipment
	netPrice   float64
	grossPrice float64
	perPackage bool
	// converted trio (non-EUR requests; spec 05 §7.3)
	convSymbol *string
	convNet    float64
	convGross  float64
}

func (i *insurance) json() map[string]any {
	return map[string]any{
		"id":       i.extraID,
		"coverage": i.coverage,
		"text":     i.text,
		"price":    priceJSON("EUR", i.grossPrice, i.netPrice, i.convSymbol, convPtr(i.convSymbol, i.convGross), convPtr(i.convSymbol, i.convNet)),
	}
}

// numberFormatThousands mirrors php number_format($v, 0, ',', '.'): 0 decimals (half-up),
// '.' thousands separator — 1000 → "1.000".
func numberFormatThousands(v float64) string {
	n := int64(v + 0.5)
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	return strings.Join(parts, ".")
}

// EN translations (WORKING_LANG=en; other languages: translation-table port pending).
const (
	txtInsuranceCoverage = "Insurance coverage of up to"
	txtPerPackage        = "(per package)"
	txtCMR               = "10 euros per 1kg gross weight"
)

// buildInsuranceText ports AbstractInsuranceProvider::buildInsuranceText.
func buildInsuranceText(coverage float64, perPackage bool) string {
	suffix := ""
	if perPackage {
		suffix = " " + txtPerPackage
	}
	return fmt.Sprintf("%s %s €%s", txtInsuranceCoverage, numberFormatThousands(coverage), suffix)
}

func (d *quoteData) totalWeight() float64 {
	w := 0.0
	for _, p := range d.items {
		w += p.weight
	}
	return round2(w)
}

// freeInsuranceOption ports BasicInsuranceProvider::getFreeInsuranceOption (spec 05 §4).
func (e *engine) freeInsuranceOption(serviceTypeID int, courierID *int, pickupCountryID, deliveryCountryID int, totalWeight float64) *insurance {
	switch serviceTypeID {
	case stSelection, stFlexi:
		coverage := 200.0
		if courierID != nil && *courierID == 109 { // PARCELFORCE: zero-price package's max
			for _, p := range e.snap.InsuranceByInsurer[109] {
				if p.PriceExclVat.Valid && p.PriceExclVat.Float64 == 0.0 {
					coverage = float64(int(p.MaxContent())) // php float→int coercion
					break
				}
			}
		}
		var extraID *int
		if courierID != nil {
			extraID = e.freeInsuranceExtraID(*courierID, pickupCountryID, deliveryCountryID)
		} else {
			z := 0
			extraID = &z
		}
		if extraID == nil {
			return nil
		}
		return &insurance{
			extraID: *extraID, text: buildInsuranceText(coverage, true),
			minValue: 0, coverage: coverage, perPackage: true,
		}
	case stVan, stFTL, stFreight, stFreightPriority, stFreightPriorityExpress, stExpress, stRegularPlus:
		pkg := e.snap.PackageByExtraID[extraInsuranceCMR]
		minVal := 0.0
		if pkg != nil {
			minVal = pkg.MinContentValue
		}
		return &insurance{
			extraID: extraInsuranceCMR, text: txtCMR,
			minValue: minVal, coverage: round2(10 * totalWeight), perPackage: false,
		}
	}
	return nil
}

// freeInsuranceExtraID ports getFreeInsuranceExtraId (courier switch + zero-price default).
func (e *engine) freeInsuranceExtraID(courierID, pickupCountryID, deliveryCountryID int) *int {
	id := func(v int) *int { return &v }
	switch courierID {
	case 1: // DPD_SLOVENIA
		if pickupCountryID == countrySlovenia && (deliveryCountryID == countrySlovenia || deliveryCountryID == countryCroatia) {
			return id(30)
		}
		return id(21)
	case 4:
		return id(22)
	case 34:
		return id(42)
	case 36:
		return id(43)
	case 50:
		return id(58)
	case 94:
		return id(85)
	case 124:
		return nil // Chronopost: deliberately none
	}
	if ex, ok := e.snap.FreeInsuranceExtraByCourier[courierID]; ok {
		return id(ex)
	}
	return nil
}

// generalInsuranceOptions ports getGeneralInsuranceOptions + the validity gate (spec 05 §5.1/5.2).
func (e *engine) generalInsuranceOptions(d *quoteData, serviceTypeID int, courierID *int, numberOfItems int) []*insurance {
	// gate: envelopes, global route, Parcelforce/Post-Luxembourg
	for _, p := range d.items {
		if p.ptype == "envelope" {
			return nil
		}
	}
	if d.isGlobalRouteFlag {
		return nil
	}
	if courierID != nil && (*courierID == 109 || *courierID == 142) {
		return nil
	}
	build := func(extraID int, multiply bool, perPackage bool) *insurance {
		extra := e.snap.ExtrasByID[extraID]
		pkg := e.snap.PackageByExtraID[extraID]
		if extra == nil || pkg == nil {
			return nil
		}
		net := 0.0
		if extra.Value.Valid {
			net = extra.Value.Float64
		}
		if multiply {
			net *= float64(numberOfItems)
		}
		return &insurance{
			extraID: extraID, text: buildInsuranceText(pkg.MaxContent(), perPackage),
			minValue: pkg.MinContentValue, coverage: pkg.MaxContent(),
			netPrice: net, grossPrice: applyVat(e.snap, net, d.vatRateID), perPackage: perPackage,
		}
	}
	var out []*insurance
	switch serviceTypeID {
	case stSelection, stFlexi, stRegularPlus, stExpress:
		for _, id := range []int{19, 20} {
			if ins := build(id, true, true); ins != nil {
				out = append(out, ins)
			}
		}
	case stIndividualOffer, stFreight, stFreightPriority, stFreightPriorityExpress:
		for _, id := range []int{34, 35, 36} {
			if ins := build(id, false, false); ins != nil {
				out = append(out, ins)
			}
		}
	}
	return out
}

// additionalInsuranceOptions ports getApplicableAdditionalInsuranceOptions: courier-specific
// first (requires shipment value > 0 — none for value-less quotes), then generals sorted
// ascending by netPrice (ties: reversed original order, php comparator never returns 0).
func (e *engine) additionalInsuranceOptions(d *quoteData, serviceTypeID int, courierID *int, numberOfItems int) []*insurance {
	generals := e.generalInsuranceOptions(d, serviceTypeID, courierID, numberOfItems)
	type indexed struct {
		ins *insurance
		idx int
	}
	tmp := make([]indexed, len(generals))
	for i, g := range generals {
		tmp[i] = indexed{g, i}
	}
	sort.SliceStable(tmp, func(a, b int) bool {
		if tmp[a].ins.netPrice != tmp[b].ins.netPrice {
			return tmp[a].ins.netPrice < tmp[b].ins.netPrice
		}
		return tmp[a].idx > tmp[b].idx // tie → reversed original order
	})
	out := make([]*insurance, 0, len(tmp))
	// courier-specific insurances would go FIRST here; they require totalValue > 0
	// (spec 05 §5.3 common bail-out) — TODO when value-carrying quotes are in scope.
	for _, t := range tmp {
		out = append(out, t.ins)
	}
	return out
}

// recommendedInsuranceID ports selectRecommendedInsuranceId (spec 05 §3.3).
func recommendedInsuranceID(additional []*insurance, basic *insurance, shipmentValue, maxParcelValue float64) *int {
	basicCoverage := 0.0
	if basic != nil {
		basicCoverage = basic.coverage
	}
	var recommendedPrice *float64
	var recommendedID *int
	var notExactDiff *float64
	var notExactID *int
	for _, ins := range additional {
		target := shipmentValue
		if ins.perPackage {
			target = maxParcelValue
		}
		if target <= basicCoverage {
			continue
		}
		if target >= ins.minValue {
			if target <= ins.coverage {
				if recommendedPrice == nil || *recommendedPrice > ins.netPrice {
					p, id := ins.netPrice, ins.extraID
					recommendedPrice, recommendedID = &p, &id
				}
			} else {
				diff := target - ins.coverage
				if notExactDiff == nil || *notExactDiff > diff {
					dcopy, id := diff, ins.extraID
					notExactDiff, notExactID = &dcopy, &id
				}
			}
		}
	}
	if recommendedID != nil {
		return recommendedID
	}
	return notExactID
}

// insuranceMaps builds the three maps keyed "%02d-%s" (main first, union: main wins).
func (e *engine) insuranceMaps(d *quoteData, r *peResp) (basic map[string]*insurance, additional map[string][]*insurance, recommended map[string]int) {
	basic = map[string]*insurance{}
	additional = map[string][]*insurance{}
	recommended = map[string]int{}
	if !r.success {
		return
	}
	totalWeight := d.totalWeight()
	numberOfItems := len(d.items)

	// php: alt maps assign per-alternative (last alt wins per key), then main + altMap union
	// (main wins). Replicate with a separate alt pass followed by the main overwrite.
	for _, a := range r.alternatives {
		if b := e.freeInsuranceOption(a.serviceTypeID, a.courierID, d.pickupCountryID, d.deliveryCountryID, totalWeight); b != nil {
			basic[a.key()] = b
		} else {
			delete(basic, a.key())
		}
		if list := e.additionalInsuranceOptions(d, a.serviceTypeID, a.courierID, numberOfItems); len(list) > 0 {
			additional[a.key()] = list
		} else {
			delete(additional, a.key())
		}
	}
	// main union wins even with a null value (union keeps main's null, array_filter drops the key)
	if b := e.freeInsuranceOption(r.serviceTypeID, r.courierID, d.pickupCountryID, d.deliveryCountryID, totalWeight); b != nil {
		basic[r.key()] = b
	} else {
		delete(basic, r.key())
	}
	if list := e.additionalInsuranceOptions(d, r.serviceTypeID, r.courierID, numberOfItems); len(list) > 0 {
		additional[r.key()] = list
	} else {
		delete(additional, r.key())
	}

	shipmentValue := d.getTotalValue()
	maxParcelValue := d.getMaxParcelValue()
	for key, list := range additional {
		if id := recommendedInsuranceID(list, basic[key], shipmentValue, maxParcelValue); id != nil {
			recommended[key] = *id
		}
	}

	// non-EUR: converted trio on every insurance (basic prices are 0 → converted 0, spec 05 §7.3)
	if d.currencyID != 1 {
		sym := currencyCodeByID[d.currencyID]
		setConv := func(i *insurance) {
			s := sym
			i.convSymbol = &s
			i.convNet = convertAmount(e.snap, i.netPrice, d.currencyID)
			i.convGross = convertAmount(e.snap, i.grossPrice, d.currencyID)
		}
		for _, b := range basic {
			setConv(b)
		}
		for _, list := range additional {
			for _, ins := range list {
				setConv(ins)
			}
		}
	}
	return
}
