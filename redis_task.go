package timewheel

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// envelopeSep 是信封各段的分隔符（ASCII unit separator）。
// 注意：Lua 侧（Redis 内置 Lua 5.1）无 \x 转义，脚本中写作 "\31"。
const envelopeSep = "\x1f"

// envelopeVersion 是信封格式版本号，位于信封首段。
const envelopeVersion = "v1"

// envelopeSegs 是信封的固定段数：v1|key|attempt|claimID|payload
const envelopeSegs = 5

// RTaskElement 任务明细（JSON 格式与存量数据保持兼容）
type RTaskElement struct {
	// 任务 key
	Key string `json:"key"`
	// 任务内容
	Msg string `json:"msg"`
	// 任务类型
	Type string `json:"type"`
	// 执行时间（秒级，展示与业务兼容用；内部 score 使用毫秒）
	ExecuteAtUnix int64 `json:"executeAtUnix"`
}

// NewRTaskElement 创建新任务
func NewRTaskElement(msg string, _type string) *RTaskElement {
	return &RTaskElement{
		Msg:  msg,
		Type: _type,
	}
}

// envelope 是 pending/processing zset member 的解析结果。
// member 格式：v1\x1f<key>\x1f<attempt>\x1f<claimID>\x1f<任务JSON>
type envelope struct {
	Key     string
	Attempt int
	ClaimID string
	Payload []byte
}

// encodeEnvelope 拼装信封 member
func encodeEnvelope(key string, attempt int, claimID string, payload []byte) string {
	return strings.Join([]string{
		envelopeVersion,
		key,
		strconv.Itoa(attempt),
		claimID,
		string(payload),
	}, envelopeSep)
}

// decodeEnvelope 解析信封 member。段数不足、版本不符或 attempt 非数字时返回错误。
func decodeEnvelope(member string) (envelope, error) {
	seg := strings.SplitN(member, envelopeSep, envelopeSegs)
	if len(seg) != envelopeSegs {
		return envelope{}, fmt.Errorf("malformed envelope: expect %d segments, got %d", envelopeSegs, len(seg))
	}
	if seg[0] != envelopeVersion {
		return envelope{}, fmt.Errorf("malformed envelope: unsupported version %q", seg[0])
	}
	attempt, err := strconv.Atoi(seg[2])
	if err != nil {
		return envelope{}, fmt.Errorf("malformed envelope: invalid attempt %q: %w", seg[2], err)
	}
	return envelope{
		Key:     seg[1],
		Attempt: attempt,
		ClaimID: seg[3],
		Payload: []byte(seg[4]),
	}, nil
}

// decodeTaskPayload 反序列化任务 JSON
func decodeTaskPayload(payload []byte) (*RTaskElement, error) {
	var task RTaskElement
	if err := json.Unmarshal(payload, &task); err != nil {
		return nil, fmt.Errorf("malformed task payload: %w", err)
	}
	return &task, nil
}

// Outcome 是一笔任务的执行结果类型
type Outcome string

const (
	// OutcomeSuccess handler 成功且 ACK 完成
	OutcomeSuccess Outcome = "success"
	// OutcomeRetryScheduled handler 失败，已按退避延期租约等待重试
	OutcomeRetryScheduled Outcome = "retry_scheduled"
	// OutcomeDeadLettered 达到最大尝试次数（或负载损坏），进入死信
	OutcomeDeadLettered Outcome = "dead_lettered"
	// OutcomeAckFailed handler 成功但 ACK 未送达（任务将由租约回收重投）
	OutcomeAckFailed Outcome = "ack_failed"
	// OutcomeRemoved 执行失败时任务已被 RemoveTask 取消，不再重试
	OutcomeRemoved Outcome = "removed"
)

// TaskResult 是结果钩子收到的单笔任务执行结果
type TaskResult struct {
	Task     *RTaskElement
	Attempt  int
	Outcome  Outcome
	Err      error
	Duration time.Duration
}

// Stats 是周期性统计快照
type Stats struct {
	PendingDepth    int64 // 当前分钟分片深度
	ProcessingDepth int64 // 在途（含重试等待）深度
	DeadDepth       int64 // 死信深度
	ClaimLagMs      int64 // 本统计周期内领取任务的执行延迟（now - executeAt）
}
