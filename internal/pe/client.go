// Package pe is go-ba's client for the pricing engine's bulk endpoint (/api/quote/services).
// Request/response shapes mirror the canonical artifacts in ai/go-ba/ (captured from the real php
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

type Parcels struct {
	AllParcels  []Parcel `json:"allParcels"`
	Envelopes   []Parcel `json:"envelopes"`
	Packages    []Parcel `json:"packages"`
	Pallets     []Parcel `json:"pallets"`
	Vans        []Parcel `json:"vans"`
	Trucks      []Parcel `json:"trucks"`
	NonStandard []Parcel `json:"nonStandard"`
	Containers  []Parcel `json:"containers"`
}

type Parcel struct {
	Type             string   `json:"type"`
	OptionalServices *string  `json:"optionalServices"`
	Weight           float64  `json:"weight"`
	Length           float64  `json:"length"`
	Width            float64  `json:"width"`
	Height           float64  `json:"height"`
	GroupID          string   `json:"groupId"`
	Stackable        bool     `json:"stackable"`
	Quantity         int      `json:"quantity"`
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
	ExtraCharges          json.RawMessage `json:"extraCharges"`
	Parcels               json.RawMessage `json:"parcels"`
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
