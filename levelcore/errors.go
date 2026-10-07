package levelcore

import (
	"errors"
	"fmt"
)

// RejectCode 是对编辑被拒原因的稳定分类标识，调用方可据此区分处理逻辑，
// 其取值在版本间保持稳定。
type RejectCode string

const (
	RejectNone RejectCode = ""
	// RejectOutOfBounds 坐标越过地图/图层/区域边界。
	RejectOutOfBounds RejectCode = "OUT_OF_BOUNDS"
	// RejectDuplicateID 同类型对象 ID 重复。
	RejectDuplicateID RejectCode = "DUPLICATE_ID"
	// RejectNotFound 被引用对象不存在。
	RejectNotFound RejectCode = "NOT_FOUND"
	// RejectOverlap 同层 Solid 实体占据了同一格。
	RejectOverlap RejectCode = "OVERLAP"
	// RejectIllegalOwner 实体归属了不存在或非法的图层。
	RejectIllegalOwner RejectCode = "ILLEGAL_OWNER"
	// RejectInvalidValue 尺寸/ID/矩形/取值非法。
	RejectInvalidValue RejectCode = "INVALID_VALUE"
	// RejectCorrupt 输入数据损坏或无法解析。
	RejectCorrupt RejectCode = "CORRUPT"
	// RejectUnknownVersion 出现不支持的格式版本。
	RejectUnknownVersion RejectCode = "UNKNOWN_VERSION"
	// RejectChecksum 校验和不一致。
	RejectChecksum RejectCode = "CHECKSUM_MISMATCH"
)

// RejectError 携带可定位到具体命令的拒绝原因。
// CommandIndex 对应当前批次中的命令下标；单条提交时为 0；
// 非命令来源（如 UnmarshalDocument）时为 -1。
type RejectError struct {
	Code         RejectCode
	CommandIndex int
	Message      string
	Cause        error
}

func rejectf(idx int, code RejectCode, format string, args ...any) *RejectError {
	return &RejectError{
		Code:         code,
		CommandIndex: idx,
		Message:      fmt.Sprintf(format, args...),
	}
}

func wrapReject(idx int, code RejectCode, cause error, format string, args ...any) *RejectError {
	e := rejectf(idx, code, format, args...)
	e.Cause = cause
	return e
}

func (e *RejectError) Error() string {
	loc := "document"
	if e.CommandIndex >= 0 {
		loc = fmt.Sprintf("command[%d]", e.CommandIndex)
	}
	if e.Cause != nil {
		return fmt.Sprintf("levelcore: %s: %s: %s (cause: %v)", e.Code, loc, e.Message, e.Cause)
	}
	return fmt.Sprintf("levelcore: %s: %s: %s", e.Code, loc, e.Message)
}

// Unwrap 支持 errors.Is/As 取出底层原因。
func (e *RejectError) Unwrap() error { return e.Cause }

// AsReject 尝试把 error 解释为 *RejectError。
func AsReject(err error) (*RejectError, bool) {
	var re *RejectError
	if errors.As(err, &re) {
		return re, true
	}
	return nil, false
}
