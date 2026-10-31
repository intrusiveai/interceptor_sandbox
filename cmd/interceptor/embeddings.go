package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/intrusive-ai/interceptor-sandbox/internal/bridge"
)

type embeddingsOptions struct {
	provider, baseURL, auth, credentialConfig, credentialID, keyEnv string
	models                                                          stringList
	awsRegion, awsProfile                                           string
}

func (o embeddingsOptions) resolve(ctx context.Context, production bool, stored func(context.Context, string, string) (string, error)) (string, error) {
	if o.provider == "" {
		if o.baseURL != "" || o.auth != "" || o.credentialConfig != "" || o.credentialID != "" || o.keyEnv != "" || o.awsRegion != "" || o.awsProfile != "" || len(o.models) != 0 {
			return "", errors.New("embeddings options require --embeddings-provider")
		}
		return "", nil
	}
	if o.provider == bridge.ProviderBedrockTitan {
		if o.baseURL != "" || o.keyEnv != "" || o.credentialConfig != "" || o.credentialID != "" {
			return "", errors.New("bedrock-titan uses host AWS identity and accepts no embeddings HTTP endpoint or key options")
		}
		if len(o.models) == 0 {
			return "", errors.New("Titan embeddings require --allow-embedding-model " + bridge.TitanEmbeddingModel)
		}
		for _, model := range o.models {
			if model != bridge.TitanEmbeddingModel {
				return "", errors.New("unsupported Titan embedding model")
			}
		}
		if o.auth != "workload-identity" && o.auth != "aws-profile" && o.auth != "aws-environment" {
			return "", errors.New("Titan --embeddings-auth must select workload-identity, aws-profile, or aws-environment")
		}
		if (o.auth == "aws-profile") != (o.awsProfile != "") {
			return "", errors.New("--embeddings-aws-profile is required only for aws-profile authentication")
		}
		return "", nil
	}
	if o.awsRegion != "" || o.awsProfile != "" {
		return "", errors.New("embeddings AWS options require bedrock-titan")
	}
	if o.provider != bridge.ProviderOpenAICompatible {
		return "", errors.New("--embeddings-provider must be openai-compatible or bedrock-titan")
	}
	if _, err := bridge.NewOpenAIEmbeddingsProvider(bridge.HTTPProviderConfig{BaseURL: o.baseURL}); err != nil {
		return "", fmt.Errorf("--embeddings-base-url: %w", err)
	}
	if len(o.models) == 0 {
		return "", errors.New("embeddings require --allow-embedding-model")
	}
	for _, model := range o.models {
		if strings.TrimSpace(model) == "" {
			return "", errors.New("--allow-embedding-model must be nonempty")
		}
	}
	if o.auth == "" && o.credentialID != "" {
		o.auth = "secret-store"
	}
	switch o.auth {
	case "secret-store":
		if o.credentialConfig == "" || o.credentialID == "" || o.keyEnv != "" {
			return "", errors.New("embeddings secret-store requires --embeddings-credential-config and --embeddings-credential-id, without --embeddings-api-key-env")
		}
		return stored(ctx, o.credentialConfig, o.credentialID)
	case "api-key-env":
		if o.keyEnv == "" || o.credentialConfig != "" || o.credentialID != "" {
			return "", errors.New("embeddings api-key-env requires only --embeddings-api-key-env")
		}
		key, err := resolveProviderOptions(ctx, bridge.ProviderOpenAIChat, o.baseURL, o.keyEnv, "", "", "api-key-env", production)
		if err != nil {
			return "", fmt.Errorf("embeddings credential: %w", err)
		}
		return key, nil
	case "none":
		if o.credentialConfig != "" || o.credentialID != "" || o.keyEnv != "" {
			return "", errors.New("embeddings none authentication conflicts with credential options")
		}
		return "", nil
	default:
		return "", errors.New("--embeddings-auth must explicitly select secret-store, api-key-env, or none")
	}
}
