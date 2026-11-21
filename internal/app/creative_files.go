package app

import (
	"github.com/Bengo-Hub/httpware/objectstore"

	"github.com/bengobox/marketflow-api/internal/modules/creative"
)

// creativeFiles is the media store as the Studio's file deleter, or nil when storage is not
// configured (a permanent version delete then refuses rather than leave files behind).
func creativeFiles(store *objectstore.Store) creative.FileDeleter {
	if store == nil || !store.Enabled() {
		return nil
	}
	return store
}
