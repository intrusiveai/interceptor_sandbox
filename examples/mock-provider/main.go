package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
)

const canary = "HNY-NORTHWIND-4471"

func main() {
	listen := flag.String("listen", "127.0.0.1:19090", "HTTP listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", chatCompletions)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	log.Printf("mock provider listening on %s", *listen)
	log.Fatal(http.ListenAndServe(*listen, mux))
}

func chatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read request", http.StatusBadRequest)
		return
	}
	content := "Handled safely"
	if bytes.Contains(body, []byte(canary)) {
		content = canary
	}
	w.Header().Set("Content-Type", "application/json")
	response := map[string]any{
		"id":     "mock-completion",
		"object": "chat.completion",
		"choices": []any{map[string]any{
			"index":   0,
			"message": map[string]any{"role": "assistant", "content": content},
		}},
		"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12},
	}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		fmt.Printf("encode response: %v\n", err)
	}
}
