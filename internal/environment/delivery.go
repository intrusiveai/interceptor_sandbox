package environment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/intrusive-ai/interceptor-sandbox/internal/delivery"
	"github.com/intrusive-ai/interceptor-sandbox/internal/digest"
	"github.com/intrusive-ai/interceptor-sandbox/internal/services"
)

func resolveDelivery(root string, report *ValidationReport) error {
	blueprint := report.Blueprint
	totalBytes := 0
	for index := range blueprint.Application.Interface.Operations {
		operation := &blueprint.Application.Interface.Operations[index]
		operation.ResolvedDelivery = operation.Delivery
		if operation.DeliveryAsset != "" {
			data, err := readDeliveryAsset(root, operation.DeliveryAsset, report.Assets)
			if err != nil {
				return fmt.Errorf("operation %s delivery asset: %w", operation.ID, err)
			}
			if _, err := delivery.DecodeJSON(data); err != nil {
				return err
			}
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.UseNumber()
			decoder.DisallowUnknownFields()
			var contract delivery.Contract
			if err := decoder.Decode(&contract); err != nil {
				return fmt.Errorf("invalid delivery asset for %s", operation.ID)
			}
			operation.ResolvedDelivery = &contract
		}
		if err := delivery.Validate(operation.ResolvedDelivery); err != nil {
			return fmt.Errorf("operation %s delivery: %w", operation.ID, err)
		}
		if contract := operation.ResolvedDelivery; contract != nil {
			adapter := blueprint.Application.Interface.Adapter
			if adapter == "command" {
				requiresJSON := blueprint.Application.Interface.Command.Stdin == "json"
				for _, binding := range operation.Arguments {
					requiresJSON = requiresJSON || binding.From != ""
				}
				if requiresJSON && contract.Input.Encoding != "json" {
					return fmt.Errorf("operation %s requires JSON packaging for command input/bindings", operation.ID)
				}
			}
			if (adapter == "code" || adapter == "external") && contract.Input.Encoding != "json" {
				return fmt.Errorf("operation %s protocol input must explicitly describe its JSON value", operation.ID)
			}
			data, _ := json.Marshal(contract)
			totalBytes += len(data)
		} else if blueprint.APIVersion == DeliveryAPIVersion {
			report.Warnings = append(report.Warnings, fmt.Sprintf("operation %s has no reviewed input delivery contract; adaptive preparation gap", operation.ID))
		}
	}
	for index := range blueprint.Services {
		service := &blueprint.Services[index]
		if !service.ExportContracts {
			continue
		}
		switch service.Kind {
		case "mcp_http":
			seed := ""
			if service.Seed != "" {
				seed = filepath.Join(root, filepath.FromSlash(service.Seed))
			}
			handler, err := services.LoadMCP(filepath.Join(root, filepath.FromSlash(service.Tools)), seed, service.Endpoint, service.ID, nil, nil)
			if err != nil {
				return err
			}
			service.ResolvedEndpoints, err = handler.DeliveryEndpoints()
			if err != nil {
				return fmt.Errorf("service %s export contract: %w", service.ID, err)
			}
		case "stateful_http":
			handler, err := services.LoadStateful(filepath.Join(root, filepath.FromSlash(service.Seed)), filepath.Join(root, filepath.FromSlash(service.Routes)), service.ID, nil, nil)
			if err != nil {
				return err
			}
			service.ResolvedEndpoints = handler.DeliveryEndpoints()
		case "fixture_http":
			set, err := services.LoadFixtureSet(filepath.Join(root, filepath.FromSlash(service.Fixtures)))
			if err != nil {
				return err
			}
			service.ResolvedEndpoints = set.DeliveryEndpoints()
		default:
			return fmt.Errorf("service %s cannot export delivery contracts", service.ID)
		}
		if len(service.ResolvedEndpoints) > 128 {
			return errors.New("service exceeds 128 delivery endpoints")
		}
		data, err := json.Marshal(service.ResolvedEndpoints)
		if err != nil {
			return err
		}
		totalBytes += len(data)
	}
	if totalBytes > 1<<20 {
		return errors.New("resolved delivery inventory exceeds 1 MiB")
	}
	return nil
}

// A contract asset is a sealed, bounded regular file, never a remote schema.
// For this input class, symlinks are prohibited even when other seed assets
// legitimately use in-tree links.
func readDeliveryAsset(root, relative string, assets []Asset) ([]byte, error) {
	if relative == "" || filepath.IsAbs(relative) || filepath.ToSlash(filepath.Clean(relative)) != relative || strings.HasPrefix(relative, "../") {
		return nil, errors.New("delivery asset must be a clean Blueprint-relative path")
	}
	current := root
	for _, part := range strings.Split(relative, "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("delivery asset path is missing or linked")
		}
	}
	confined, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer confined.Close()
	file, err := confined.Open(relative)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > delivery.MaxContractBytes {
		return nil, errors.New("delivery asset is not a bounded regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, delivery.MaxContractBytes+1))
	if err != nil || len(data) > delivery.MaxContractBytes {
		return nil, errors.New("delivery asset exceeds limit")
	}
	for _, asset := range assets {
		if asset.Path == relative && asset.Type == "file" && asset.Size == int64(len(data)) && asset.Digest == digest.Bytes(data) {
			return data, nil
		}
	}
	return nil, errors.New("delivery asset differs from validated inventory")
}
