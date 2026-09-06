package app

import (
	"errors"
	"fmt"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("revision conflict")
	ErrUnauthorized = errors.New("unauthorized")
	ErrInvalid      = errors.New("invalid request")
	ErrUnsupported  = errors.New("unsupported")
	ErrUnavailable  = errors.New("runtime unavailable")
	ErrUncertain    = errors.New("effect outcome uncertain")
)

type ErrorCode string

const (
	CodeForbidden      ErrorCode = "forbidden"
	CodeNotFound       ErrorCode = "not_found"
	CodeConflict       ErrorCode = "conflict"
	CodeInvalidRequest ErrorCode = "invalid_request"
	CodeUnsupported    ErrorCode = "unsupported"
	CodeUnavailable    ErrorCode = "unavailable"
	CodeUncertain      ErrorCode = "uncertain"
	CodeInternal       ErrorCode = "internal"
)

type Error struct {
	Kind   error
	Detail string
}

func (e *Error) Error() string { return fmt.Sprintf("%v: %s", e.Kind, e.Detail) }

func (e *Error) Unwrap() error { return e.Kind }

func (e *Error) Code() string { return string(codeFor(e.Kind)) }

func Code(err error) string {
	var applicationError *Error
	if errors.As(err, &applicationError) {
		return applicationError.Code()
	}
	return string(codeFor(err))
}

func codeFor(err error) ErrorCode {
	switch {
	case errors.Is(err, ErrUnauthorized):
		return CodeForbidden
	case errors.Is(err, ErrNotFound):
		return CodeNotFound
	case errors.Is(err, ErrConflict):
		return CodeConflict
	case errors.Is(err, ErrInvalid):
		return CodeInvalidRequest
	case errors.Is(err, ErrUnsupported):
		return CodeUnsupported
	case errors.Is(err, ErrUnavailable):
		return CodeUnavailable
	case errors.Is(err, ErrUncertain):
		return CodeUncertain
	default:
		return CodeInternal
	}
}

func fail(kind error, format string, args ...any) error {
	return &Error{Kind: kind, Detail: fmt.Sprintf(format, args...)}
}
