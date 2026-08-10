// Package edgejwks — json shim. Centralized so tests can swap in
// fault-injecting decoders; production uses encoding/json.
package edgejwks

import "encoding/json"

func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}