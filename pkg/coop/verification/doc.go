// Package verification defines the advisory result contract produced by
// Co-op's session-owned passive observer and stored on session nodes.
//
// The package deliberately does not execute checks, observe applications,
// choose retries, or gate workflow progress. Consumers remain responsible for
// those decisions.
package verification
