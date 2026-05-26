package images

import "regexp"

// sha256RE matches a lowercase-hex SHA-256 digest as the API/DB expect. We
// canonicalize to lowercase to make the UNIQUE constraint on images.sha256
// behave; agents and clients are expected to lowercase before submitting.
var sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// imageIDRE matches the 32-hex-char ID produced by newImageID/newUploadID.
var imageIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)
