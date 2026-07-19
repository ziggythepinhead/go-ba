// Non-EUR currency conversion. Ports AbstractPriceCalculator::addConvertedValues +
// CurrencyConverter/StoreFactory::convert_to_currency: converted = round2(amount × officialRate ×
// exchange_rate_percentage); original prices stay EUR, the requested currency appears only in the
// converted blocks and options.exchangeRate. Specs: 03 §8.8, agent findings 2026-07-11.
package quote

import "github.com/eurosender/go-be/internal/refdata"

var currencyCodeByID = map[int]string{1: "EUR", 2: "HRK", 3: "CZK", 4: "DKK", 5: "GBP", 6: "PLN", 7: "SEK", 8: "RON", 12: "USD"}

// modifiedExchangeRate = latest official rate × ns_catalog_currencies.exchange_rate_percentage
// (a multiplier like 1.00/1.02). Returns 0 when the code has no rate row.
func modifiedExchangeRate(snap *refdata.Snapshot, currencyID int) float64 {
	code := currencyCodeByID[currencyID]
	return snap.ExchangeRateByCode[code] * snap.ExchangePercentageByCode[code]
}

// convertAmount = round2(amount × modifiedRate); identity for EUR.
func convertAmount(snap *refdata.Snapshot, amount float64, currencyID int) float64 {
	if currencyID == 1 {
		return amount
	}
	return round2(amount * modifiedExchangeRate(snap, currencyID))
}

// addConvertedValues ports AbstractPriceCalculator::addConvertedValues onto a transformed price:
// no-op for EUR; else fills the converted mirror fields. Preserves the php bug where the
// flexible-booking GROSS is overwritten with its converted value (spec 03 §8.8 gotcha 7).
func addConvertedValues(snap *refdata.Snapshot, r *peResp, currencyID int) {
	if currencyID == 1 {
		r.hasConverted = false
		return
	}
	rate := modifiedExchangeRate(snap, currencyID)
	conv := func(v float64) float64 { return round2(v * rate) }
	r.hasConverted = true
	r.exchangeRate = rate
	r.convertedSymbol = currencyCodeByID[currencyID]
	r.convTotalNet = conv(r.totalNet)
	r.convTotalGross = conv(r.totalGross)
	r.convItemsNet = conv(r.itemsNet)
	r.convItemsGross = conv(r.itemsGross)
	// coupon/insurance converted values: guests carry 0 → conv(0)=0, fields unused downstream yet
	r.convFlexNet = conv(r.flexNet)
	r.flexGross = conv(r.flexGross) // php bug ported verbatim: gross overwritten, converted-gross never set
}
