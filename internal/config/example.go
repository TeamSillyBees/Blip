package config

import (
	_ "embed"
	"encoding/json"
)

//go:embed example.json
var Example []byte

// ExampleBytes creates the initial sample configuration with this computer's
// hostname. The disabled examples use literal settings for the user to replace.
func ExampleBytes() ([]byte, error) {
	var c Config
	if err := json.Unmarshal(Example, &c); err != nil {
		return nil, err
	}
	c.DeviceName = DefaultDeviceName()
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
