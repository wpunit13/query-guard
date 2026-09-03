package parser

import "errors"

// Package-level typed errors returned by the parser sub-package.
// According to the fail-open policy, ErrParserUnsupported triggers a bypass.
var (
	// ErrParserUnsupported is returned when the SQL syntax cannot be parsed.
	// The caller MUST treat this as a fail-open signal and forward the query.
	ErrParserUnsupported = errors.New("parser: unsupported SQL syntax")

	// ErrEmptyStatement is returned when the SQL string is blank or whitespace-only.
	ErrEmptyStatement = errors.New("parser: empty statement")

	// ErrStatementBlocked is returned when the statement verb matches a
	// blocked statement type from the policy.
	ErrStatementBlocked = errors.New("parser: statement type is blocked")
)
