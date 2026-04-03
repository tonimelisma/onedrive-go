package config

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// decodeMode controls whether drive section parsing is strict (fail on first
// error) or lenient (collect errors as warnings, skip bad drives).
type decodeMode int

const (
	decodeModeStrict decodeMode = iota
	decodeModeLenient
)

// driveSectionDecoder owns the second TOML decode pass so strict and lenient
// config loading do not drift apart on how drive tables are interpreted.
type driveSectionDecoder struct{}

func newDriveSectionDecoder() driveSectionDecoder {
	return driveSectionDecoder{}
}

// decodeDriveSections performs the strict second TOML decode pass to extract
// drive sections. Returns an error on the first invalid section.
func decodeDriveSections(data []byte, cfg *Config) error {
	return newDriveSectionDecoder().decodeStrict(data, cfg)
}

// decodeDriveSectionsLenient is the lenient variant that collects errors as
// warnings instead of failing. Drives with structural issues are skipped.
func decodeDriveSectionsLenient(data []byte, cfg *Config) []ConfigWarning {
	return newDriveSectionDecoder().decodeLenient(data, cfg)
}

func (d driveSectionDecoder) decodeStrict(data []byte, cfg *Config) error {
	_, err := d.decode(data, cfg, decodeModeStrict)
	return err
}

func (d driveSectionDecoder) decodeLenient(data []byte, cfg *Config) []ConfigWarning {
	warnings, err := d.decode(data, cfg, decodeModeLenient)
	if err != nil {
		warnings = append(warnings, ConfigWarning{
			Message: fmt.Sprintf("decoding drive sections: %v", err),
		})
	}

	return warnings
}

// decode is the shared implementation for both strict and lenient drive
// section parsing. Drive sections have canonical IDs containing ":" as their
// key. In strict mode, the first error is returned immediately. In lenient
// mode, errors are collected as warnings and bad drives are skipped.
func (d driveSectionDecoder) decode(data []byte, cfg *Config, mode decodeMode) ([]ConfigWarning, error) {
	rawMap, err := decodeRawMap(data)
	if err != nil {
		if mode == decodeModeStrict {
			return nil, fmt.Errorf("drive sections: %w", err)
		}

		// Lenient: TOML re-decode failed — but the first decode already
		// succeeded, so this should be unreachable. Keep the lenient contract
		// and surface no additional warnings here.
		return nil, nil
	}

	var warnings []ConfigWarning

	for key, val := range rawMap {
		if !strings.Contains(key, ":") {
			continue
		}

		canonicalID, canonicalIDErr := driveid.NewCanonicalID(key)
		if canonicalIDErr != nil {
			if mode == decodeModeStrict {
				return nil, fmt.Errorf("drive section [%q]: invalid canonical ID: %w", key, canonicalIDErr)
			}

			warnings = append(warnings, ConfigWarning{
				Message: fmt.Sprintf("drive section [%q]: invalid canonical ID: %s", key, canonicalIDErr),
			})

			continue
		}

		driveMap, ok := val.(map[string]any)
		if !ok {
			if mode == decodeModeStrict {
				return nil, fmt.Errorf("drive section [%q] must be a table", key)
			}

			warnings = append(warnings, ConfigWarning{
				Message: fmt.Sprintf("drive section [%q] must be a table", key),
			})

			continue
		}

		unknownKeyErrs := collectDriveUnknownKeyErrors(driveMap, key)
		if mode == decodeModeStrict {
			if err := errors.Join(unknownKeyErrs...); err != nil {
				return nil, err
			}
		} else {
			warnings = appendWarnings(warnings, unknownKeyErrs)
		}

		var drive Drive
		if err := mapToDrive(driveMap, &drive); err != nil {
			if mode == decodeModeStrict {
				return nil, fmt.Errorf("drive section [%q]: %w", key, err)
			}

			warnings = append(warnings, ConfigWarning{
				Message: fmt.Sprintf("drive section [%q]: %s", key, err),
			})

			continue
		}

		cfg.Drives[canonicalID] = drive
	}

	return warnings, nil
}

func decodeRawMap(data []byte) (map[string]any, error) {
	var rawMap map[string]any
	if _, err := toml.Decode(string(data), &rawMap); err != nil {
		return nil, fmt.Errorf("decoding raw config map: %w", err)
	}

	return rawMap, nil
}

// mapToDrive converts a raw map to a Drive struct by re-encoding as TOML
// and decoding into the typed struct. This reuses the TOML library's type
// coercion rather than hand-writing map extraction for each field.
func mapToDrive(m map[string]any, d *Drive) error {
	var buf bytes.Buffer

	enc := toml.NewEncoder(&buf)
	if err := enc.Encode(m); err != nil {
		return fmt.Errorf("encoding drive data: %w", err)
	}

	if _, err := toml.Decode(buf.String(), d); err != nil {
		return fmt.Errorf("decoding drive data: %w", err)
	}

	return nil
}
