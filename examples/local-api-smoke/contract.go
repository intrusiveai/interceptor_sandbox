package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
)

// These independent consumer pins model Operator without depending on its code.
// Update deliberately after reviewing the contract; never derive them at runtime.
const supportedContractVersion = "0.4.0"
const supportedContractSHA256 = "ca89d9b4e00c0bf94fd9056a2513953de5c60ef8cbc04e322b5eed36ac730888"

func checkContract(httpClient *http.Client, baseURL, expected, expectedHash string) error {
	r, err := httpClient.Post(baseURL+"/v1/contract-version", "application/json", bytes.NewBufferString(`{}`))
	if err != nil {
		return fmt.Errorf("Interceptor compatibility check failed: %w", err)
	}
	defer r.Body.Close()
	var result struct {
		Status int `json:"status"`
		Body   struct {
			Version string `json:"interceptor_contract_version"`
			SHA256  string `json:"interceptor_contract_sha256"`
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
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(result.Body.SHA256) {
		return fmt.Errorf("missing or malformed Interceptor contract SHA-256; install a compatible release")
	}
	if actual != expected || result.Body.SHA256 != expectedHash {
		return fmt.Errorf("Interceptor contract mismatch: expected %s SHA-256 %s, actual %s SHA-256 %s; use matching integration and Interceptor contracts", expected, expectedHash, actual, result.Body.SHA256)
	}
	return nil
}
