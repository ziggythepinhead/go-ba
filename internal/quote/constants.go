// Constants ported verbatim from the php backend (modules/dlabs/options/*, provider constants).
// Sources: CourierOptions.php, FedexCourierOptions.php, UpsCourierOptions.php,
// DhlExpressCourierOptions.php, KuehneNagelCourierOptions.php, ServiceTypeOptions.php,
// ServiceTypeProvider.php, Aggregate*PriceProvider.php. See ai/go-be/specs/ for provenance.
package quote

// ServiceTypeOptions
const (
	stSelection              = 1
	stFlexi                  = 2
	stFreight                = 4
	stIndividualOffer        = 7
	stExpress                = 8
	stVan                    = 9
	stFTL                    = 10
	stRegularPlus            = 11
	stFreightPriority        = 12
	stFreightPriorityExpress = 13
	stContainer              = 14
	stRail                   = 15
)

const (
	subD2D = "door_to_door"
	subD2S = "door_to_shop"
	subS2D = "shop_to_door"
	subS2S = "shop_to_shop"
)

const courierNotSet = 10

// Country ids (CountryOptions)
const (
	countryUS          = 1
	countryCanada      = 2
	countryCroatia     = 53
	countryIreland     = 103
	countryItaly       = 105
	countryLuxembourg  = 125
	countryNorway      = 161
	countryPortugal    = 172
	countryRomania     = 176
	countrySlovenia    = 191
	countrySpain       = 196
	countrySweden      = 204
	countrySwitzerland = 205
	countryUK          = 223
)

func setOf(ids ...int) map[int]bool {
	m := make(map[int]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// Courier groups (FedexCourierOptions::getAllCouriers = PRIORITY ∪ ECONOMY ∪ REGIONAL_ECONOMY)
var fedexGroup = setOf(
	97, 100, 105, 107, 112, 140, 2356, 2347, // PRIORITY
	98, 101, 2353, 2344, // ECONOMY (140 already present)
	106, 108, 113, 116, 122, 117, 115, 118, 119, 125, 120, 127, 128, 129, 130, 131, 132, 126, 175, // REGIONAL_ECONOMY
)

var upsGroup = setOf(66, 67, 68, 69, 70, 71, 72, 73, 74, 75, 76, 77, 78, 79, 80, 81, 82, 83, 84, 85,
	110, 111, 133, 134, 135, 136, 137, 138, 139)

var dhlExpressGroup = setOf(46, 144)

var kuehneNagelGroup = setOf(14, 25, 15, 22, 16, 19, 20, 21, 30, 8, 17, 18, 29)

// COURIERS_WITH_LABEL = UPS ∪ FEDEX ∪ DHL_EXPRESS ∪ KN ∪ singles
var couriersWithLabel = func() map[int]bool {
	m := map[int]bool{}
	for _, g := range []map[int]bool{upsGroup, fedexGroup, dhlExpressGroup, kuehneNagelGroup} {
		for id := range g {
			m[id] = true
		}
	}
	for _, id := range []int{9, 4, 50, 40, 59, 64, 11, 7, 93, 94, 13, 96, 1, 26, 2, 6, 109, 31, 124, 142, 34, 143, 23, 174} {
		m[id] = true
	}
	return m
}()

// acceptsOrdersOnHolidayInCourierCountry ⇔ FedEx ∪ UPS ∪ DHL-Express
func acceptsOrdersOnHolidayInCourierCountry(courierID int) bool {
	return fedexGroup[courierID] || upsGroup[courierID] || dhlExpressGroup[courierID]
}

func isPickupTimeFrameSelectionPossible(courierID *int) bool {
	return courierID != nil && fedexGroup[*courierID]
}

// canBeOrderedBeforeHoliday (CourierOptions)
var canBeOrderedBeforeHoliday = setOf(24, 114, 95, 7, 31, 33, 6, 2, 38, 9, 32)

// orderingDateAllowedOnHolidayInPickupCountryForAbroadOrder returns FALSE for these couriers
// (i.e. the pickup-country holiday check IS required).
var orderingDateHolidayCheckRequired = setOf(26, 96, 94, 93, 11, 13, 7, 1, 31, 140, 105, 107, 112, 97, 100, 99, 3)

// isSameDayPickupAllowedForCourier: denied couriers (PickupDateService)
var sameDayDeniedCouriers = setOf(124, 13, 11, 31) // CHRONOPOST, DPD_PT, DPD_PL, FAN

// SAME_DAY_SERVICE_TYPES {FLEXI, REGULAR_PLUS, EXPRESS, FREIGHT_PRIORITY, FPE, VAN}
var sameDayServiceTypes = setOf(stFlexi, stRegularPlus, stExpress, stFreightPriority, stFreightPriorityExpress, stVan)

// requiresShipmentValue courier sets per serviceType (CourierOptions::requiresShipmentValue)
func requiresShipmentValue(courierID *int, serviceTypeID int) bool {
	if courierID == nil {
		return false
	}
	c := *courierID
	switch serviceTypeID {
	case stRegularPlus:
		return fedexGroup[c] || upsGroup[c] || c == 124 || c == 64 || c == 144
	case stExpress:
		return fedexGroup[c] || upsGroup[c] || c == 124 || c == 59 || c == 46 || c == 144
	case stFreight:
		return fedexGroup[c] || c == 46 || c == 144
	case stFreightPriority:
		return fedexGroup[c] || c == 144
	case stFreightPriorityExpress:
		return fedexGroup[c] || c == 46 || c == 144
	}
	return false
}

// PUDO address-required flags (CourierOptions)
var pickupSenderAddrRequiredWhenPudo = func() map[int]bool {
	m := map[int]bool{13: true, 1: true}
	for id := range fedexGroup {
		m[id] = true
	}
	return m
}()

var deliveryRecipientAddrRequiredWhenPudo = func() map[int]bool {
	m := map[int]bool{46: true, 144: true, 13: true, 1: true, 142: true, 174: true}
	for id := range fedexGroup {
		m[id] = true
	}
	return m
}()

// PE type maps (PriceEngineServiceTypeMapper)
var peSubtypeSeries = map[int][4]int{ // esType -> [d2d, d2s, s2d, s2s] PE ids
	stSelection:   {1, 101, 102, 103},
	stFlexi:       {2, 201, 202, 203},
	stExpress:     {8, 801, 802, 803},
	stRegularPlus: {11, 1101, 1102, 1103},
}

var subtypeIndex = map[string]int{subD2D: 0, subD2S: 1, subS2D: 2, subS2S: 3}
var subtypeByIndex = [4]string{subD2D, subD2S, subS2D, subS2S}

func mapESToPEType(esType int, subtype string) int {
	if series, ok := peSubtypeSeries[esType]; ok {
		if idx, ok := subtypeIndex[subtype]; ok {
			return series[idx]
		}
	}
	return esType
}

func mapPEToESType(peType int) int {
	switch {
	case peType >= 1100:
		return stRegularPlus
	case peType >= 800 && peType < 900:
		return stExpress
	case peType >= 200 && peType < 300:
		return stFlexi
	case peType >= 100 && peType < 200:
		return stSelection
	}
	return peType
}

func mapPEToESSubtype(peType int) string {
	if peType > 100 {
		switch peType % 100 {
		case 1:
			return subD2S
		case 2:
			return subS2D
		case 3:
			return subS2S
		}
	}
	return subD2D
}

// explodeESTypesToPETypes ports explodeMultipleEurosenderTypesToPriceEngineTypes.
func explodeESTypesToPETypes(types []int, selectedType int, selectedSubtype string) []int {
	var out []int
	selectedInTypes := false
	for _, t := range types {
		if t == selectedType {
			selectedInTypes = true
		}
		if series, ok := peSubtypeSeries[t]; ok {
			out = append(out, series[0], series[1], series[2], series[3])
		} else {
			out = append(out, t)
		}
	}
	if series, ok := peSubtypeSeries[selectedType]; ok && !selectedInTypes {
		selIdx := subtypeIndex[selectedSubtype]
		for i := 0; i < 4; i++ {
			if i != selIdx {
				out = append(out, series[i])
			}
		}
	}
	return out
}

// mapESTypesToPETypes ports mapMultipleEurosenderTypesToPriceEngineTypes (same subtype, no explosion).
func mapESTypesToPETypes(types []int, subtype string) []int {
	out := make([]int, 0, len(types))
	for _, t := range types {
		out = append(out, mapESToPEType(t, subtype))
	}
	return out
}

// Aggregate providers
var packagesSupportedTypes = []int{stFlexi, stSelection, stRegularPlus, stExpress}
var packagesSupportedPETypes = setOf(2, 1, 11, 8, 101, 102, 103, 1101, 1102, 1103, 801, 802, 803, 201, 202, 203)
var packagesHierarchy = []hierarchyEntry{
	{stExpress, []int{stRegularPlus, stFlexi}},
	{stRegularPlus, []int{stFlexi}},
	{stSelection, nil},
	{stFlexi, nil},
}

var palletsSupportedTypes = []int{stFreight, stFreightPriority, stFreightPriorityExpress}
var palletsSupportedPETypes = setOf(4, 12, 13)
var palletsHierarchy = []hierarchyEntry{
	{stFreightPriorityExpress, []int{stFreightPriority, stFreight}},
	{stFreightPriority, []int{stFreight}},
	{stFreight, nil},
}

type hierarchyEntry struct {
	important int
	cheaper   []int
}

// ServiceTypeProvider orderings
var availableServiceTypesOrdered = []int{stFlexi, stSelection, stRegularPlus, stFreight, stFreightPriority, stFreightPriorityExpress, stExpress}
var subtypeOrdered = []string{subD2D, subS2D, subD2S, subS2S}
var upgradesTo = []string{subD2S, subS2D, subS2S}

// excludedCouriersPerService key order (CourierExclusionService)
var excludedCouriersKeyOrder = []int{stRegularPlus, stFreight, stFreightPriority, stFreightPriorityExpress, stExpress, stSelection, stFlexi}

// Addon codes (OrderLevelAddonOptions / ExtraOptions)
const (
	extraFedexIPE        = 150
	extraFlexibleBooking = 38
	extraSameDayFee      = 171
	extraNextDayFee      = 172
	extraInsuranceCMR    = 17
)

var extraIDToAddonCode = map[int]string{
	extraFedexIPE:        "fedexInternationalPriorityExpress",
	extraFlexibleBooking: "flexibleChanges",
	extraSameDayFee:      "sameDayPickupFee",
	extraNextDayFee:      "nextDayPickupFee",
}

// Currencies (CurrencyOptions)
var currencyIDByCode = map[string]int{"EUR": 1, "HRK": 2, "CZK": 3, "DKK": 4, "GBP": 5, "PLN": 6, "SEK": 7, "RON": 8, "USD": 12}

// Supported currency ids per payment code (getSupportedCurrenciesForPaymentType)
var paymentSupportedCurrencies = map[string]map[int]bool{
	"credit_card": setOf(1, 3, 4, 5, 6, 7),
	"apple_pay":   setOf(1, 3, 4, 5, 6, 7),
	"google_pay":  setOf(1, 3, 4, 5, 6, 7),
	"bank":        setOf(1),
	"paypal":      setOf(1, 4, 6, 7, 3, 5),
	"credit":      setOf(1),
	"deferred":    setOf(1, 12),
}

var paymentGatewayByCode = map[string]string{
	"credit_card": "braintree", "paypal": "braintree", "google_pay": "braintree", "apple_pay": "braintree",
	"credit": "usercredit", "deferred": "deferred", "bank": "trustpay",
}

// Languages (LanguageOptions::OPTIONS)
var supportedLanguages = setOf2("hr", "da", "nl", "en", "fr", "de", "el", "it", "pl", "pt", "ro", "sl", "es", "tr")

func setOf2(ss ...string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}
