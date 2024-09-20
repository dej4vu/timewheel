package timewheel

import (
	"testing"
	"time"
)

func Test_timeWheel(t *testing.T) {
	timeWheel := NewTimeWheel(10, 500*time.Millisecond)
	defer timeWheel.Stop()

	start := time.Now()

	timeWheel.AddTask("test1", func() {
		now := time.Now()
		if isTimeBetween(now, start.Add(time.Second), start.Add(2*time.Second)) {
			t.Logf("test1, %v", now)
		} else {
			t.Errorf("test1, %v", now)
		}
	}, time.Now().Add(time.Second))

	timeWheel.AddTask("test2", func() {
		now := time.Now()
		if isTimeBetween(now, start.Add(5*time.Second), start.Add(6*time.Second)) {
			t.Logf("test2, %v", now)
		} else {
			t.Errorf("test2, %v", now)
		}
	}, time.Now().Add(5*time.Second))

	timeWheel.AddTask("test2", func() {
		now := time.Now()
		if isTimeBetween(now, start.Add(3*time.Second), start.Add(4*time.Second)) {
			t.Logf("test2, %v", now)
		} else {
			t.Errorf("test2, %v", now)
		}
	}, time.Now().Add(3*time.Second))

	<-time.After(6 * time.Second)
}

func isTimeBetween(t time.Time, begin time.Time, end time.Time) bool {
	return t.After(begin) && t.Before(end)
}
