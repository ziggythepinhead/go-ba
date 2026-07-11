// corpus-record drives a matrix of quote payloads through an oracle php backend
// (POST /api/v2/quote) and records (request, response) pairs into a corpus file. Sequential,
// gently paced — safe against a stage environment.
//
// Usage:
//
//	go run ./cmd/corpus-record -oracle https://stagea.eurosender.dev -out testdata/corpus.json
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/eurosender/go-ba/internal/corpus"
)

type route struct {
	name             string
	pickup, delivery int
	pickupZip        string
	deliveryZip      string
}

// Country ids per backend's CountryOptions (verified against live quotes this session).
var routes = []route{
	{"SI-IT", 191, 110, "1000", "20100"},
	{"IT-SI", 110, 191, "20100", "1000"},
	{"AT-BE", 16, 23, "1010", "1000"},
	{"DE-SI", 56, 191, "10115", "1000"},
	{"BE-DE", 23, 56, "1000", "10115"},
	{"IT-DE", 110, 56, "20100", "10115"},
	{"SI-AT", 191, 16, "1000", "1010"},
	{"DE-AT", 56, 16, "10115", "1010"},
}

type parcel struct {
	name    string
	weight  float64
	h, w, l int
}

var parcels = []parcel{
	{"small-2kg", 2, 15, 20, 25},
	{"mid-5kg", 5, 20, 20, 20},
	{"box-12kg", 12, 30, 40, 50},
	{"heavy-27kg", 27, 40, 50, 60},
	{"bulky-40kg", 40, 60, 80, 100},
}

func payload(r route, p parcel, accountType string, withZips bool) map[string]any {
	pickupZip, deliveryZip := any(nil), any(nil)
	if withZips {
		pickupZip, deliveryZip = r.pickupZip, r.deliveryZip
	}
	address := func(countryID int, zip any) map[string]any {
		return map[string]any{
			"zip": zip, "city": nil, "street": nil, "additionalInfo": nil, "region": nil,
			"countryId": countryID, "customFields": []any{}, "comment": nil, "pudoPointCode": nil,
		}
	}
	return map[string]any{
		"paymentMethod":         "credit_card",
		"selectedServiceTypeId": nil,
		"serviceSubtype":        "door_to_door",
		"accountType":           accountType,
		"additionalInsuranceId": nil,
		"couponCode":            nil,
		"currencyCode":          "EUR",
		"parcels": map[string]any{
			"packages": []any{map[string]any{
				"parcelId": "9f11f5e0-e832-438e-bbdd-059132cc4365", "quantity": 1,
				"weight": p.weight, "height": p.h, "width": p.w, "length": p.l, "value": nil,
			}},
		},
		"shipment": map[string]any{
			"pickupAddress":   address(r.pickup, pickupZip),
			"deliveryAddress": address(r.delivery, deliveryZip),
			"pickupDate":      nil,
			"addOns":          []any{},
			"value":           nil,
		},
		"unfinishedOrderUuid": nil,
	}
}

func main() {
	oracle := flag.String("oracle", "", "base URL of the php backend oracle (required)")
	out := flag.String("out", "testdata/corpus.json", "output corpus file")
	pace := flag.Duration("pace", 300*time.Millisecond, "sleep between requests")
	timeout := flag.Duration("timeout", 60*time.Second, "per-request timeout")
	shapes := flag.String("shapes", "", "prod shapes JSONL; when set, cases come from real traffic shapes instead of the synthetic matrix")
	maxCases := flag.Int("max-cases", 320, "case cap in -shapes mode")
	hostHeader := flag.String("host-header", "", "override the HTTP Host header (local devbox oracle behind a port-forward, e.g. be.docker.localhost)")
	currencies := flag.String("currencies", "", "comma-separated currency codes rotated per case (overrides the shapes' currencyCode; e.g. PLN,CZK,GBP,SEK)")
	flag.Parse()
	if *oracle == "" {
		log.Fatal("-oracle is required")
	}
	var currencyRotation []string
	if *currencies != "" {
		currencyRotation = strings.Split(*currencies, ",")
	}

	client := &http.Client{Timeout: *timeout}
	postHost = *hostHeader
	c := corpus.Corpus{RecordedAt: time.Now().UTC().Format(time.RFC3339), Oracle: *oracle}

	total, failed := 0, 0
	if *shapes != "" {
		shapeCases, err := buildShapeCases(*shapes, *maxCases)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("built %d cases from prod shapes", len(shapeCases))
		for i, sc := range shapeCases {
			if len(currencyRotation) > 0 {
				sc.payload["currencyCode"] = strings.TrimSpace(currencyRotation[i%len(currencyRotation)])
			}
			body, err := json.Marshal(sc.payload)
			if err != nil {
				log.Fatalf("%s: marshal: %v", sc.name, err)
			}
			status, resp, err := post(client, *oracle+"/api/v2/quote", body)
			total++
			if err != nil {
				failed++
				log.Printf("FAIL %s: %v", sc.name, err)
				continue
			}
			if !json.Valid(resp) {
				failed++
				log.Printf("FAIL %s: oracle returned non-JSON (status %d, %d bytes)", sc.name, status, len(resp))
				continue
			}
			c.Cases = append(c.Cases, corpus.Case{Name: sc.name, Request: body, Status: status, Response: resp})
			if total%25 == 0 {
				log.Printf("progress %d/%d", total, len(shapeCases))
			}
			time.Sleep(*pace)
		}
		writeCorpus(c, *out, total, failed)
		return
	}
	for _, r := range routes {
		for _, p := range parcels {
			for _, variant := range []struct {
				suffix      string
				accountType string
				withZips    bool
			}{
				{"guest", "person", false},
				{"guest-zip", "person", true},
				{"company-zip", "company", true},
			} {
				// keep the matrix affordable: company + no-zip variants only for one parcel size
				if variant.suffix != "guest-zip" && p.name != "box-12kg" {
					continue
				}
				name := fmt.Sprintf("%s/%s/%s", r.name, p.name, variant.suffix)
				body, err := json.Marshal(payload(r, p, variant.accountType, variant.withZips))
				if err != nil {
					log.Fatalf("%s: marshal: %v", name, err)
				}
				status, resp, err := post(client, *oracle+"/api/v2/quote", body)
				total++
				if err != nil {
					failed++
					log.Printf("FAIL %s: %v", name, err)
					continue
				}
				c.Cases = append(c.Cases, corpus.Case{Name: name, Request: body, Status: status, Response: resp})
				log.Printf("ok %s status=%d bytes=%d", name, status, len(resp))
				time.Sleep(*pace)
			}
		}
	}

	writeCorpus(c, *out, total, failed)
}

func writeCorpus(c corpus.Corpus, out string, total, failed int) {
	f, err := os.Create(out)
	if err != nil {
		log.Fatal(err)
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "\t")
	if err := enc.Encode(c); err != nil {
		log.Fatal(err)
	}
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("recorded %d/%d cases (failed=%d) -> %s\n", len(c.Cases), total, failed, out)
	if failed > 0 {
		os.Exit(1)
	}
}

// postHost, when non-empty, overrides the Host header (devbox nginx routes by server_name).
var postHost string

func post(client *http.Client, url string, body []byte) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	if postHost != "" {
		req.Host = postHost
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://www.eurosender.com")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, b, nil
}
