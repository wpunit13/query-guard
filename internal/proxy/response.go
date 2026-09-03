package proxy

import (
	"encoding/json"
	"net/http"
)

// ──────────────────────────────────────────────────────────────────────────────
// Standard JSON rejection formatter
// ──────────────────────────────────────────────────────────────────────────────

// errorCodeLimitBreach is the canonical error_code returned for any 4xx
// rejection produced by query-guard.
const errorCodeLimitBreach = "QUERY_GUARD_LIMIT_BREACH"

// RejectionReason labels the stage of the pipeline that rejected a query.
type RejectionReason string

const (
	ReasonTableBlocklist     RejectionReason = "TABLE_BLOCKLIST"
	ReasonRequiredFilter     RejectionReason = "REQUIRED_FILTER_MISSING"
	ReasonStatementBlocklist RejectionReason = "STATEMENT_BLOCKLIST"
	ReasonCostLimitBreach    RejectionReason = "COST_LIMIT_BREACH"
)

// rejectionResponse is the wire schema returned for rejected queries.
type rejectionResponse struct {
	ErrorCode string          `json:"error_code"`
	Reason    RejectionReason `json:"reason"`
	Message   string          `json:"message"`
	// RequestID correlates the rejection with the guard's log lines (and
	// echoes an inbound X-Request-ID when the client supplied one).
	RequestID string `json:"request_id,omitempty"`
}

// WriteRejection renders a standard JSON 4xx response for a rejected query.
// requestID may be empty (omitted from the body).
func WriteRejection(w http.ResponseWriter, status int, reason RejectionReason, message, requestID string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Encoding errors are not actionable here; the status line is already sent.
	_ = json.NewEncoder(w).Encode(rejectionResponse{
		ErrorCode: errorCodeLimitBreach,
		Reason:    reason,
		Message:   message,
		RequestID: requestID,
	})
}
