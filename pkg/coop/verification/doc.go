// Package verification defines the durable result contract for Co-op's
// automatic Stripe resource verification, plus the sanitizer and bounded
// storage helpers used to persist results on session nodes.
//
// The package does not execute checks or talk to Stripe; pkg/coop/resourcecheck
// produces results and pkg/coop/workflow decides how they gate the node
// lifecycle.
package verification
