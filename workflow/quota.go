package workflow

import (
	"math"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const quotaMaxAttempts = 5
const quotaMinDelay = time.Minute
const quotaMaxDelay = 6 * time.Hour

var resetHintPattern = regexp.MustCompile(`(?i)(?:resets?\s+(?:in|at)|try again\s+(?:in|at))\s+[^\n]+`)
var relativeReset = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*(weeks?|w|days?|d|hours?|hrs?|h|minutes?|mins?|m|seconds?|secs?|s)`)
var absoluteReset = regexp.MustCompile(`(?i)(?:resets?|resetting|try again)\s+(?:at|on)\s+(\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:\s*(?:[+-]\d{2}:?\d{2}|Z))?)`)
var clockReset = regexp.MustCompile(`(?i)(?:resets?|resetting|try again)\s+at\s+(\d{1,2}):(\d{2})\s*([AP]M)`)
var resetDate = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`)

func extractResetHint(text string) string {
	return strings.TrimSpace(resetHintPattern.FindString(text))
}

func parseResetHint(hint string, now time.Time) (time.Duration, bool) {
	if match := absoluteReset.FindStringSubmatch(hint); match != nil {
		value := strings.Replace(match[1], " ", "T", 1)
		value = strings.ReplaceAll(value, " ", "")
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05-0700", "2006-01-02T15:04Z07:00", "2006-01-02T15:04-0700", "2006-01-02T15:04:05", "2006-01-02T15:04"} {
			target, err := time.ParseInLocation(layout, value, now.Location())
			if err == nil {
				return max(0, target.Sub(now)), true
			}
		}
		return 0, false
	}
	if match := clockReset.FindStringSubmatch(hint); match != nil {
		hour, _ := strconv.Atoi(match[1])
		minute, _ := strconv.Atoi(match[2])
		if hour < 1 || hour > 12 || minute > 59 {
			return 0, false
		}
		hour %= 12
		if strings.EqualFold(match[3], "PM") {
			hour += 12
		}
		year, month, day := now.Date()
		date := resetDate.FindString(hint)
		if date != "" {
			parsed, err := time.Parse("2006-01-02", date)
			if err != nil {
				return 0, false
			}
			year, month, day = parsed.Date()
		}
		target := time.Date(year, month, day, hour, minute, 0, 0, now.Location())
		if date == "" && !target.Add(time.Minute).After(now) {
			target = target.AddDate(0, 0, 1)
		}
		return max(0, target.Sub(now)), true
	}
	var seconds float64
	found := false
	for _, indices := range relativeReset.FindAllStringSubmatchIndex(hint, -1) {
		end := indices[1]
		if end < len(hint) && ((hint[end] >= 'a' && hint[end] <= 'z') || (hint[end] >= 'A' && hint[end] <= 'Z')) {
			continue
		}
		value, _ := strconv.ParseFloat(hint[indices[2]:indices[3]], 64)
		unit := strings.ToLower(hint[indices[4]:indices[5]])
		multiplier := float64(1)
		switch unit[0] {
		case 'w':
			multiplier = 7 * 86400
		case 'd':
			multiplier = 86400
		case 'h':
			multiplier = 3600
		case 'm':
			multiplier = 60
		}
		seconds += value * multiplier
		found = true
	}
	if !found || math.IsNaN(seconds) {
		return 0, false
	}
	return time.Duration(min(seconds, float64(math.MaxInt64/int64(time.Second)))) * time.Second, true
}

func quotaDelay(hint string, attempt int, pause, now time.Time, random func() float64) time.Duration {
	base, ok := parseResetHint(hint, pause)
	if !ok {
		base = 5 * time.Minute
	}
	delay := float64(base) * math.Pow(2, float64(min(30, max(0, attempt-1)))) * (0.9 + random()*0.2)
	if delay >= float64(quotaMaxDelay) {
		delay = float64(quotaMaxDelay) * (1 - 0.1*random())
	}
	clamped := min(quotaMaxDelay, max(quotaMinDelay, time.Duration(min(delay, float64(quotaMaxDelay)))))
	return max(quotaMinDelay, clamped-max(0, now.Sub(pause)))
}

// All timer mutations occur under Manager.mu. A persisted armed count survives
// restart; cold start re-arms that count instead of spending another attempt.
func (m *Manager) cancelQuotaTimer(e *entry) {
	if e.quotaTimer != nil {
		e.quotaTimer.Stop()
		e.quotaTimer = nil
	}
}
func (m *Manager) armQuotaResume(e *entry, fresh bool) {
	m.cancelQuotaTimer(e)
	if m.ctx.Err() != nil {
		return
	}
	if fresh {
		e.run.AutoResumeAttempts++
		e.quotaRefusals = 0
	} else {
		e.run.AutoResumeAttempts = max(1, e.run.AutoResumeAttempts)
	}
	if e.run.AutoResumeAttempts > quotaMaxAttempts {
		e.run.AutoResumeAttempts = quotaMaxAttempts + 1
		e.run.ResumeAt = time.Time{}
		return
	}
	now := time.Now()
	pause := e.run.FinishedAt
	if pause.IsZero() {
		pause = now
	}
	delay := quotaDelay(e.run.ResetHint, e.run.AutoResumeAttempts, pause, now, rand.Float64)
	e.run.ResumeAt = now.Add(delay)
	m.persist(e)
	m.setQuotaTimer(e, delay)
}
func (m *Manager) setQuotaTimer(e *entry, delay time.Duration) {
	revision := e.run.Revision
	e.quotaTimer = time.AfterFunc(delay, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.ctx.Err() != nil || e.run.Revision != revision || e.run.Status != Paused || e.run.PauseReason != "usage_limit" {
			return
		}
		e.quotaTimer = nil
		if _, err := m.resumeLocked(e.run.OwnerSession, e.run.ID, ResumeOptions{}, true); err == nil {
			return
		}
		e.quotaRefusals++
		if e.quotaRefusals > 10 {
			e.run.ResumeAt = time.Time{}
			m.persist(e)
			return
		}
		e.run.ResumeAt = time.Now().Add(quotaMinDelay)
		m.persist(e)
		m.setQuotaTimer(e, quotaMinDelay)
	})
}
