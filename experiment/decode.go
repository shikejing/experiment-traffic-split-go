package experiment

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// ParseConfig decodes a JSON configuration document and immediately validates
// it. It is safe to feed bytes received from a config center: an invalid
// document yields an error and never touches a running Manager.
func ParseConfig(data []byte) (*Config, error) {
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// DecodeConfig reads and validates one JSON configuration document from r.
func DecodeConfig(r io.Reader) (*Config, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return ParseConfig(data)
}
