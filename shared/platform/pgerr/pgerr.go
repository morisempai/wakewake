// Package pgerr identifies Postgres errors by SQLSTATE.
//
// This package is small but load-bearing. ADR-0003 puts the no-double-booking invariant in a
// Postgres exclusion constraint, which means the *only* signal that two customers raced for one
// slot is SQLSTATE 23P01 coming back from an INSERT. The service-template skill calls the
// mapping 23P01 -> ErrSlotUnavailable -> 409 reservation_overlap "a contract obligation, not an
// implementation detail".
//
// Matching on error text instead would work until Postgres changes a message or the server
// locale differs, at which point the invariant silently degrades into a 500 and the caller
// retries into the same wall. SQLSTATE codes are stable and locale-independent; use them.
package pgerr

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// SQLSTATE codes this project cares about. Full list: postgresql.org/docs/current/errcodes-appendix.html
const (
	// ExclusionViolation (23P01) is the no-double-booking invariant firing. It is the expected,
	// designed outcome of a lost race — not an error condition to be logged at error level.
	ExclusionViolation = "23P01"

	// UniqueViolation (23505) is a duplicate key: a replayed idempotency key, or a redelivered
	// event hitting the consumer dedupe table.
	UniqueViolation = "23505"

	// ForeignKeyViolation (23503) means a referenced row is missing.
	ForeignKeyViolation = "23503"

	// CheckViolation (23514) is a domain rule enforced in the schema, e.g. a reservation window
	// that normalises to an empty range (ADR-0011).
	CheckViolation = "23514"

	// NotNullViolation (23502).
	NotNullViolation = "23502"

	// SerializationFailure (40001) and DeadlockDetected (40P01) are both retryable: the
	// transaction did not happen and re-running it is safe and expected.
	SerializationFailure = "40001"
	DeadlockDetected     = "40P01"
)

// Code returns the SQLSTATE of err, or "" if it is not a Postgres server error. It unwraps, so
// an error wrapped with %w through several layers still reports its code.
func Code(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// Is reports whether err is a Postgres error with the given SQLSTATE.
//
//	if pgerr.Is(err, pgerr.ExclusionViolation) {
//	    return domain.ErrSlotUnavailable
//	}
func Is(err error, code string) bool {
	return Code(err) == code
}

// ConstraintName returns the name of the violated constraint, or "".
//
// Useful when one table has several constraints of the same class and they mean different
// things: a 23514 from reservation_window_not_empty is a client sending an empty window, while
// one from reservation_held_iff_expiry is an internal bug. Both are 23514; only the constraint
// name distinguishes them.
func ConstraintName(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

// IsRetryable reports whether err is a transient serialization failure or deadlock, where the
// transaction provably did not take effect and re-running it is safe.
//
// It deliberately does NOT include connection errors: those leave the outcome unknown, and
// blindly retrying an unknown-outcome write is how a single booking becomes two.
func IsRetryable(err error) bool {
	switch Code(err) {
	case SerializationFailure, DeadlockDetected:
		return true
	default:
		return false
	}
}
