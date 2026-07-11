// corpus-gate replays a recorded corpus against a candidate /api/v2/quote implementation and
// semantically compares every response against the recorded oracle response. This is go-ba's
// Phase-0 gate: 0 divergences or it doesn't ship. Also usable as a stability probe by pointing
// -target back at the oracle itself.
//
// Usage:
//
//	go run ./cmd/corpus-gate -corpus testdata/corpus.json -target http://localhost:8091
//	go run ./cmd/corpus-gate -corpus testdata/corpus.json -target https://stagea.eurosender.dev  # self-check
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
	"sort"
	"strings"
	"time"

	"github.com/eurosender/go-ba/internal/corpus"
)

func main() {
	corpusPath := flag.String("corpus", "testdata/corpus.json", "recorded corpus file")
	target := flag.String("target", "", "base URL of the candidate implementation (required)")
	pace := flag.Duration("pace", 200*time.Millisecond, "sleep between requests")
	timeout := flag.Duration("timeout", 60*time.Second, "per-request timeout")
	tolerance := flag.Float64("tolerance", 1e-9, "float comparison tolerance")
	ignore := flag.String("ignore", "", "comma-separated path prefixes to ignore (indices as *)")
	maxDiffs := flag.Int("max-diffs", 10, "diffs printed per case")
	hostHeader := flag.String("host-header", "", "override the HTTP Host header (local devbox target behind a port-forward)")
	flag.Parse()
	postHost = *hostHeader
	if *target == "" {
		log.Fatal("-target is required")
	}

	raw, err := os.ReadFile(*corpusPath)
	if err != nil {
		log.Fatal(err)
	}
	var c corpus.Corpus
	if err := json.Unmarshal(raw, &c); err != nil {
		log.Fatal(err)
	}

	opts := corpus.Options{FloatTolerance: *tolerance}
	if *ignore != "" {
		opts.IgnorePaths = strings.Split(*ignore, ",")
	}

	client := &http.Client{Timeout: *timeout}
	pass, fail := 0, 0
	diffCounts := map[string]int{}
	for _, cs := range c.Cases {
		status, resp, err := post(client, *target+"/api/v2/quote", cs.Request)
		if err != nil {
			fail++
			fmt.Printf("ERROR %s: %v\n", cs.Name, err)
			continue
		}
		if status != cs.Status {
			fail++
			fmt.Printf("DIVERGED %s: status oracle=%d candidate=%d\n", cs.Name, cs.Status, status)
			continue
		}
		diffs, err := corpus.Compare(cs.Response, resp, opts)
		if err != nil {
			fail++
			fmt.Printf("ERROR %s: %v\n", cs.Name, err)
			continue
		}
		if len(diffs) == 0 {
			pass++
			time.Sleep(*pace)
			continue
		}
		fail++
		fmt.Printf("DIVERGED %s: %d diffs\n", cs.Name, len(diffs))
		for i, d := range diffs {
			if i >= *maxDiffs {
				fmt.Printf("  … %d more\n", len(diffs)-*maxDiffs)
				break
			}
			fmt.Printf("  %s: oracle=%s candidate=%s\n", d.Path, d.Oracle, d.Candidate)
		}
		for _, d := range diffs {
			diffCounts[d.Path]++
		}
		time.Sleep(*pace)
	}

	fmt.Printf("\n=== corpus gate: %d/%d pass", pass, pass+fail)
	if fail == 0 {
		fmt.Println(" — GATE CLEAN ===")
		return
	}
	fmt.Printf(", %d diverged ===\n", fail)
	fmt.Println("divergence hotspots (path: cases):")
	type kv struct {
		k string
		v int
	}
	var hs []kv
	for k, v := range diffCounts {
		hs = append(hs, kv{k, v})
	}
	sort.Slice(hs, func(i, j int) bool { return hs[i].v > hs[j].v })
	for i, h := range hs {
		if i >= 15 {
			break
		}
		fmt.Printf("  %s: %d\n", h.k, h.v)
	}
	os.Exit(1)
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
