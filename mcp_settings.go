package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"

	"gopkg.in/yaml.v3"
	"tproxy-gateway/internal/mcpauth"
)

type mcpRuntimePolicy struct {
	Config   MCPConfig
	UISecret string
}

var liveMCPPolicy atomic.Pointer[mcpRuntimePolicy]

func currentMCPPolicy() mcpRuntimePolicy {
	if current := liveMCPPolicy.Load(); current != nil {
		return *current
	}
	return mcpRuntimePolicy{}
}

func publishMCPPolicy(configuration *Config) {
	if configuration == nil {
		return
	}
	configMu.Lock()
	defer configMu.Unlock()
	policy := mcpRuntimePolicy{Config: configuration.MCP, UISecret: configuration.UI.Secret}
	policy.Config.AllowedOrigins = append([]string(nil), policy.Config.AllowedOrigins...)
	// A full reload may have parsed the file before an authentication-only save.
	// Read the latest auth fields before publishing to prevent stale credentials.
	if document, err := readMCPSettingsDocument(ActiveConfigPath); err == nil {
		if mcp := mappingValue(document.Content[0], "mcp"); mcp != nil {
			if auth := mappingValue(mcp, "auth"); auth != nil {
				var latest mcpauth.Config
				if auth.Decode(&latest) == nil && mcpauth.Validate(latest) == nil {
					policy.Config.Auth = latest
				}
			}
		}
	}
	liveMCPPolicy.Store(&policy)
}

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return mapping.Content[index+1]
		}
	}
	return nil
}

func replaceMappingValue(mapping *yaml.Node, key string, value *yaml.Node) {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			mapping.Content[index+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

func readMCPSettingsDocument(path string) (*yaml.Node, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (4<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 4<<20 {
		return nil, errors.New("configuration file exceeds settings size limit")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("configuration must contain one YAML document")
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("configuration must be a YAML mapping")
	}
	var structuralCheck Config
	if err := document.Decode(&structuralCheck); err != nil {
		return nil, err
	}
	return &document, nil
}

func saveMCPAuthentication(ctx context.Context, path string, update mcpauth.Update) (mcpauth.Config, error) {
	configMu.Lock()
	defer configMu.Unlock()
	if err := ctx.Err(); err != nil {
		return mcpauth.Config{}, err
	}
	document, err := readMCPSettingsDocument(path)
	if err != nil {
		return mcpauth.Config{}, err
	}
	root := document.Content[0]
	mcp := mappingValue(root, "mcp")
	if mcp == nil {
		mcp = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		replaceMappingValue(root, "mcp", mcp)
	}
	if mcp.Kind != yaml.MappingNode {
		return mcpauth.Config{}, errors.New("MCP configuration must be a mapping")
	}
	policy := currentMCPPolicy()
	candidate, err := update.Apply(policy.Config.Auth)
	if err != nil {
		return mcpauth.Config{}, err
	}
	var authNode yaml.Node
	if err := authNode.Encode(candidate); err != nil {
		return mcpauth.Config{}, err
	}
	replaceMappingValue(mcp, "auth", &authNode)
	var encoded bytes.Buffer
	encoder := yaml.NewEncoder(&encoded)
	encoder.SetIndent(2)
	if err := encoder.Encode(document); err != nil {
		return mcpauth.Config{}, err
	}
	if err := encoder.Close(); err != nil {
		return mcpauth.Config{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return mcpauth.Config{}, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".mcp-auth-*.tmp")
	if err != nil {
		return mcpauth.Config{}, err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(info.Mode().Perm()); err != nil {
		temporary.Close()
		return mcpauth.Config{}, err
	}
	if _, err := temporary.Write(encoded.Bytes()); err != nil {
		temporary.Close()
		return mcpauth.Config{}, err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return mcpauth.Config{}, err
	}
	if err := temporary.Close(); err != nil {
		return mcpauth.Config{}, err
	}
	if err := ctx.Err(); err != nil {
		return mcpauth.Config{}, err
	}
	if err := os.Rename(name, path); err != nil {
		return mcpauth.Config{}, err
	}
	policy.Config.Auth = candidate
	liveMCPPolicy.Store(&policy)
	return candidate, nil
}
