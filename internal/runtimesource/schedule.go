package runtimesource

import "time"

const ManualRefreshDebounce = 10 * time.Second

var failureBackoff = [...]time.Duration{
	time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	time.Hour,
}

func nextRegularRefresh(now time.Time, interval time.Duration, random float64) *time.Time {
	if interval <= 0 {
		return nil
	}
	random = boundedRandom(random)
	jittered := time.Duration(float64(interval) * (0.9 + 0.2*random))
	next := now.UTC().Add(jittered)
	return &next
}

func nextFailureRefresh(
	now time.Time,
	attempts int,
	normalInterval time.Duration,
	retryAfter time.Duration,
	random float64,
) *time.Time {
	if normalInterval <= 0 {
		return nil
	}
	if retryAfter > 0 && retryAfter <= normalInterval {
		next := now.UTC().Add(retryAfter)
		return &next
	}
	index := attempts
	if index < 0 {
		index = 0
	}
	if index >= len(failureBackoff) {
		index = len(failureBackoff) - 1
	}
	delay := min(failureBackoff[index], normalInterval)
	delay += time.Duration(float64(delay) * 0.1 * boundedRandom(random))
	if delay > normalInterval {
		delay = normalInterval
	}
	next := now.UTC().Add(delay)
	return &next
}

func boundedRandom(value float64) float64 {
	switch {
	case value < 0:
		return 0
	case value > 1:
		return 1
	default:
		return value
	}
}
