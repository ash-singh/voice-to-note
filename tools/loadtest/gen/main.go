// Command gen drives POST /v1/notes at a given concurrency and request count,
// polls each job to completion, and prints latency/throughput stats. Intended
// to run against a server wired to tools/loadtest/mockapi, so results reflect
// the app's own overhead rather than a real LLM or sink.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"mime/multipart"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type result struct {
	submitMs   float64
	submitCode int
	submitErr  error
	totalMs    float64 // submit to job done/failed, 0 if it never finished
	finalState string  // done | failed | timeout
}

func postNote(client *http.Client, addr string, payload []byte) (jobID string, code int, err error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("audio", fmt.Sprintf("loadtest-%d.m4a", rand.Int63()))
	if err != nil {
		return "", 0, err
	}
	fw.Write(payload)
	w.Close()

	req, err := http.NewRequest(http.MethodPost, addr+"/v1/notes", &buf)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()

	var out struct {
		Data struct {
			JobID string `json:"job_id"`
		} `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	return out.Data.JobID, resp.StatusCode, nil
}

func pollUntilDone(client *http.Client, addr, jobID string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(addr + "/v1/notes/" + jobID)
		if err == nil {
			var out struct {
				Data struct {
					State string `json:"state"`
				} `json:"data"`
			}
			json.NewDecoder(resp.Body).Decode(&out)
			resp.Body.Close()
			if out.Data.State == "done" || out.Data.State == "failed" {
				return out.Data.State
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	return "timeout"
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p / 100 * float64(len(sorted)-1))
	return sorted[idx]
}

func main() {
	addr := flag.String("addr", "http://127.0.0.1:8080", "server address")
	n := flag.Int("n", 100, "total requests")
	c := flag.Int("c", 10, "concurrency")
	size := flag.Int("size", 2048, "fake audio payload size in bytes")
	pollTimeout := flag.Duration("poll-timeout", 30*time.Second, "max time to wait for a job to finish")
	submitTimeout := flag.Duration("submit-timeout", 15*time.Second, "max time to wait for the POST /v1/notes response")
	label := flag.String("label", "", "label for this run, printed in the summary line")
	flag.Parse()

	client := &http.Client{
		Timeout: *submitTimeout,
		Transport: &http.Transport{
			MaxIdleConns:        *c * 2,
			MaxIdleConnsPerHost: *c * 2,
			IdleConnTimeout:     90 * time.Second,
		},
	}
	results := make([]result, *n)
	var next int64

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < *c; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				idx := int(atomic.AddInt64(&next, 1) - 1)
				if idx >= *n {
					return
				}
				payload := make([]byte, *size)
				rand.Read(payload)

				t0 := time.Now()
				jobID, code, err := postNote(client, *addr, payload)
				submitMs := time.Since(t0).Seconds() * 1000

				r := result{submitMs: submitMs, submitCode: code, submitErr: err}
				if err == nil && code == http.StatusAccepted && jobID != "" {
					state := pollUntilDone(client, *addr, jobID, *pollTimeout)
					r.finalState = state
					r.totalMs = time.Since(t0).Seconds() * 1000
				}
				results[idx] = r
			}
		}()
	}
	wg.Wait()
	wallSec := time.Since(start).Seconds()

	var submitLat, totalLat []float64
	var accepted, done, failed, timeout, errs, non202 int
	statusCounts := map[int]int{}
	var sampleErr error
	for _, r := range results {
		if r.submitErr != nil {
			errs++
			if sampleErr == nil {
				sampleErr = r.submitErr
			}
			continue
		}
		submitLat = append(submitLat, r.submitMs)
		statusCounts[r.submitCode]++
		if r.submitCode != http.StatusAccepted {
			non202++
			continue
		}
		accepted++
		switch r.finalState {
		case "done":
			done++
			totalLat = append(totalLat, r.totalMs)
		case "failed":
			failed++
		default:
			timeout++
		}
	}
	sort.Float64s(submitLat)
	sort.Float64s(totalLat)

	fmt.Fprintf(os.Stdout, "\n=== load test: %s ===\n", *label)
	fmt.Fprintf(os.Stdout, "requests=%d concurrency=%d wall=%.2fs throughput=%.1f req/s\n", *n, *c, wallSec, float64(*n)/wallSec)
	fmt.Fprintf(os.Stdout, "submit: accepted=%d non-202=%d network-err=%d status-counts=%v\n", accepted, non202, errs, statusCounts)
	if sampleErr != nil {
		fmt.Fprintf(os.Stdout, "sample network error: %v\n", sampleErr)
	}
	fmt.Fprintf(os.Stdout, "submit latency ms:  p50=%.1f p95=%.1f p99=%.1f max=%.1f\n",
		percentile(submitLat, 50), percentile(submitLat, 95), percentile(submitLat, 99), maxOf(submitLat))
	fmt.Fprintf(os.Stdout, "pipeline: done=%d failed=%d timeout=%d\n", done, failed, timeout)
	fmt.Fprintf(os.Stdout, "end-to-end latency ms (submit->done): p50=%.1f p95=%.1f p99=%.1f max=%.1f\n",
		percentile(totalLat, 50), percentile(totalLat, 95), percentile(totalLat, 99), maxOf(totalLat))
}

func maxOf(sorted []float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[len(sorted)-1]
}
