package bridge

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// nativeBedrockConverse decodes the public Bedrock Converse HTTP shape into
// the AWS SDK shape. The model identifier comes from /model/{modelId}/converse,
// exactly as it does at an AWS Bedrock Runtime endpoint.
func nativeBedrockConverse(model string, body []byte) (*bedrockruntime.ConverseInput, []byte, error) {
	request, err := decodeJSONObject(body)
	if err != nil {
		return nil, nil, err
	}
	if model == "" {
		return nil, nil, errors.New("Bedrock Converse path must contain a model identifier")
	}
	if err := rejectUnsupportedKeys(request, []string{
		"messages", "system", "inferenceConfig", "toolConfig", "additionalModelRequestFields",
		"additionalModelResponseFieldPaths", "requestMetadata",
	}, "Bedrock Converse parameter"); err != nil {
		return nil, nil, err
	}
	input := &bedrockruntime.ConverseInput{ModelId: aws.String(model)}
	if raw, exists := request["messages"]; exists {
		input.Messages, err = nativeBedrockMessages(raw)
		if err != nil {
			return nil, nil, err
		}
	}
	if raw, exists := request["system"]; exists {
		input.System, err = nativeBedrockSystem(raw)
		if err != nil {
			return nil, nil, err
		}
	}
	if raw, exists := request["inferenceConfig"]; exists {
		input.InferenceConfig, err = nativeBedrockInference(raw)
		if err != nil {
			return nil, nil, err
		}
	}
	if raw, exists := request["toolConfig"]; exists {
		input.ToolConfig, err = nativeBedrockTools(raw)
		if err != nil {
			return nil, nil, err
		}
	}
	if raw, exists := request["additionalModelRequestFields"]; exists {
		input.AdditionalModelRequestFields = document.NewLazyDocument(raw)
	}
	if raw, exists := request["additionalModelResponseFieldPaths"]; exists {
		input.AdditionalModelResponseFieldPaths, err = stringListValue(raw)
		if err != nil {
			return nil, nil, errors.New("additionalModelResponseFieldPaths must be an array of strings")
		}
	}
	if raw, exists := request["requestMetadata"]; exists {
		input.RequestMetadata, err = stringMapValue(raw)
		if err != nil {
			return nil, nil, errors.New("requestMetadata must be an object with string values")
		}
	}
	evidenceRequest := make(map[string]any, len(request)+1)
	for key, value := range request {
		evidenceRequest[key] = value
	}
	evidenceRequest["modelId"] = model
	evidence, err := json.Marshal(evidenceRequest)
	return input, evidence, err
}

func nativeBedrockMessages(raw any) ([]types.Message, error) {
	items, ok := raw.([]any)
	if !ok {
		return nil, errors.New("messages must be an array")
	}
	result := make([]types.Message, 0, len(items))
	for index, item := range items {
		message, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("message %d must be an object", index)
		}
		if err := rejectUnsupportedKeys(message, []string{"role", "content"}, fmt.Sprintf("message %d field", index)); err != nil {
			return nil, err
		}
		role, _ := message["role"].(string)
		if role != "user" && role != "assistant" {
			return nil, fmt.Errorf("message %d has unsupported role %q", index, role)
		}
		content, err := nativeBedrockContent(message["content"])
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", index, err)
		}
		result = append(result, types.Message{Role: types.ConversationRole(role), Content: content})
	}
	return result, nil
}

func nativeBedrockContent(raw any) ([]types.ContentBlock, error) {
	items, ok := raw.([]any)
	if !ok || len(items) == 0 {
		return nil, errors.New("content must be a non-empty array")
	}
	result := make([]types.ContentBlock, 0, len(items))
	for _, item := range items {
		block, ok := item.(map[string]any)
		if !ok || len(block) != 1 {
			return nil, errors.New("content block must be an object with one supported member")
		}
		if text, exists := block["text"]; exists {
			value, ok := text.(string)
			if !ok {
				return nil, errors.New("text content must be a string")
			}
			result = append(result, &types.ContentBlockMemberText{Value: value})
			continue
		}
		if rawUse, exists := block["toolUse"]; exists {
			value, ok := rawUse.(map[string]any)
			if !ok {
				return nil, errors.New("toolUse must be an object")
			}
			if err := rejectUnsupportedKeys(value, []string{"toolUseId", "name", "input"}, "toolUse field"); err != nil {
				return nil, err
			}
			id, _ := value["toolUseId"].(string)
			name, _ := value["name"].(string)
			input, exists := value["input"]
			if id == "" || name == "" || !exists {
				return nil, errors.New("toolUse requires toolUseId, name, and input")
			}
			result = append(result, &types.ContentBlockMemberToolUse{Value: types.ToolUseBlock{
				ToolUseId: aws.String(id), Name: aws.String(name), Input: document.NewLazyDocument(input),
			}})
			continue
		}
		if rawResult, exists := block["toolResult"]; exists {
			value, ok := rawResult.(map[string]any)
			if !ok {
				return nil, errors.New("toolResult must be an object")
			}
			converted, err := nativeBedrockToolResult(value)
			if err != nil {
				return nil, err
			}
			result = append(result, converted)
			continue
		}
		return nil, errors.New("supported content members are text, toolUse, and toolResult")
	}
	return result, nil
}

func nativeBedrockToolResult(value map[string]any) (types.ContentBlock, error) {
	if err := rejectUnsupportedKeys(value, []string{"toolUseId", "content", "status"}, "toolResult field"); err != nil {
		return nil, err
	}
	id, _ := value["toolUseId"].(string)
	items, ok := value["content"].([]any)
	if id == "" || !ok || len(items) == 0 {
		return nil, errors.New("toolResult requires toolUseId and non-empty content")
	}
	result := types.ToolResultBlock{ToolUseId: aws.String(id)}
	if status, exists := value["status"].(string); exists {
		if status != "success" && status != "error" {
			return nil, errors.New("toolResult status must be success or error")
		}
		result.Status = types.ToolResultStatus(status)
	}
	for _, item := range items {
		block, ok := item.(map[string]any)
		if !ok || len(block) != 1 {
			return nil, errors.New("toolResult content block must contain one member")
		}
		if text, exists := block["text"]; exists {
			value, ok := text.(string)
			if !ok {
				return nil, errors.New("toolResult text must be a string")
			}
			result.Content = append(result.Content, &types.ToolResultContentBlockMemberText{Value: value})
		} else if value, exists := block["json"]; exists {
			result.Content = append(result.Content, &types.ToolResultContentBlockMemberJson{Value: document.NewLazyDocument(value)})
		} else {
			return nil, errors.New("supported toolResult content members are text and json")
		}
	}
	return &types.ContentBlockMemberToolResult{Value: result}, nil
}

func nativeBedrockSystem(raw any) ([]types.SystemContentBlock, error) {
	items, ok := raw.([]any)
	if !ok {
		return nil, errors.New("system must be an array")
	}
	result := make([]types.SystemContentBlock, 0, len(items))
	for _, item := range items {
		block, ok := item.(map[string]any)
		text, textOK := block["text"].(string)
		if !ok || !textOK || len(block) != 1 {
			return nil, errors.New("system currently supports text blocks only")
		}
		result = append(result, &types.SystemContentBlockMemberText{Value: text})
	}
	return result, nil
}

func nativeBedrockInference(raw any) (*types.InferenceConfiguration, error) {
	value, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("inferenceConfig must be an object")
	}
	if err := rejectUnsupportedKeys(value, []string{"maxTokens", "temperature", "topP", "stopSequences"}, "inferenceConfig field"); err != nil {
		return nil, err
	}
	result := &types.InferenceConfiguration{}
	if raw, exists := value["maxTokens"]; exists {
		number, err := int32Value(raw)
		if err != nil || number <= 0 {
			return nil, errors.New("inferenceConfig.maxTokens must be a positive 32-bit integer")
		}
		result.MaxTokens = aws.Int32(number)
	}
	for name, target := range map[string]**float32{"temperature": &result.Temperature, "topP": &result.TopP} {
		if raw, exists := value[name]; exists {
			number, err := float32Value(raw)
			if err != nil {
				return nil, fmt.Errorf("inferenceConfig.%s must be a number", name)
			}
			*target = aws.Float32(number)
		}
	}
	if raw, exists := value["stopSequences"]; exists {
		var err error
		result.StopSequences, err = stringListValue(raw)
		if err != nil {
			return nil, errors.New("inferenceConfig.stopSequences must be an array of strings")
		}
	}
	return result, nil
}

func nativeBedrockTools(raw any) (*types.ToolConfiguration, error) {
	value, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("toolConfig must be an object")
	}
	if err := rejectUnsupportedKeys(value, []string{"tools", "toolChoice"}, "toolConfig field"); err != nil {
		return nil, err
	}
	items, ok := value["tools"].([]any)
	if !ok || len(items) == 0 {
		return nil, errors.New("toolConfig.tools must be a non-empty array")
	}
	result := &types.ToolConfiguration{}
	for _, item := range items {
		wrapper, ok := item.(map[string]any)
		spec, okSpec := wrapper["toolSpec"].(map[string]any)
		if !ok || !okSpec || len(wrapper) != 1 {
			return nil, errors.New("toolConfig currently supports toolSpec entries only")
		}
		if err := rejectUnsupportedKeys(spec, []string{"name", "description", "inputSchema", "strict"}, "toolSpec field"); err != nil {
			return nil, err
		}
		name, _ := spec["name"].(string)
		inputSchema, _ := spec["inputSchema"].(map[string]any)
		schema, exists := inputSchema["json"]
		if name == "" || !exists || len(inputSchema) != 1 {
			return nil, errors.New("toolSpec requires name and inputSchema.json")
		}
		converted := types.ToolSpecification{Name: aws.String(name), InputSchema: &types.ToolInputSchemaMemberJson{Value: document.NewLazyDocument(schema)}}
		if description, ok := spec["description"].(string); ok {
			converted.Description = aws.String(description)
		}
		if strict, ok := spec["strict"].(bool); ok {
			converted.Strict = aws.Bool(strict)
		}
		result.Tools = append(result.Tools, &types.ToolMemberToolSpec{Value: converted})
	}
	if rawChoice, exists := value["toolChoice"]; exists {
		choice, ok := rawChoice.(map[string]any)
		if !ok || len(choice) != 1 {
			return nil, errors.New("toolChoice must contain auto, any, or tool")
		}
		switch {
		case choice["auto"] != nil:
			result.ToolChoice = &types.ToolChoiceMemberAuto{Value: types.AutoToolChoice{}}
		case choice["any"] != nil:
			result.ToolChoice = &types.ToolChoiceMemberAny{Value: types.AnyToolChoice{}}
		case choice["tool"] != nil:
			selected, ok := choice["tool"].(map[string]any)
			name, _ := selected["name"].(string)
			if !ok || name == "" {
				return nil, errors.New("toolChoice.tool requires a name")
			}
			result.ToolChoice = &types.ToolChoiceMemberTool{Value: types.SpecificToolChoice{Name: aws.String(name)}}
		default:
			return nil, errors.New("toolChoice must contain auto, any, or tool")
		}
	}
	return result, nil
}

func stringMapValue(raw any) (map[string]string, error) {
	object, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("not a string map")
	}
	result := make(map[string]string, len(object))
	for key, rawValue := range object {
		value, ok := rawValue.(string)
		if !ok {
			return nil, errors.New("not a string map")
		}
		result[key] = value
	}
	return result, nil
}
