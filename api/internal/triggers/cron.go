// Package triggers contains deterministic trigger input and occurrence rules.
// Scheduling belongs to the metadata service, never an always-on user Compute.
package triggers

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// Cron is a bounded five-field numeric UTC schedule. Month/day names, seconds,
// macros and local time zones are intentionally not part of this contract.
type Cron struct {
	fields   [5]map[int]bool
	wildcard [5]bool
}

var ErrCron = errors.New("invalid five-field numeric UTC cron")
var bounds = [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}

func decimal(text string) (int, error) {
	if len(text) == 0 || len(text) > 5 {
		return 0, ErrCron
	}
	for _, r := range text {
		if r < '0' || r > '9' {
			return 0, ErrCron
		}
	}
	value, err := strconv.Atoi(text)
	if err != nil {
		return 0, ErrCron
	}
	return value, nil
}

func ParseCron(text string) (Cron, error) {
	var out Cron
	if len(text) == 0 || len(text) > 128 || strings.ContainsAny(text, "\r\n\x00") {
		return out, ErrCron
	}
	for _, r := range text {
		if (r < 32 && r != '\t') || r > 126 {
			return out, ErrCron
		}
	}
	fields := strings.Fields(text)
	if len(fields) != 5 {
		return out, ErrCron
	}
	for index, field := range fields {
		minimum, maximum := bounds[index][0], bounds[index][1]
		out.fields[index] = map[int]bool{}
		for _, part := range strings.Split(field, ",") {
			step := 1
			tokens := strings.Split(part, "/")
			if len(tokens) > 2 {
				return Cron{}, ErrCron
			}
			if len(tokens) == 2 {
				var err error
				step, err = decimal(tokens[1])
				if err != nil || step < 1 {
					return Cron{}, ErrCron
				}
			}
			span := tokens[0]
			start, end := minimum, maximum
			if span == "*" && step == 1 {
				out.wildcard[index] = true
			}
			if span != "*" {
				values := strings.Split(span, "-")
				if len(values) > 2 {
					return Cron{}, ErrCron
				}
				var err error
				start, err = decimal(values[0])
				if err != nil {
					return Cron{}, ErrCron
				}
				end = start
				if len(values) == 2 {
					end, err = decimal(values[1])
					if err != nil {
						return Cron{}, ErrCron
					}
				} else if len(tokens) == 2 {
					end = maximum
				}
			}
			if start < minimum || end > maximum || start > end {
				return Cron{}, ErrCron
			}
			for value := start; value <= end; value += step {
				normalized := value
				if index == 4 && value == 7 {
					normalized = 0
				}
				out.fields[index][normalized] = true
			}
		}
		if len(out.fields[index]) == 0 {
			return Cron{}, ErrCron
		}
	}
	// Every possible month/day combination in a year-free UTC cron either has
	// an occurrence within eight years or is impossible (including century leap
	// gaps). This also rejects impossible dates instead of silently starving them.
	if _, err := out.Next(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		return Cron{}, err
	}
	return out, nil
}

func (c Cron) dayMatches(value time.Time) bool {
	dom, dow := c.fields[2][value.Day()], c.fields[4][int(value.Weekday())]
	if c.wildcard[2] {
		return dow
	}
	if c.wildcard[4] {
		return dom
	}
	return dom || dow // POSIX: restricted month-day and week-day are alternatives.
}

// Next is strictly after the input, rounded to a minute boundary in UTC. Month,
// day and hour skips keep rare leap-day schedules cheap, without minute scans
// across years. Input time zone and DST cannot change the UTC occurrence.
func (c Cron) Next(after time.Time) (time.Time, error) {
	for _, field := range c.fields {
		if len(field) == 0 {
			return time.Time{}, ErrCron
		}
	}
	value := after.UTC().Truncate(time.Minute).Add(time.Minute)
	lastYear := value.Year() + 8
	for value.Year() <= lastYear && value.Year() <= 9999 {
		if !c.fields[3][int(value.Month())] {
			value = time.Date(value.Year(), value.Month()+1, 1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if !c.dayMatches(value) {
			value = time.Date(value.Year(), value.Month(), value.Day()+1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if !c.fields[1][value.Hour()] {
			value = time.Date(value.Year(), value.Month(), value.Day(), value.Hour()+1, 0, 0, 0, time.UTC)
			continue
		}
		if !c.fields[0][value.Minute()] {
			value = value.Add(time.Minute)
			continue
		}
		return value, nil
	}
	return time.Time{}, ErrCron
}
