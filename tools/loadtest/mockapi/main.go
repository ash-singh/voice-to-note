// Command mockapi fakes the OpenAI-compatible LLM endpoints and a webhook
// sink so the server's own HTTP/queue/worker path can be load tested without
// hitting a real LLM or a real sink.
package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"time"
)

func main() {
	addr := flag.String("addr", ":9090", "listen address")
	transcribeLatency := flag.Duration("transcribe-latency", 50*time.Millisecond, "simulated speech-to-text latency")
	chatLatency := flag.Duration("chat-latency", 100*time.Millisecond, "simulated chat-completion latency")
	webhookLatency := flag.Duration("webhook-latency", 20*time.Millisecond, "simulated sink latency")
	flag.Parse()

	mux := http.NewServeMux()

	mux.HandleFunc("/v1/audio/transcriptions", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(*transcribeLatency)
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"text": "call Anna about the invoice"})
	})

	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(*chatLatency)
		io.Copy(io.Discard, r.Body)
		note := map[string]any{
			"title":        "Call Anna about Invoice",
			"summary":      "Discuss the unpaid invoice with Anna.",
			"action_items": []string{"Call Anna about the unpaid invoice"},
			"transcript":   "call Anna about the invoice",
		}
		content, _ := json.Marshal(note)
		resp := map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": string(content)}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/webhook", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(*webhookLatency)
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"id": "mock-ref"})
	})

	log.Printf("mockapi listening on %s (transcribe=%s chat=%s webhook=%s)", *addr, *transcribeLatency, *chatLatency, *webhookLatency)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
