// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package prunesched is the cluster-wide prune schedule: its label vocabulary,
// its parser and the arithmetic of when it runs.
//
// The schedule lives as CONTAINER labels on the agent service
// (TaskTemplate.ContainerSpec.Labels), because that is the one place an agent
// on a WORKER node can read: it inspects its own container through the local
// socket. Service and node labels would need the manager API, which a worker's
// agent has no access to. The price is that changing the schedule is a service
// update, so the agents restart one by one.
//
// Shared by the agent, which parses the labels at start and runs the schedule,
// and the cli, which edits them — so the two cannot disagree about what a
// label means or when "3 times a day from 00:00" is.
package prunesched

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	// The agent image carries no zoneinfo, and a schedule in Europe/Berlin
	// must not silently fail on it. Embedding costs ~450 KB.
	_ "time/tzdata"
)

// Prefix is common to every schedule label; a label with it belongs to this
// package, which is how the editor and `init --force` find them all.
const Prefix = "swarmexec.prune."

// The labels. No label at all means "disabled".
const (
	LabelEnabled         = Prefix + "enabled"          // true|false
	LabelDays            = Prefix + "schedule.days"    // mon,tue,…,sun (default every day)
	LabelStart           = Prefix + "schedule.start"   // HH:MM (default 00:00)
	LabelTimes           = Prefix + "schedule.times"   // runs per day, the day split evenly from start (default 1)
	LabelTZ              = Prefix + "schedule.tz"      // IANA zone (default UTC)
	LabelCategories      = Prefix + "categories"       // default containers,images,buildcache,networks
	LabelContainersUntil = Prefix + "containers.until" // Go duration (default 24h)
	LabelFilterLabel     = Prefix + "filter.label"     // optional: only objects with these labels
)

// Category names, as the labels and the cli spell them.
const (
	Containers     = "containers"
	Images         = "images"          // every unused image
	DanglingImages = "dangling-images" // untagged only
	BuildCache     = "buildcache"
	Networks       = "networks" // local scope only
	Volumes        = "volumes"  // named included; never a default
)

// Categories is every category, in the order a prune runs them: containers
// first, because removing them is what makes images, networks and volumes
// unused.
var Categories = []string{Containers, Images, DanglingImages, BuildCache, Networks, Volumes}

// DefaultCategories is what a schedule prunes when the label is absent.
// Volumes are deliberately not in it: they hold data.
var DefaultCategories = []string{Containers, Images, BuildCache, Networks}

// DefaultContainersUntil keeps stopped containers for a day, so the logs of a
// task that just crashed — which the cli falls back to — stay readable.
const DefaultContainersUntil = 24 * time.Hour

// MaxTimesPerDay bounds the runs per day. More than hourly is not a cleanup
// schedule any more, it is load.
const MaxTimesPerDay = 24

// dayNames is indexed by time.Weekday.
var dayNames = [7]string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// weekOrder is the order days are written in: Monday first.
var weekOrder = [7]time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Saturday, time.Sunday}

// Schedule is a parsed schedule.
type Schedule struct {
	Enabled         bool
	Days            [7]bool // indexed by time.Weekday
	Start           int     // minutes after midnight
	Times           int     // runs per day
	TZ              string  // IANA name
	Categories      []string
	ContainersUntil time.Duration
	LabelFilters    []string
}

// Default is the schedule the labels describe when they leave everything out,
// disabled.
func Default() Schedule {
	s := Schedule{Start: 0, Times: 1, TZ: "UTC", ContainersUntil: DefaultContainersUntil}
	for i := range s.Days {
		s.Days[i] = true
	}
	s.Categories = slices.Clone(DefaultCategories)
	return s
}

// HasLabels reports whether any schedule label is present.
func HasLabels(labels map[string]string) bool {
	for k := range labels {
		if strings.HasPrefix(k, Prefix) {
			return true
		}
	}
	return false
}

// Parse reads a schedule from container labels. With no schedule label at all
// it returns the disabled default and no error. Any label that does not parse
// is an error; the caller (the agent) then runs no schedule at all rather than
// a guess at one.
func Parse(labels map[string]string) (Schedule, error) {
	s := Default()
	var errs []error
	get := func(k string) (string, bool) {
		v, ok := labels[k]
		return strings.TrimSpace(v), ok
	}
	if v, ok := get(LabelEnabled); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s=%q: want true or false", LabelEnabled, v))
		}
		s.Enabled = b
	}
	if v, ok := get(LabelDays); ok {
		d, err := ParseDays(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", LabelDays, err))
		}
		s.Days = d
	}
	if v, ok := get(LabelStart); ok {
		m, err := ParseClock(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", LabelStart, err))
		}
		s.Start = m
	}
	if v, ok := get(LabelTimes); ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxTimesPerDay {
			errs = append(errs, fmt.Errorf("%s=%q: want a number from 1 to %d", LabelTimes, v, MaxTimesPerDay))
		}
		s.Times = n
	}
	if v, ok := get(LabelTZ); ok {
		if v == "" {
			v = "UTC"
		}
		if _, err := time.LoadLocation(v); err != nil {
			errs = append(errs, fmt.Errorf("%s=%q: unknown time zone", LabelTZ, v))
		}
		s.TZ = v
	}
	if v, ok := get(LabelCategories); ok {
		c, err := ParseCategories(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", LabelCategories, err))
		}
		s.Categories = c
	}
	if v, ok := get(LabelContainersUntil); ok {
		d, err := ParseUntil(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", LabelContainersUntil, err))
		}
		s.ContainersUntil = d
	}
	if v, ok := get(LabelFilterLabel); ok && v != "" {
		f, err := ParseLabelFilters(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", LabelFilterLabel, err))
		}
		s.LabelFilters = f
	}
	if err := errors.Join(errs...); err != nil {
		s.Enabled = false
		return s, err
	}
	return s, nil
}

// Validate checks a schedule built in code (the editor) by the same rules
// Parse applies to labels.
func (s Schedule) Validate() error {
	_, err := Parse(s.Labels())
	return err
}

// Labels renders the schedule as container labels. Every key is written, the
// defaults included, so what the agents run is visible in `docker service
// inspect` without knowing the defaults.
func (s Schedule) Labels() map[string]string {
	out := map[string]string{
		LabelEnabled:         strconv.FormatBool(s.Enabled),
		LabelDays:            FormatDays(s.Days),
		LabelStart:           FormatClock(s.Start),
		LabelTimes:           strconv.Itoa(s.Times),
		LabelTZ:              s.TZ,
		LabelCategories:      strings.Join(s.Categories, ","),
		LabelContainersUntil: FormatUntil(s.ContainersUntil),
	}
	if out[LabelTZ] == "" {
		out[LabelTZ] = "UTC"
	}
	if len(s.LabelFilters) > 0 {
		out[LabelFilterLabel] = strings.Join(s.LabelFilters, ",")
	}
	return out
}

// Apply returns existing with every schedule label replaced by s's: the other
// labels are kept exactly, and a schedule label s no longer writes (an
// emptied label filter) is removed rather than left behind.
func Apply(existing map[string]string, s Schedule) map[string]string {
	out := map[string]string{}
	for k, v := range existing {
		if !strings.HasPrefix(k, Prefix) {
			out[k] = v
		}
	}
	maps.Copy(out, s.Labels())
	return out
}

// Carry copies the schedule labels of from into into (allocating it when nil)
// and returns it. `init --force` builds a fresh agent spec from flags, which
// would otherwise drop a schedule the operator set in the TUI.
func Carry(from, into map[string]string) map[string]string {
	for k, v := range from {
		if !strings.HasPrefix(k, Prefix) {
			continue
		}
		if into == nil {
			into = map[string]string{}
		}
		into[k] = v
	}
	return into
}

// Location is the schedule's time zone. It falls back to UTC for a zone that
// does not load; Parse has already rejected those.
func (s Schedule) Location() *time.Location {
	if s.TZ == "" || s.TZ == "UTC" {
		return time.UTC
	}
	loc, err := time.LoadLocation(s.TZ)
	if err != nil {
		return time.UTC
	}
	return loc
}

// TimesOfDay are the run times, in minutes after midnight, ascending: the day
// split evenly from Start, wrapping past midnight. 2 runs from 00:00 are 00:00
// and 12:00; 3 runs from 22:00 are 06:00, 14:00 and 22:00.
func (s Schedule) TimesOfDay() []int {
	n := s.Times
	if n < 1 {
		n = 1
	}
	out := make([]int, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, (s.Start+i*(24*60)/n)%(24*60))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Next is the first run strictly after `after`, in the schedule's zone. A run
// belongs to the weekday on which it happens: with days=mon and a run time of
// 00:00, the run is Monday 00:00, not "the end of Sunday". ok is false when no
// day is selected.
func (s Schedule) Next(after time.Time) (next time.Time, ok bool) {
	loc := s.Location()
	local := after.In(loc)
	times := s.TimesOfDay()
	for d := 0; d <= 8; d++ {
		day := time.Date(local.Year(), local.Month(), local.Day()+d, 0, 0, 0, 0, loc)
		if !s.Days[day.Weekday()] {
			continue
		}
		for _, m := range times {
			t := time.Date(day.Year(), day.Month(), day.Day(), m/60, m%60, 0, 0, loc)
			if t.After(after) {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

// Upcoming lists the next n runs after `after`, for the editor's preview.
func (s Schedule) Upcoming(after time.Time, n int) []time.Time {
	var out []time.Time
	for len(out) < n {
		t, ok := s.Next(after)
		if !ok {
			break
		}
		out = append(out, t)
		after = t
	}
	return out
}

// ParseDays reads "mon,tue", ranges like "mon-fri", or "*"/"all"/"daily".
func ParseDays(v string) ([7]bool, error) {
	var d [7]bool
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" || v == "*" || v == "all" || v == "daily" {
		for i := range d {
			d[i] = true
		}
		return d, nil
	}
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if from, to, isRange := strings.Cut(part, "-"); isRange {
			a, ok1 := dayIndex(from)
			b, ok2 := dayIndex(to)
			if !ok1 || !ok2 {
				return d, fmt.Errorf("%q: not a day range (mon-fri)", part)
			}
			// Walk in week order, so sat-mon wraps over Sunday.
			ia, ib := slices.Index(weekOrder[:], a), slices.Index(weekOrder[:], b)
			for i := ia; ; i = (i + 1) % 7 {
				d[weekOrder[i]] = true
				if i == ib {
					break
				}
			}
			continue
		}
		wd, ok := dayIndex(part)
		if !ok {
			return d, fmt.Errorf("%q: not a day (mon, tue, wed, thu, fri, sat, sun)", part)
		}
		d[wd] = true
	}
	return d, nil
}

func dayIndex(s string) (time.Weekday, bool) {
	s = strings.TrimSpace(strings.ToLower(s))
	if len(s) > 3 {
		s = s[:3] // "monday" reads as "mon"
	}
	for i, n := range dayNames {
		if n == s {
			return time.Weekday(i), true
		}
	}
	return 0, false
}

// FormatDays writes the selected days Monday first, comma separated.
func FormatDays(d [7]bool) string {
	var parts []string
	for _, wd := range weekOrder {
		if d[wd] {
			parts = append(parts, dayNames[wd])
		}
	}
	return strings.Join(parts, ",")
}

// DayName is the label spelling of a weekday.
func DayName(wd time.Weekday) string { return dayNames[wd] }

// WeekOrder is the weekdays Monday first, the order the editor lists them.
func WeekOrder() [7]time.Weekday { return weekOrder }

// ParseClock reads "HH:MM" (24-hour) into minutes after midnight.
func ParseClock(v string) (int, error) {
	h, m, ok := strings.Cut(strings.TrimSpace(v), ":")
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if !ok || err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 || len(m) != 2 {
		return 0, fmt.Errorf("%q: want HH:MM, 00:00 to 23:59", v)
	}
	return hh*60 + mm, nil
}

// FormatClock writes minutes after midnight as HH:MM.
func FormatClock(min int) string { return fmt.Sprintf("%02d:%02d", min/60, min%60) }

// ParseCategories reads a comma-separated category list. Unknown names are an
// error; an empty list is too, since a schedule that prunes nothing is a typo.
func ParseCategories(v string) ([]string, error) {
	var out []string
	for _, c := range strings.Split(v, ",") {
		c = strings.ToLower(strings.TrimSpace(c))
		if c == "" {
			continue
		}
		if !slices.Contains(Categories, c) {
			return nil, fmt.Errorf("%q is not a category (%s)", c, strings.Join(Categories, ", "))
		}
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no categories")
	}
	return out, nil
}

// ParseUntil reads the containers age filter: a Go duration ("24h", "90m"),
// or "0" for no age filter.
func ParseUntil(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%q: want a duration like 24h or 90m, or 0", v)
	}
	return d, nil
}

// FormatUntil writes the age filter compactly ("24h", "1h30m", "0").
func FormatUntil(d time.Duration) string {
	if d <= 0 {
		return "0"
	}
	s := d.String() // "24h0m0s", "1h30m0s", "1m30s"
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

// ParseLabelFilters reads a comma-separated list of "key" or "key=value".
func ParseLabelFilters(v string) ([]string, error) {
	var out []string
	for _, f := range strings.Split(v, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		k, _, _ := strings.Cut(f, "=")
		if strings.TrimSpace(k) == "" || strings.ContainsAny(f, " \t") {
			return nil, fmt.Errorf("%q: want key or key=value", f)
		}
		out = append(out, f)
	}
	return out, nil
}
