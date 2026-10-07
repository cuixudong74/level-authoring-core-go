package levelcore

import (
	"errors"
	"fmt"
)

// Kind classifies every error produced by the kernel so callers can
// distinguish rejection reasons programmatically.
type Kind int

const (
	KindUnknown Kind = iota
	KindInvalidArgument
	KindNotFound
	KindDuplicate
	KindOutOfBounds
	KindBadReference
	KindOverlap
	KindIllegalOwner
	KindUnsupportedVersion
	KindCorrupt
)

func (k Kind) String() string { return kindNames[k] }

var kindNames = [...]string{
	"unknown", "invalid_argument", "not_found", "duplicate",
	"out_of_bounds", "bad_reference", "overlap", "illegal_owner",
	"unsupported_version", "corrupt",
}

// Error is the structured error type used for edit rejections and
// import failures. CommandIndex is set when the failure belongs to a
// specific command inside a batch (-1 otherwise).
type Error struct {
	Kind         Kind
	Op           string
	CommandIndex int
	Detail       string
	Err          error
}

func (e *Error) Error() string {
	s := e.Op
	if s == "" {
		s = "levelcore"
	}
	if e.CommandIndex >= 0 {
		s = fmt.Sprintf("%s: command[%d]", s, e.CommandIndex)
	}
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	if e.Kind != KindUnknown {
		s = fmt.Sprintf("%s (%s)", s, e.Kind)
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

func newError(kind Kind, op, detail string, idx int) *Error {
	return &Error{Kind: kind, Op: op, CommandIndex: idx, Detail: detail}
}

// IsKind reports whether err is a kernel *Error of the given kind.
func IsKind(err error, kind Kind) bool {
	var ke *Error
	if errors.As(err, &ke) {
		return ke.Kind == kind
	}
	return false
}

// Sentinel errors used by the versioned import/export layer.
var (
	ErrUnsupportedVersion = errors.New("unsupported bundle format version")
	ErrCorrupt            = errors.New("corrupt bundle")
)
