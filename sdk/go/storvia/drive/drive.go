// Package drive provides Drive-specific types and helpers for the Storvia SDK.
//
// It is a modular sub-package of storvia: it imports the root storvia package
// for the core client and error types. The root storvia package does not import
// this package, so there are no import cycles.
//
// This package is primarily useful for type aliases and Drive-specific
// constants. Connection management functions live in the storage sub-package
// since they share the same client methods.
package drive

import (
	storvia "github.com/shovan04/storvia-sdk"
)

// DriveConnection is an alias for storvia.DriveConnection, provided for
// domain-specific imports.
type DriveConnection = storvia.DriveConnection

// ScopeDriveFile is the narrow per-file scope.
const ScopeDriveFile = storvia.ScopeDriveFile

// ScopeDriveAppData is the app-data scope for hidden storage.
const ScopeDriveAppData = storvia.ScopeDriveAppData
