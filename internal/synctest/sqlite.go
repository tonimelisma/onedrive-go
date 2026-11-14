package synctest

import (
	"net/url"
	"path/filepath"
)

// SQLiteFileURI is the SQLite URI a test outside internal/sync opens a state
// database with, carrying query. It builds exactly the URI the sync store
// builds (internal/sync holds the two equal): the path percent-encoded, since
// SQLite reads a '?' in a "file:" URI as the start of the query and a '#' as
// the start of a fragment; an empty authority for an absolute path
// ("file:///abs") and none for a relative one ("file:rel").
func SQLiteFileURI(path string, query url.Values) string {
	uri := url.URL{
		Scheme:   "file",
		Path:     path,
		OmitHost: !filepath.IsAbs(path),
		RawQuery: query.Encode(),
	}

	return uri.String()
}
