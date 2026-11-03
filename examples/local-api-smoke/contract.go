package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
)

// This independent consumer pin models Operator without depending on its code.
const supportedContractVersion = "0.1.0"

func checkContract(httpClient *http.Client, baseURL, expected string) error {
	r, err := httpClient.Post(baseURL+"/v1/contract-version", "application/json", bytes.NewBufferString(`{}`))
	if err != nil {
		return fmt.Errorf("Interceptor compatibility check failed: %w", err)
	}
	defer r.Body.Close()
	var result struct {
		Status int `json:"status"`
		Body   struct {
			Version string `json:"interceptor_contract_version"`
		} `json:"body"`
	}
	if r.StatusCode != 200 {
		return fmt.Errorf("Interceptor compatibility endpoint returned HTTP %d; use an Interceptor release supporting the contract API", r.StatusCode)
	}
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&result); err != nil {
		return fmt.Errorf("unreadable Interceptor contract version: %w", err)
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return fmt.Errorf("unreadable Interceptor contract version: trailing response data")
	}
	actual := result.Body.Version
	if result.Status != 200 || len(actual) > 128 || !regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`).MatchString(actual) {
		return fmt.Errorf("missing or malformed Interceptor contract version; install a compatible release")
	}
	if actual != expected {
		return fmt.Errorf("Interceptor contract mismatch: expected %s, actual %s; use matching integration and Interceptor contracts", expected, actual)
	}
	return nil
}
