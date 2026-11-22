package util

import "time"

const (
	YYYY_MM_DD_HH_MM = "2006-01-02-15:04"
)

func GetTimeMinuteStr(t time.Time) string {
	return t.Format(YYYY_MM_DD_HH_MM)
}

func GetTimeSecond(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.Local)
}

// MinuteStartMs 返回 t 所在分钟的起始毫秒时间戳（本地时区）
func MinuteStartMs(t time.Time) int64 {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, time.Local).UnixMilli()
}
