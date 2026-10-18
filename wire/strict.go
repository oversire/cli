package wire

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Decode decodes data into v and fails on fields that the contract does not know.
func Decode(data []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.More() {
		return fmt.Errorf("wire: data after the JSON value")
	}
	return nil
}
