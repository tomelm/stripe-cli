package checks

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
)

//go:embed catalog.json
var embeddedCatalog []byte

// LoadCatalog strictly decodes and validates the embedded check catalog.
func LoadCatalog() (Catalog, error) {
	return decodeCatalog(embeddedCatalog)
}

func decodeCatalog(data []byte) (Catalog, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var catalog Catalog
	if err := decoder.Decode(&catalog); err != nil {
		return Catalog{}, fmt.Errorf("decoding check catalog: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Catalog{}, err
	}
	if err := catalog.Validate(); err != nil {
		return Catalog{}, fmt.Errorf("validating check catalog: %w", err)
	}
	return catalog, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("decoding check catalog: unexpected trailing JSON value")
		}
		return fmt.Errorf("decoding check catalog: %w", err)
	}
	return nil
}
