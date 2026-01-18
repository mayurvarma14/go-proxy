package config

import (
    "encoding/json"
    "fmt"
    "os"
    "path/filepath"

    yaml "gopkg.in/yaml.v3"
)

// Load reads a config file in JSON (default) or YAML (by extension) and unmarshals it.
// Supported YAML extensions: .yaml, .yml
func Load(path string) (ProxyConfig, error) {
    var out ProxyConfig
    b, err := os.ReadFile(path)
    if err != nil {
        return out, fmt.Errorf("read config: %w", err)
    }
    switch ext := filepath.Ext(path); ext {
    case ".yaml", ".yml":
        // Unmarshal YAML into a generic map then re-marshal to JSON so we can
        // leverage the existing JSON tags on structs.
        var ym any
        if err := yaml.Unmarshal(b, &ym); err != nil {
            return out, fmt.Errorf("parse YAML: %w", err)
        }
        jb, err := json.Marshal(ym)
        if err != nil {
            return out, fmt.Errorf("yaml->json: %w", err)
        }
        if err := json.Unmarshal(jb, &out); err != nil {
            return out, fmt.Errorf("decode config (YAML): %w", err)
        }
    default:
        if err := json.Unmarshal(b, &out); err != nil {
            return out, fmt.Errorf("parse JSON: %w", err)
        }
    }
    return out, nil
}
