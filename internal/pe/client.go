// Package pe is go-be's client for the pricing engine's bulk endpoint (/api/quote/services).
// Request/response shapes mirror the canonical artifacts in ai/go-be/ (captured from the real php
// path 2026-07-10) — change those files and this package together or not at all.
package pe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Client struct {
	baseURL string
	secret  string
	http    *http.Client
}

func NewClient(baseURL, secret string) *Client {
	return &Client{baseURL: baseURL, secret: secret, http: &http.Client{Timeout: 30 * time.Second}}
}

// Request is the canonical batch request DTO (see canonical_pe_batch_payload.json).
type Request struct {
	ID                          *string          `json:"id"`
	CombinedCourierTag          *string          `json:"combinedCourierTag"`
	Version                     string           `json:"version"`
	Parcels                     Parcels          `json:"parcels"`
	Client                      ClientInfo       `json:"client"`
	ExcludedCourierIDs          map[string][]int `json:"excludedCourierIds"`
	PickupDate                  string           `json:"pickupDate"`
	SelectedServiceType         int              `json:"selectedServiceType"`
	OptionalServiceTypes        []int            `json:"optionalServiceTypes"`
	Route                       Route            `json:"route"`
	CountriesOnHolidayForPickup []string         `json:"countriesOnHolidayForPickup"`
	HolidaysOnPickupCountry     []string         `json:"holidaysOnPickupCountry"`
	Tags                        []string         `json:"tags"`
	SpecificCourierIDs          []int            `json:"specificCourierIds"`
	ForcedCourierID             *int             `json:"forcedCourierId"`
}

// Parcels buckets parcel DTOs by type; element shapes differ per bucket (php *Dto classes), so
// they are built as maps by the quote package. allParcels = concatenation in php getter order.
type Parcels struct {
	AllParcels  []map[string]any `json:"allParcels"`
	Envelopes   []map[string]any `json:"envelopes"`
	Packages    []map[string]any `json:"packages"`
	Pallets     []map[string]any `json:"pallets"`
	Vans        []map[string]any `json:"vans"`
	Trucks      []map[string]any `json:"trucks"`
	NonStandard []map[string]any `json:"nonStandard"`
	Containers  []map[string]any `json:"containers"`
}

type ClientInfo struct {
	AccountType string  `json:"accountType"`
	AccountTier *string `json:"accountTier"`
	ID          *int    `json:"id"`
}

type Route struct {
	PickupAddress   Address `json:"pickupAddress"`
	DeliveryAddress Address `json:"deliveryAddress"`
	IsEU            bool    `json:"isEu"`
	Distance        float64 `json:"distance"`
	ChargeableStops int     `json:"chargeableStops"`
}

type Address struct {
	Zip             *string `json:"zip"`
	Zone            *string `json:"zone"`
	City            *string `json:"city"`
	Street          string  `json:"street"`
	RegionID        *int    `json:"regionId"`
	CountryID       int     `json:"countryId"`
	TimeZoneName    string  `json:"timeZoneName"`
	Country2IsoCode string  `json:"country2IsoCode"`
}

// Response mirrors canonical_pe_bulk_response.json.
type Response struct {
	Prices []Price `json:"prices"`
}

type Price struct {
	CourierID             int             `json:"courierId"`
	EstimatedDeliveryTime string          `json:"estimatedDeliveryTime"`
	PickupDate            string          `json:"pickupDate"`
	ServiceTypeID         int             `json:"serviceTypeId"`
	CourierPrice          CourierPrice    `json:"courierPrice"`
	ExtraCharges          []ExtraCharge   `json:"extraCharges"`
	Parcels               json.RawMessage `json:"parcels"`
}

// ExtraCharge mirrors ExtraChargePriceDto {name, courierPrice, sellingPrice}.
type ExtraCharge struct {
	Name         string  `json:"name"`
	CourierPrice float64 `json:"courierPrice"`
	SellingPrice float64 `json:"sellingPrice"`
}

type CourierPrice struct {
	CourierID   int        `json:"courierId"`
	CourierName string     `json:"courierName"`
	Price       PriceParts `json:"price"`
}

type PriceParts struct {
	TotalPrice float64 `json:"totalPrice"`
	TotalCost  float64 `json:"totalCost"`
	Margin     float64 `json:"margin"`
}

// NoPriceError is the PE's 422 "evaluated but nothing priced" outcome — the php batch path treats
// it as zero prices for the item, not a failure.
type NoPriceError struct{ Body string }

func (e *NoPriceError) Error() string { return "pe: no price: " + e.Body }

func (c *Client) QuoteServices(ctx context.Context, req *Request) (*Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/quote/services", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Application-Secret", c.secret)
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnprocessableEntity {
		return nil, &NoPriceError{Body: string(raw)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pe: status %d: %.200s", resp.StatusCode, raw)
	}
	var out Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("pe: decode: %w", err)
	}
	return &out, nil
}
