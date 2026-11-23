package tokenfile

import (
	"encoding/json"
	"errors"
	"fmt"

	"golang.org/x/oauth2"
)

func marshalTokenFile(tok *oauth2.Token) ([]byte, error) {
	if tok == nil {
		return nil, errors.New("tokenfile: refusing to save nil token")
	}

	tf := File{Token: tok}

	data, err := json.MarshalIndent(tf, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("tokenfile: encoding: %w", err)
	}

	return data, nil
}
