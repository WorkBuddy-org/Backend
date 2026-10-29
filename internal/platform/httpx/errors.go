package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// Code is the machine-readable error code in every error response. Clients
// branch on it; the message is for debugging only and may change.
type Code string

const (
	CodeEmailExists        Code = "EMAIL_EXISTS"
	CodeInvalidCredentials Code = "INVALID_CREDENTIALS"
	CodeValidation         Code = "VALIDATION_ERROR"
	CodeUnauthorized       Code = "UNAUTHORIZED"
	CodeTokenExpired       Code = "TOKEN_EXPIRED"
	CodeForbidden          Code = "FORBIDDEN"
	CodeNotFound           Code = "NOT_FOUND"
	CodeMethodNotAllowed   Code = "METHOD_NOT_ALLOWED"
	CodeConflict           Code = "CONFLICT"
	CodeRoomFull           Code = "ROOM_FULL"
	CodeAlreadyReserved    Code = "ALREADY_RESERVED"
	CodeRoomEnded          Code = "ROOM_ENDED"
	CodeExtendLimitReached Code = "EXTEND_LIMIT_REACHED"
	CodeInviteExpired      Code = "INVITE_EXPIRED"
	CodeInviteInvalid      Code = "INVITE_INVALID"
	CodeFileTooLarge       Code = "FILE_TOO_LARGE"
	CodeUnsupportedFile    Code = "UNSUPPORTED_FILE_TYPE"
	CodeRateLimited        Code = "RATE_LIMITED"
	CodeServiceUnavailable Code = "SERVICE_UNAVAILABLE"
	CodeInternal           Code = "INTERNAL"
)

// Per-field reasons used in details.fields of a VALIDATION_ERROR.
const (
	FieldRequired      = "REQUIRED"
	FieldInvalidFormat = "INVALID_FORMAT"
	FieldInvalidValue  = "INVALID_VALUE"
	FieldInvalidType   = "INVALID_TYPE"
	FieldInvalidLength = "INVALID_LENGTH"
	FieldTooShort      = "TOO_SHORT"
	FieldTooLong       = "TOO_LONG"
	FieldUnknown       = "UNKNOWN_FIELD"
)

// internalMessage is the only message a 500 ever carries.
const internalMessage = "internal server error"

// maxBodyBytes caps JSON request bodies.
const maxBodyBytes = 1 << 20

// Fields maps a request field name to its reason, e.g. {"email": "INVALID_FORMAT"}.
type Fields map[string]string

type ErrorBody struct {
	Code    Code           `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// Error writes {"error": {"code": code, "message": message}}.
func Error(w http.ResponseWriter, status int, code Code, message string) {
	ErrorWithDetails(w, status, code, message, nil)
}

// ErrorWithDetails is Error with a details object; empty details are omitted.
func ErrorWithDetails(w http.ResponseWriter, status int, code Code, message string, details map[string]any) {
	if len(details) == 0 {
		details = nil
	}
	JSON(w, status, ErrorResponse{Error: ErrorBody{Code: code, Message: message, Details: details}})
}

// Validation writes a 400 VALIDATION_ERROR, with details.fields when fields
// is not empty.
func Validation(w http.ResponseWriter, message string, fields Fields) {
	var details map[string]any
	if len(fields) > 0 {
		details = map[string]any{"fields": fields}
	}
	ErrorWithDetails(w, http.StatusBadRequest, CodeValidation, message, details)
}

// InvalidPathID writes a 400 VALIDATION_ERROR for a malformed {id} path
// parameter.
func InvalidPathID(w http.ResponseWriter, what string) {
	Validation(w, "invalid "+what+" id", Fields{"id": FieldInvalidFormat})
}

func Unauthorized(w http.ResponseWriter, message string) {
	Error(w, http.StatusUnauthorized, CodeUnauthorized, message)
}

func Forbidden(w http.ResponseWriter, message string) {
	Error(w, http.StatusForbidden, CodeForbidden, message)
}

func NotFound(w http.ResponseWriter, message string) {
	Error(w, http.StatusNotFound, CodeNotFound, message)
}

// RateLimited sets Retry-After and writes a 429 RATE_LIMITED.
func RateLimited(w http.ResponseWriter, retryAfter time.Duration, message string) {
	RetryAfter(w, retryAfter)
	Error(w, http.StatusTooManyRequests, CodeRateLimited, message)
}

// Internal logs err with the given context and writes a 500 INTERNAL with a
// generic message; err never reaches the client.
func Internal(w http.ResponseWriter, logMsg string, err error, args ...any) {
	slog.Error(logMsg, append(args, "error", err)...)
	Error(w, http.StatusInternalServerError, CodeInternal, internalMessage)
}

// DecodeJSON decodes a single JSON object from the body into dst, rejecting
// unknown fields and bodies over 1 MiB. Pass a failure to DecodeError.
func DecodeJSON(r *http.Request, dst any) error {
	defer func() { _ = r.Body.Close() }()

	decoder := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()
	return decoder.Decode(dst)
}

// DecodeError writes the 400 VALIDATION_ERROR for a DecodeJSON failure,
// naming the offending field when the decoder reports one.
func DecodeError(w http.ResponseWriter, err error) {
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.Is(err, io.EOF):
		Validation(w, "request body is required", nil)
	case errors.As(err, &typeErr) && typeErr.Field != "":
		Validation(w, fmt.Sprintf("%s has the wrong type", typeErr.Field), Fields{typeErr.Field: FieldInvalidType})
	default:
		// encoding/json has no typed error for unknown fields.
		if name, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
			name = strings.Trim(name, `"`)
			Validation(w, "unknown field "+name, Fields{name: FieldUnknown})
			return
		}
		Validation(w, "invalid request body", nil)
	}
}

// NotFoundHandler and MethodNotAllowedHandler replace the router's plain-text
// defaults so unmatched routes use the error format too.
func NotFoundHandler(w http.ResponseWriter, _ *http.Request) {
	NotFound(w, "route not found")
}

func MethodNotAllowedHandler(w http.ResponseWriter, _ *http.Request) {
	Error(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
}

// Recoverer turns a panic in a handler into a logged 500 INTERNAL.
// http.ErrAbortHandler is re-panicked so net/http can abort the response.
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(rec)
			}
			slog.Error("panic in handler", "panic", rec, "method", r.Method, "path", r.URL.Path, "stack", string(debug.Stack()))
			if r.Header.Get("Connection") != "Upgrade" {
				Error(w, http.StatusInternalServerError, CodeInternal, internalMessage)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
