package provider

import (
	"encoding/json"
	"io"
)

// encodeJSON marshals v to a byte slice. Returns an error rather than
// panicking so callers can surface the failure to the gateway's error
// pipeline rather than crashing the request goroutine.
func encodeJSON(v any) ([]byte, error) {
	return json.Marshal(v)
}

// decodeJSON reads from r into v. The decoder is permissive about
// unknown fields so future upstream additions do not break the gateway.
func decodeJSON(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	return dec.Decode(v)
}