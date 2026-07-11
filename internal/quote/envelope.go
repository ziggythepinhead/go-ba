// Envelope: ServiceTypeProvider part-B collapse + QuoteResponseOptions rendering + data.order.
// Specs: 04, 07, 08. Insurance maps arrive in a later round (nil/empty placeholders here).
package quote

import (
	"fmt"
	"math"
	"strings"
	"time"
)

const rfc3339 = "2006-01-02T15:04:05-07:00"

// serviceTypeDetails mirrors ServiceTypeDetails.
type serviceTypeDetails struct {
	serviceTypeID                      int
	subtype                            string
	minPickupDate                      time.Time
	courierID                          *int
	isLabelRequired                    bool
	isCallRequired                     bool
	isPickupSenderAddressRequired      bool
	isDeliveryRecipientAddressRequired bool
	edt                                *string  // only IO/default entries carry one
	upgradePaths                       []string // target subtypes, [d2s, s2d, s2s] order filtered
}

func (d *serviceTypeDetails) key() string {
	return fmt.Sprintf("%02d-%s", d.serviceTypeID, d.subtype)
}

// buildDetails ports ServiceTypeProvider::buildServiceTypeDetails flags (spec 04 §3/§7).
func (e *engine) buildDetails(serviceTypeID int, subtype string, courierID *int, pickupCountryID int, minPickup time.Time) serviceTypeDetails {
	d := serviceTypeDetails{
		serviceTypeID: serviceTypeID, subtype: subtype, minPickupDate: minPickup, courierID: courierID,
	}
	d.isLabelRequired = e.isLabelRequired(courierID, pickupCountryID, serviceTypeID, subtype)
	d.isCallRequired = e.isCallRequired(courierID, pickupCountryID, serviceTypeID, subtype)
	pudoPickup := subtype == subS2D || subtype == subS2S
	pudoDelivery := subtype == subD2S || subtype == subS2S
	if courierID != nil {
		d.isPickupSenderAddressRequired = pudoPickup && pickupSenderAddrRequiredWhenPudo[*courierID]
		d.isDeliveryRecipientAddressRequired = pudoDelivery && deliveryRecipientAddrRequiredWhenPudo[*courierID]
	}
	return d
}

// isLabelRequired ports LabelRestrictionsChecker (spec 04 §7.1).
func (e *engine) isLabelRequired(courierID *int, pickupCountryID, serviceTypeID int, subtype string) bool {
	if courierID == nil {
		return false
	}
	c := *courierID
	if pickupCountryID == countrySweden {
		return true
	}
	if !couriersWithLabel[c] {
		return false
	}
	if c == 94 && pickupCountryID != countryLuxembourg {
		return false
	}
	if c == 7 && pickupCountryID != countryRomania {
		return false
	}
	if serviceTypeID == stSelection {
		if subtype == subS2D || subtype == subS2S {
			return true
		}
		if c == 23 || c == 2 || c == 6 || c == 143 || c == 1 || c == 26 {
			return false
		}
	}
	return true
}

// isCallRequired ports CourierPickupRestrictions (spec 04 §7.2).
func (e *engine) isCallRequired(courierID *int, pickupCountryID, serviceTypeID int, subtype string) bool {
	if courierID == nil {
		return false
	}
	if serviceTypeID != stExpress && serviceTypeID != stRegularPlus {
		return false
	}
	if subtype == subS2D || subtype == subS2S {
		return false
	}
	c := *courierID
	if !dhlExpressGroup[c] && !upsGroup[c] {
		return false
	}
	effective := c
	if upsGroup[c] {
		effective = 87
	}
	return e.snap.PickupBlockedByCountryCourier[[2]int{pickupCountryID, effective}]
}

// providerGet ports ServiceTypeProvider::get (spec 04 §3).
func (e *engine) providerGet(d *quoteData, r *peResp) []serviceTypeDetails {
	if !inIntList(availableServiceTypesOrdered, d.serviceType) {
		return nil
	}
	type hkey struct {
		subtype string
		typeID  int
	}
	hierarchy := map[hkey]serviceTypeDetails{}
	add := func(typeID int, subtype string, courierID *int, minPickup time.Time) {
		hierarchy[hkey{subtype, typeID}] = e.buildDetails(typeID, subtype, courierID, d.pickupCountryID, minPickup)
	}
	if r.success {
		add(r.serviceTypeID, r.getSubtype(), r.courierID, r.minPickupCarbon())
	}
	for _, a := range r.alternatives {
		add(a.serviceTypeID, a.subtype, a.courierID, a.minPickupDate)
	}
	// addSubtypeUpgradePaths
	for k, det := range hierarchy {
		if k.subtype != subD2D {
			continue
		}
		var ups []string
		for _, target := range upgradesTo {
			if _, ok := hierarchy[hkey{target, k.typeID}]; ok {
				ups = append(ups, target)
			}
		}
		det.upgradePaths = ups
		hierarchy[k] = det
	}
	var out []serviceTypeDetails
	for _, subtype := range subtypeOrdered {
		for _, typeID := range availableServiceTypesOrdered {
			if det, ok := hierarchy[hkey{subtype, typeID}]; ok {
				out = append(out, det)
			}
		}
	}
	return out
}

// providerGetForIO ports getForIndividualOffer (spec 08 §6.3).
func (e *engine) providerGetForIO(d *quoteData, alternatives []altService) []serviceTypeDetails {
	edtOf := func(s string) *string { return &s }
	ioMin := e.h.minPickupIO(d.pickupCountryID)
	if d.containsVan() {
		det := serviceTypeDetails{serviceTypeID: stVan, subtype: subD2D, minPickupDate: ioMin, edt: edtOf("1-3")}
		return []serviceTypeDetails{det}
	}
	if d.containsTrucks() {
		det := serviceTypeDetails{serviceTypeID: stFTL, subtype: subD2D, minPickupDate: ioMin, edt: edtOf(ftlEDTBands(0))}
		return []serviceTypeDetails{det}
	}
	if d.containsOnlyEnvelopes() {
		det := serviceTypeDetails{serviceTypeID: stExpress, subtype: subD2D, minPickupDate: ioMin,
			isLabelRequired: true, edt: edtOf("1-2")}
		return []serviceTypeDetails{det}
	}
	var out []serviceTypeDetails
	hasPackage, hasFreight := false, false
	for _, a := range alternatives {
		switch a.serviceTypeID {
		case stVan, stFTL, stContainer, stRail:
			out = append(out, serviceTypeDetails{serviceTypeID: a.serviceTypeID, subtype: subD2D,
				minPickupDate: a.minPickupDate, courierID: a.courierID})
		default:
			out = append(out, e.buildDetails(a.serviceTypeID, a.subtype, a.courierID, d.pickupCountryID, a.minPickupDate))
		}
		switch a.serviceTypeID {
		case stSelection, stRegularPlus, stFlexi, stExpress:
			hasPackage = true
		case stFreight, stFreightPriority, stFreightPriorityExpress:
			hasFreight = true
		}
	}
	if d.containsPallets() && !hasFreight {
		out = append(out, serviceTypeDetails{serviceTypeID: stFreight, subtype: subD2D, minPickupDate: ioMin, edt: edtOf("5-8")})
	} else if !d.containsPallets() && !hasPackage {
		out = append(out,
			serviceTypeDetails{serviceTypeID: stSelection, subtype: subD2D, minPickupDate: ioMin, edt: edtOf("3")},
			serviceTypeDetails{serviceTypeID: stExpress, subtype: subD2D, minPickupDate: ioMin, isLabelRequired: true, edt: edtOf("1-2")})
	}
	return out
}

// tcLinks: language pick per row CSV, unconditional en fallback.
func tcLink(link, languagesCSV, lang string) string {
	l := "en"
	for _, cand := range strings.Split(languagesCSV, ",") {
		if cand == lang {
			l = lang
			break
		}
	}
	return fmt.Sprintf("%s_%s.pdf", link, l)
}

// courierTCLink ports TermsAndConditionsLinks::getLinks courier side (spec 07 §2.2).
func (e *engine) courierTCLink(serviceTypeID int, courierID *int, lang string) string {
	if serviceTypeID == stVan || serviceTypeID == stFTL || serviceTypeID == stContainer || serviceTypeID == stRail {
		return ""
	}
	cid := courierNotSet
	if courierID != nil {
		cid = *courierID
	}
	if cid == courierNotSet {
		return ""
	}
	row := e.snap.ActiveTermsByCourier[cid]
	if row == nil {
		return ""
	}
	return tcLink(row.Link, row.Languages, lang)
}

// price JSON helper: {"original": {...}, "converted": null|{...}} with abs + 2dp.
func priceJSON(currency string, gross, net float64, convCurrency *string, convGross, convNet *float64) map[string]any {
	orig := map[string]any{
		"currencyCode": currency,
		"gross":        round2(absF(gross)),
		"net":          round2(absF(net)),
	}
	var converted any
	if convCurrency != nil && convGross != nil && convNet != nil {
		converted = map[string]any{
			"currencyCode": *convCurrency,
			"gross":        round2(absF(*convGross)),
			"net":          round2(absF(*convNet)),
		}
	}
	return map[string]any{"original": orig, "converted": converted}
}

func absF(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// addOn is a rendered QuoteResponseAddOn.
type addOn struct {
	code  string
	price map[string]any
}

// buildAddOnsMap ports QuoteResponseAddonsHandler for the guest path: extras-as-addons of the
// main response (spec 06 summary; flexible-booking only when applied). Keyed [courier][type].
func (e *engine) buildAddOnsMap(d *quoteData, r *peResp) map[int]map[int][]addOn {
	out := map[int]map[int][]addOn{}
	if !r.success || r.courierID == nil {
		return out
	}
	appendAddon := func(courierID, typeID int, a addOn) {
		if out[courierID] == nil {
			out[courierID] = map[int][]addOn{}
		}
		out[courierID][typeID] = append(out[courierID][typeID], a)
	}
	// flexibleChanges OFFER (prepareFlexibleBookingAddons): unconditional for guests on supported
	// service types, priced off the MAIN response's itemsNetPrice, keyed under the MAIN courierId
	// for main + every alternative serviceType (php quirks preserved: duplicates per subtype,
	// alternatives never visible when their entry courier differs from the main's).
	flexSupported := map[int]bool{stSelection: true, stRegularPlus: true, stFlexi: true, stExpress: true,
		stFreight: true, stFreightPriority: true, stFreightPriorityExpress: true}
	// main addon keyed under the REQUESTED/DETECTED service type (php:
	// $addOn[$per->getCourierId()][$dynamicPriceData->getServiceType()]) — invisible when the
	// detected type never renders; alternatives keyed under their own type, main's courier.
	if flexSupported[d.serviceType] {
		net := round2(math.Max(0.25*r.itemsNet, 8.54))
		gross := applyVat(e.snap, net, d.vatRateID)
		var flexConvSym *string
		var flexConvGross, flexConvNet *float64
		if d.currencyID != 1 {
			sym := currencyCodeByID[d.currencyID]
			cg, cn := convertAmount(e.snap, gross, d.currencyID), convertAmount(e.snap, net, d.currencyID)
			flexConvSym, flexConvGross, flexConvNet = &sym, &cg, &cn
		}
		p := priceJSON("EUR", gross, net, flexConvSym, flexConvGross, flexConvNet)
		appendAddon(*r.courierID, d.serviceType, addOn{code: "flexibleChanges", price: p})
		for _, a := range r.alternatives {
			appendAddon(*r.courierID, a.serviceTypeID, addOn{code: "flexibleChanges", price: p})
		}
	}
	// extras-as-addons: extras with mapped codes; Price always carries a converted block (EUR→EUR too)
	for _, ex := range r.extras {
		code, ok := extraIDToAddonCode[ex.extraID]
		if !ok {
			continue
		}
		gross := applyVat(e.snap, ex.net, d.vatRateID)
		sym := currencyCodeByID[d.currencyID]
		g, n := convertAmount(e.snap, gross, d.currencyID), convertAmount(e.snap, ex.net, d.currencyID)
		appendAddon(*r.courierID, r.serviceTypeID, addOn{code: code,
			price: priceJSON("EUR", gross, ex.net, &sym, &g, &n)})
	}
	return out
}

// buildEnvelope assembles the whole /api/v2/quote response body.
func (e *engine) buildEnvelope(d *quoteData, r *peResp, lang string, selectedServiceTypeSet bool) map[string]any {
	// serviceTypesDetails dispatch (spec 04 §2 + 08 §6.3)
	var details []serviceTypeDetails
	if r.success {
		switch d.serviceType {
		case stIndividualOffer:
			details = e.providerGetForIO(d, r.alternatives)
		case stVan, stFTL:
			details = []serviceTypeDetails{{serviceTypeID: d.serviceType, subtype: subD2D,
				minPickupDate: r.minPickupCarbon(), courierID: r.courierID}}
		default:
			details = e.providerGet(d, r)
		}
	} else if d.source != "api" && d.source != "shopify" {
		details = e.providerGetForIO(d, r.alternatives)
	}

	// excluded dates (spec 07 §1.1)
	nonWorking := map[int][]time.Time{}
	if r.success {
		selCourierCountry := r.courierCountryID
		nonWorking[d.serviceType] = e.h.excludedDates(d.pickupCountryID, r.courierID, &selCourierCountry, d.serviceType, r.minPickupCarbon())
		for _, a := range r.alternatives {
			var ccPtr *int
			if a.courierID != nil {
				cc := e.snap.CourierCountryByID[*a.courierID]
				ccPtr = &cc
			}
			if _, ok := nonWorking[a.serviceTypeID]; ok && a.serviceTypeID == d.serviceType {
				continue // selected entry wins (php + union)
			}
			nonWorking[a.serviceTypeID] = e.h.excludedDates(d.pickupCountryID, a.courierID, ccPtr, a.serviceTypeID, a.minPickupDate)
		}
	}
	for _, det := range details {
		if _, ok := nonWorking[det.serviceTypeID]; !ok {
			nonWorking[det.serviceTypeID] = e.h.excludedDates(d.pickupCountryID, nil, nil, stIndividualOffer, det.minPickupDate)
		}
	}

	// alternativeServicesByKey / allServicesByKey (spec 04 §4)
	altByKey := map[string]altService{}
	for _, a := range r.alternatives {
		altByKey[a.key()] = a // last wins
	}
	allByKey := map[string]altService{}
	for k, v := range altByKey {
		allByKey[k] = v
	}
	if r.success {
		if _, ok := allByKey[r.key()]; !ok {
			allByKey[r.key()] = altFromResp(r)
		}
	}

	addOnsMap := map[int]map[int][]addOn{}
	tcByTypeSubtype := map[int]map[string]string{}
	if r.success {
		addOnsMap = e.buildAddOnsMap(d, r)
		for _, det := range details {
			if tcByTypeSubtype[det.serviceTypeID] == nil {
				tcByTypeSubtype[det.serviceTypeID] = map[string]string{}
			}
			tcByTypeSubtype[det.serviceTypeID][det.subtype] = e.courierTCLink(det.serviceTypeID, det.courierID, lang)
		}
	}

	basicIns, additionalIns, recommendedIns := e.insuranceMaps(d, r)

	now := e.now()
	serviceTypes := make([]any, 0, len(details))
	for _, det := range details {
		key := det.key()
		var price any
		var usedPickupDate any
		var edt string
		if a, ok := altByKey[key]; ok {
			price = priceJSON("EUR", a.priceGross, a.priceNet, a.convSymbol, convPtr(a.convSymbol, a.convGross), convPtr(a.convSymbol, a.convNet))
			edt = a.edt
			if a.usedPickupDate != nil {
				usedPickupDate = a.usedPickupDate.Format(rfc3339)
			}
		} else if r.success && r.key() == key {
			price = mainPriceJSON(r, r.itemsGross, r.itemsNet, r.convItemsGross, r.convItemsNet)
			if det.edt != nil {
				edt = *det.edt
			} else {
				edt = r.edt
			}
			if r.usedPickupDate != nil {
				usedPickupDate = r.usedPickupDate.Format(rfc3339)
			}
		} else if det.edt != nil {
			edt = *det.edt
		}

		var edtDateFrom, edtDateTo any
		if edt != "" {
			from, to := edtDates(now, edt, det.minPickupDate.Location())
			edtDateFrom = from.Format(rfc3339)
			if to != nil {
				edtDateTo = to.Format(rfc3339)
			}
		}

		excl := []any{}
		for _, t := range nonWorking[det.serviceTypeID] {
			excl = append(excl, t.Format(rfc3339))
		}

		var entryAddOns []any = []any{}
		if det.courierID != nil {
			for _, a := range addOnsMap[*det.courierID][det.serviceTypeID] {
				if a.code == "fedexInternationalPriorityExpress" && det.subtype != subD2D {
					continue
				}
				entryAddOns = append(entryAddOns, map[string]any{"code": a.code, "price": a.price})
			}
		}

		var tcl any
		if m, ok := tcByTypeSubtype[det.serviceTypeID]; ok {
			if v, ok2 := m[det.subtype]; ok2 {
				tcl = v
			}
		}

		ups := []any{}
		for _, target := range det.upgradePaths {
			upKey := fmt.Sprintf("%02d-%s", det.serviceTypeID, target)
			var discount any
			if base, ok := allByKey[key]; ok {
				if upgraded, ok2 := allByKey[upKey]; ok2 {
					netDiff := round2(round2(base.priceNet) - round2(upgraded.priceNet))
					if netDiff > 0 {
						grossDiff := round2(round2(base.priceGross) - round2(upgraded.priceGross))
						var cSym *string
						var cGross, cNet *float64
						if base.convSymbol != nil {
							cn := round2(round2(base.convNet) - round2(upgraded.convNet))
							cg := round2(round2(base.convGross) - round2(upgraded.convGross))
							cSym, cGross, cNet = base.convSymbol, &cg, &cn
						}
						discount = priceJSON("EUR", grossDiff, netDiff, cSym, cGross, cNet)
					}
				}
			}
			ups = append(ups, map[string]any{
				"serviceTypeId": det.serviceTypeID, "serviceSubtype": target,
				"serviceNameKey": upKey, "discount": discount,
			})
		}

		var courierIDVal any
		if det.courierID != nil {
			courierIDVal = *det.courierID
		}

		serviceTypes = append(serviceTypes, map[string]any{
			"id":                                 det.serviceTypeID,
			"serviceSubtype":                     det.subtype,
			"serviceNameKey":                     key,
			"minPickupDate":                      det.minPickupDate.Format(rfc3339),
			"usedPickupDate":                     usedPickupDate,
			"isCallRequired":                     det.isCallRequired,
			"isLabelRequired":                    det.isLabelRequired,
			"edt":                                edt,
			"edtDateFrom":                        edtDateFrom,
			"edtDateTo":                          edtDateTo,
			"price":                              price,
			"pickupDateFee":                      nil,
			"basicInsurance":                     basicInsJSON(basicIns[key]),
			"additionalInsurances":               additionalInsJSON(additionalIns[key]),
			"recommendedAdditionalInsuranceId":   recommendedInsJSON(recommendedIns, key),
			"pickupExcludedDates":                excl,
			"addOns":                             entryAddOns,
			"courierTermsAndConditionsLink":      tcl,
			"upgradePaths":                       ups,
			"courierId":                          courierIDVal,
			"requiresShipmentValue":              requiresShipmentValue(det.courierID, det.serviceTypeID),
			"isPickupSenderAddressRequired":      det.isPickupSenderAddressRequired,
			"isDeliveryRecipientAddressRequired": det.isDeliveryRecipientAddressRequired,
			"pickupTimeFrameSelectionPossible":   isPickupTimeFrameSelectionPossible(det.courierID),
			"otherPickupDatePrices":              []any{},
		})
	}

	// paymentMethods (guest, success only)
	paymentMethods := []any{}
	if r.success {
		currencyID := 1 // response currency: converted symbol when non-EUR (PaymentMethodsTrait)
		if r.hasConverted {
			currencyID = d.currencyID
		}
		for _, code := range []string{"paypal", "credit_card", "apple_pay", "google_pay", "bank"} {
			paymentMethods = append(paymentMethods, map[string]any{
				"code":            code,
				"paymentDiscount": nil,
				"switchToEur":     !paymentSupportedCurrencies[code][currencyID],
				"paymentGateway":  paymentGatewayByCode[code],
				"newPrice":        nil,
			})
		}
	}

	generalTC := ""
	if e.snap.ActiveTerms != nil {
		generalTC = tcLink(e.snap.ActiveTerms.Link, e.snap.ActiveTerms.Languages, lang)
	}

	var order any
	if selectedServiceTypeSet && r.success {
		order = e.buildOrder(d, r, basicIns[r.key()], additionalIns[r.key()])
	}

	return map[string]any{
		"jsonApi": map[string]any{"version": "2.0"},
		"data": map[string]any{
			"options": map[string]any{
				"paymentMethods":                paymentMethods,
				"parcelLevelOptionalServices":   []any{},
				"parcelTransportTypePrices":     []any{},
				"serviceTypes":                  serviceTypes,
				"pricePerKm":                    nil,
				"truckOptions":                  nil,
				"vatRate":                       e.snap.VatRateByID[d.vatRateID],
				"exchangeRate":                  exchangeRateJSON(r),
				"generalTermsAndConditionsLink": generalTC,
				"isGlobalRoute":                 d.isGlobalRouteFlag,
			},
			"order":    order,
			"warnings": []any{},
		},
	}
}

func basicInsJSON(b *insurance) any {
	if b == nil {
		return nil
	}
	return b.json()
}

func additionalInsJSON(list []*insurance) []any {
	out := []any{}
	for _, i := range list {
		out = append(out, i.json())
	}
	return out
}

func recommendedInsJSON(m map[string]int, key string) any {
	if id, ok := m[key]; ok {
		return id
	}
	return nil
}

// buildOrder ports QuoteResponseOrder (spec 08 §4). Guest scope: coupon/paymentDiscount nil.
func (e *engine) buildOrder(d *quoteData, r *peResp, basic *insurance, additional []*insurance) map[string]any {
	// parcels: de-quantified customer ids, first occurrence wins, item order
	parcels := []any{}
	seen := map[string]bool{}
	for _, p := range d.items {
		id := p.groupID
		if i := strings.Index(id, "#"); i >= 0 {
			id = id[:i]
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		parcels = append(parcels, map[string]any{"parcelId": id, "optionalServiceIds": []any{}})
	}
	addOns := []any{}
	if r.flexApplied {
		addOns = append(addOns, "flexibleChanges")
	}
	for _, ex := range r.extras {
		if ex.extraID == extraFedexIPE {
			addOns = append(addOns, "fedexInternationalPriorityExpress")
			break
		}
	}
	serviceType := r.serviceTypeID
	if serviceType == 0 {
		serviceType = d.serviceType
	}
	minPickup, _ := time.ParseInLocation("2006-01-02", r.minPickupDate, backendZone)
	var courierIDVal any
	if r.courierID != nil {
		courierIDVal = *r.courierID
	}
	var basicID any
	if basic != nil {
		basicID = basic.extraID
	}
	// additionalInsuranceId: request insuranceId echoed iff it matches an offered additional (§11.3)
	var additionalID any
	if r.insuranceID != nil {
		for _, ins := range additional {
			if ins.extraID == *r.insuranceID {
				additionalID = *r.insuranceID
				break
			}
		}
	}
	return map[string]any{
		"totalPrice":            mainPriceJSON(r, r.totalGross, r.totalNet, r.convTotalGross, r.convTotalNet),
		"basicInsuranceId":      basicID,
		"additionalInsuranceId": additionalID,
		"coupon":                nil,
		"parcels":               parcels,
		"paymentDiscount":       nil,
		"addOns":                addOns,
		"serviceType":           serviceType,
		"serviceSubtype":        r.getSubtype(),
		"minPickupDate":         minPickup.Format(rfc3339),
		"pickupDateFeeId":       nil,
		"estimatedDeliveryTime": r.edt,
		"courierId":             courierIDVal,
		"courierTag":            nil,
		"calculatedDistance":    nil,
	}
}

// convPtr returns &v only when the converted symbol is set (EUR requests keep converted null).
func convPtr(sym *string, v float64) *float64 {
	if sym == nil {
		return nil
	}
	return &v
}

// mainPriceJSON renders a main-response Price with its converted mirror when present.
func mainPriceJSON(r *peResp, gross, net, convGross, convNet float64) map[string]any {
	if !r.hasConverted {
		return priceJSON("EUR", gross, net, nil, nil, nil)
	}
	sym := r.convertedSymbol
	return priceJSON("EUR", gross, net, &sym, &convGross, &convNet)
}

// exchangeRateJSON: options.exchangeRate = the modified rate for non-EUR, null for EUR.
func exchangeRateJSON(r *peResp) any {
	if !r.hasConverted {
		return nil
	}
	return r.exchangeRate
}
