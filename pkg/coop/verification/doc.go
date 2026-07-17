// Package verification defines the inert data contract shared by Co-op
// verification producers.
//
// The package deliberately does not execute checks, observe applications,
// choose retries, or gate workflow progress. Consumers remain responsible for
// those decisions. Existing Co-op sessions do not use these types until a
// later feature explicitly adopts them.
package verification
