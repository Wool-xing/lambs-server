package runtime

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Minimal 5-field cron parser (min hour dom month dow). Supports the
// standard subset: "*", "N", "a-b", "a,b", "*/n", "a-b/n". Standard cron
// dom/dow semantics: when BOTH are restricted, a day matches if EITHER
// matches.
type cronField struct {
	values [60]bool
	min    int
	max    int
}

type cronSpec struct {
	minute, hour, dom, month, dow cronField
	domRestricted, dowRestricted  bool
}

func parseCronField(spec string, min, max int) (cronField, bool, error) {
	f := cronField{min: min, max: max}
	star := false
	for _, part := range strings.Split(spec, ",") {
		step := 1
		base := part
		if i := strings.Index(part, "/"); i >= 0 {
			base = part[:i]
			n, err := strconv.Atoi(part[i+1:])
			if err != nil || n <= 0 {
				return f, false, fmt.Errorf("无效步进: %s", part)
			}
			step = n
		}
		lo, hi := min, max
		if base == "*" {
			star = true
		} else if i := strings.Index(base, "-"); i >= 0 {
			var err error
			lo, err = strconv.Atoi(base[:i])
			if err != nil {
				return f, false, fmt.Errorf("无效区间: %s", part)
			}
			hi, err = strconv.Atoi(base[i+1:])
			if err != nil {
				return f, false, fmt.Errorf("无效区间: %s", part)
			}
		} else {
			n, err := strconv.Atoi(base)
			if err != nil {
				return f, false, fmt.Errorf("无效字段: %s", part)
			}
			lo, hi = n, n
		}
		if lo < min || hi > max || lo > hi {
			return f, false, fmt.Errorf("数值越界: %s", part)
		}
		for v := lo; v <= hi; v += step {
			f.values[v-min] = true
		}
	}
	return f, !star, nil
}

// ParseCron validates a 5-field cron expression.
func ParseCron(expr string) error {
	_, err := parseCron(expr)
	return err
}

// parseCron parses a 5-field cron expression.
func parseCron(expr string) (*cronSpec, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return nil, fmt.Errorf("cron 需 5 个字段: 分 时 日 月 周")
	}
	s := &cronSpec{}
	var err error
	if s.minute, _, err = parseCronField(parts[0], 0, 59); err != nil {
		return nil, err
	}
	if s.hour, _, err = parseCronField(parts[1], 0, 23); err != nil {
		return nil, err
	}
	if s.dom, s.domRestricted, err = parseCronField(parts[2], 1, 31); err != nil {
		return nil, err
	}
	if s.month, _, err = parseCronField(parts[3], 1, 12); err != nil {
		return nil, err
	}
	if s.dow, s.dowRestricted, err = parseCronField(parts[4], 0, 6); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *cronSpec) matches(t time.Time) bool {
	if !s.minute.values[t.Minute()] || !s.hour.values[t.Hour()] {
		return false
	}
	if !s.month.values[int(t.Month())-1] {
		return false
	}
	domOK := s.dom.values[t.Day()-1]
	dowOK := s.dow.values[int(t.Weekday())]
	if s.domRestricted && s.dowRestricted {
		return domOK || dowOK
	}
	if s.domRestricted {
		return domOK
	}
	if s.dowRestricted {
		return dowOK
	}
	return true
}

// nextAfter returns the next fire time strictly after t, scanning at most
// 5 years. Returns zero time if the expression never fires.
func (s *cronSpec) nextAfter(t time.Time) time.Time {
	tDay := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	day := tDay
	for i := 0; i < 366*5; i++ {
		startMin := 0
		if day.Equal(tDay) {
			// strictly after t: skip all minutes of today up to and
			// including the current one
			startMin = t.Hour()*60 + t.Minute() + 1
		}
		for m := startMin; m < 1440; m++ {
			cand := day.Add(time.Duration(m) * time.Minute)
			if s.matches(cand) {
				return cand
			}
		}
		day = day.AddDate(0, 0, 1)
	}
	return time.Time{}
}
