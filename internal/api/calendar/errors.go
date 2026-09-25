package calendar

import (
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// Provider errors (domain::ports::GoogleProviderError)
// ---------------------------------------------------------------------------

// ProviderErrorKind classifies a provider failure for retry/reauth policy.
type ProviderErrorKind int

const (
	// ProviderTransient covers transport/throttle/timeout failures that may recover.
	ProviderTransient ProviderErrorKind = iota
	// ProviderPermanent is a non-retryable request failure.
	ProviderPermanent
	// ProviderReauthRequired means the grant is invalid/revoked/insufficient.
	ProviderReauthRequired
	// ProviderSyncTokenExpired means the continuation token is dead; resync fully.
	ProviderSyncTokenExpired
)

// ProviderError is the typed Google Calendar failure crossing the provider port.
type ProviderError struct {
	Kind    ProviderErrorKind
	Message string
}

func (e *ProviderError) Error() string {
	return "Google Calendar provider request failed: " + e.Message
}

// ---------------------------------------------------------------------------
// Mutation errors (domain::ports::CalendarMutationError)
// ---------------------------------------------------------------------------

// MutationErrorCode is the machine-readable failure category on the wire.
type MutationErrorCode string

const (
	ErrCodeNotFound         MutationErrorCode = "not_found"
	ErrCodeOccurrenceGone   MutationErrorCode = "occurrence_not_found"
	ErrCodeReadOnly         MutationErrorCode = "read_only"
	ErrCodeNoWritable       MutationErrorCode = "no_writable_calendar"
	ErrCodeNotAttendee      MutationErrorCode = "not_attendee"
	ErrCodeInvalidInput     MutationErrorCode = "invalid_input"
	ErrCodeReauthRequired   MutationErrorCode = "reauth_required"
	ErrCodeProviderRejected MutationErrorCode = "provider_rejected"
	ErrCodeRetryable        MutationErrorCode = "retryable"
	ErrCodePersistFailed    MutationErrorCode = "persist_failed"
)

// MutationError mirrors domain::ports::CalendarMutationError.
type MutationError struct {
	Code    MutationErrorCode
	Message string
}

func (e *MutationError) Error() string { return e.Message }

func mutationErr(code MutationErrorCode, msg string) *MutationError {
	return &MutationError{Code: code, Message: msg}
}

var (
	// ErrMutationNotFound maps to 404.
	ErrMutationNotFound = &MutationError{ErrCodeNotFound, "calendar event was not found"}
	// ErrOccurrenceNotFound maps to 404.
	ErrOccurrenceNotFound = &MutationError{ErrCodeOccurrenceGone, "the targeted occurrence was not found on the recurring event; the calendar was out of date and has been refreshed"}
	// ErrReadOnly maps to 403.
	ErrReadOnly = &MutationError{ErrCodeReadOnly, "this calendar is read-only"}
	// ErrNoWritableCalendar maps to 409.
	ErrNoWritableCalendar = &MutationError{ErrCodeNoWritable, "no connected calendar can accept new events"}
	// ErrNotAttendee maps to 409.
	ErrNotAttendee = &MutationError{ErrCodeNotAttendee, "the connected account is not an attendee of this event"}
)

// providerMutationError maps a ProviderError to a MutationError, mirroring
// mutations::provider_error.
func providerMutationError(err error) error {
	var pe *ProviderError
	if !errors.As(err, &pe) {
		return &MutationError{ErrCodeRetryable, fmt.Sprintf("calendar mutation failed transiently: %v", err)}
	}
	switch pe.Kind {
	case ProviderReauthRequired:
		return &MutationError{ErrCodeReauthRequired, "calendar mutation requires reauthorization: " + pe.Message}
	case ProviderPermanent:
		return &MutationError{ErrCodeProviderRejected, "calendar provider rejected the mutation: " + pe.Message}
	default:
		return &MutationError{ErrCodeRetryable, "calendar mutation failed transiently: " + pe.Message}
	}
}

// ---------------------------------------------------------------------------
// Validation errors (domain::service::CalendarValidationError)
// ---------------------------------------------------------------------------

// ValidationError mirrors CalendarValidationError → HTTP 400.
type ValidationError struct{ Message string }

func (e *ValidationError) Error() string { return e.Message }

// MENTION_PREVIEWS_MAX mirrors MENTION_PREVIEWS_MAX.
const MentionPreviewsMax = 100

var (
	errInvalidRange = &ValidationError{"calendar occurrence range must be positive, no larger than 370 days, and inside the materialized one-year-history/two-year-future window"}
	errInvalidLimit = &ValidationError{"calendar repository query limit must be between 1 and 2001"}
	errTooMany      = &ValidationError{fmt.Sprintf("calendar mention previews accept at most %d events per request", MentionPreviewsMax)}
)
