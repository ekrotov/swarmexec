// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package prunesched

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestNoLabelsIsDisabledDefault(t *testing.T) {
	s, err := Parse(map[string]string{"com.docker.stack.namespace": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Enabled {
		t.Error("no labels must mean disabled")
	}
	if s.TZ != "UTC" || s.Start != 0 || s.Times != 1 || s.ContainersUntil != 24*time.Hour {
		t.Errorf("defaults wrong: %+v", s)
	}
	if !slices.Equal(s.Categories, []string{"containers", "images", "buildcache", "networks"}) {
		t.Errorf("default categories = %v; volumes must not be a default", s.Categories)
	}
	for wd, on := range s.Days {
		if !on {
			t.Errorf("default days miss %v", time.Weekday(wd))
		}
	}
}

func TestParseFull(t *testing.T) {
	s, err := Parse(map[string]string{
		LabelEnabled:         "true",
		LabelDays:            "mon, Wed,fri",
		LabelStart:           "02:30",
		LabelTimes:           "3",
		LabelTZ:              "Europe/Berlin",
		LabelCategories:      "containers,volumes",
		LabelContainersUntil: "48h",
		LabelFilterLabel:     "env=test,scratch",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Enabled || s.Start != 150 || s.Times != 3 || s.TZ != "Europe/Berlin" || s.ContainersUntil != 48*time.Hour {
		t.Errorf("parsed %+v", s)
	}
	if FormatDays(s.Days) != "mon,wed,fri" {
		t.Errorf("days = %s", FormatDays(s.Days))
	}
	if !slices.Equal(s.Categories, []string{"containers", "volumes"}) {
		t.Errorf("categories = %v", s.Categories)
	}
	if !slices.Equal(s.LabelFilters, []string{"env=test", "scratch"}) {
		t.Errorf("filters = %v", s.LabelFilters)
	}
}

func TestParseDaysRanges(t *testing.T) {
	cases := map[string]string{
		"mon-fri": "mon,tue,wed,thu,fri",
		"sat-mon": "mon,sat,sun",
		"*":       "mon,tue,wed,thu,fri,sat,sun",
		"sunday":  "sun",
	}
	for in, want := range cases {
		d, err := ParseDays(in)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if got := FormatDays(d); got != want {
			t.Errorf("%s = %s, want %s", in, got, want)
		}
	}
}

// Every invalid label disables the schedule with an error naming the label —
// a typo must never turn into a schedule nobody asked for.
func TestInvalidLabelsDisable(t *testing.T) {
	bad := map[string]string{
		LabelEnabled:         "yes please",
		LabelDays:            "mon,funday",
		LabelStart:           "24:00",
		LabelTimes:           "0",
		LabelTZ:              "Mars/Olympus",
		LabelCategories:      "containers,everything",
		LabelContainersUntil: "a day",
		LabelFilterLabel:     "=x",
	}
	for k, v := range bad {
		labels := map[string]string{LabelEnabled: "true", k: v}
		s, err := Parse(labels)
		if err == nil {
			t.Errorf("%s=%q parsed without error", k, v)
			continue
		}
		if s.Enabled {
			t.Errorf("%s=%q: invalid labels must disable the schedule", k, v)
		}
		if !strings.Contains(err.Error(), k) {
			t.Errorf("error %q does not name the label %s", err, k)
		}
	}
	if _, err := Parse(map[string]string{LabelStart: "7:5"}); err == nil {
		t.Error("7:5 is not HH:MM")
	}
	if _, err := Parse(map[string]string{LabelTimes: "25"}); err == nil {
		t.Error("25 runs a day exceeds the limit")
	}
}

func TestTimesOfDaySplitsTheDayFromStart(t *testing.T) {
	cases := []struct {
		start, times int
		want         []string
	}{
		{0, 1, []string{"00:00"}},
		{0, 2, []string{"00:00", "12:00"}},
		{0, 3, []string{"00:00", "08:00", "16:00"}},
		{22 * 60, 3, []string{"06:00", "14:00", "22:00"}}, // wraps past midnight
		{90, 4, []string{"01:30", "07:30", "13:30", "19:30"}},
	}
	for _, c := range cases {
		s := Default()
		s.Start, s.Times = c.start, c.times
		var got []string
		for _, m := range s.TimesOfDay() {
			got = append(got, FormatClock(m))
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("start %s ×%d = %v, want %v", FormatClock(c.start), c.times, got, c.want)
		}
	}
}

func mustTime(t *testing.T, s string, loc *time.Location) time.Time {
	t.Helper()
	tm, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func TestNextAcrossMidnightAndWeekdays(t *testing.T) {
	s := Default()
	s.Enabled = true
	s.Start, s.Times = 22*60, 2 // 10:00 and 22:00
	s.Days, _ = ParseDays("fri")

	// 2026-11-05 is a Thursday.
	cases := []struct{ now, want string }{
		{"2026-11-05 23:00", "2026-11-06 10:00"}, // Thursday night → Friday morning
		{"2026-11-06 10:00", "2026-11-06 22:00"}, // strictly after: not the run that is now
		{"2026-11-06 22:30", "2026-11-13 10:00"}, // after Friday's last run → next Friday
		{"2026-11-07 00:00", "2026-11-13 10:00"},
	}
	for _, c := range cases {
		got, ok := s.Next(mustTime(t, c.now, time.UTC))
		if !ok || got.Format("2006-01-02 15:04") != c.want {
			t.Errorf("Next(%s) = %v, want %s", c.now, got, c.want)
		}
	}
}

func TestNextUsesTheScheduleTimeZone(t *testing.T) {
	s := Default()
	s.Enabled, s.TZ = true, "Europe/Berlin" // UTC+1 in November
	s.Days, _ = ParseDays("mon")
	// Sunday 23:30 UTC is already Monday 00:30 in Berlin, so Monday's 00:00 has
	// passed there: the next run is the following Monday.
	now := mustTime(t, "2026-11-08 23:30", time.UTC)
	got, _ := s.Next(now)
	berlin, _ := time.LoadLocation("Europe/Berlin")
	if want := mustTime(t, "2026-11-16 00:00", berlin); !got.Equal(want) {
		t.Errorf("Next = %v, want %v", got, want)
	}
	if got.Location().String() != "Europe/Berlin" {
		t.Errorf("result zone = %s", got.Location())
	}
}

func TestNextWithNoDaysNeverRuns(t *testing.T) {
	s := Default()
	s.Days = [7]bool{}
	if _, ok := s.Next(time.Now()); ok {
		t.Error("a schedule with no days must not produce a run")
	}
}

// The editor rewrites the schedule labels and nothing else; the agent spec
// carries labels the schedule does not own.
func TestApplyKeepsOtherLabelsAndDropsStaleOnes(t *testing.T) {
	existing := map[string]string{
		"swarmexec.role":   "agent",
		"team":             "ops",
		LabelFilterLabel:   "old=1",
		Prefix + "unknown": "left from a newer client",
	}
	s := Default()
	s.Enabled = true
	got := Apply(existing, s)
	if got["swarmexec.role"] != "agent" || got["team"] != "ops" {
		t.Errorf("other labels lost: %v", got)
	}
	if _, ok := got[LabelFilterLabel]; ok {
		t.Error("an emptied filter must remove its label")
	}
	if _, ok := got[Prefix+"unknown"]; ok {
		t.Error("stale schedule labels must not survive a save")
	}
	if existing[LabelFilterLabel] != "old=1" {
		t.Error("Apply must not modify its input")
	}
	back, err := Parse(got)
	if err != nil || !back.Enabled || FormatDays(back.Days) != FormatDays(s.Days) {
		t.Errorf("round trip: %+v, %v", back, err)
	}
}

func TestLabelsRoundTrip(t *testing.T) {
	s := Default()
	s.Enabled = true
	s.Days, _ = ParseDays("tue,sat")
	s.Start, s.Times, s.TZ = 75, 5, "America/New_York"
	s.Categories = []string{Images, Volumes}
	s.ContainersUntil = 90 * time.Minute
	s.LabelFilters = []string{"a=b"}
	back, err := Parse(s.Labels())
	if err != nil {
		t.Fatal(err)
	}
	if back.Enabled != s.Enabled || back.Days != s.Days || back.Start != s.Start || back.Times != s.Times ||
		back.TZ != s.TZ || !slices.Equal(back.Categories, s.Categories) ||
		back.ContainersUntil != s.ContainersUntil || !slices.Equal(back.LabelFilters, s.LabelFilters) {
		t.Errorf("round trip changed it:\n%+v\n%+v", s, back)
	}
}

func TestCarryCopiesOnlyScheduleLabels(t *testing.T) {
	from := map[string]string{LabelEnabled: "true", LabelTimes: "2", "other": "x"}
	got := Carry(from, nil)
	if len(got) != 2 || got[LabelTimes] != "2" {
		t.Errorf("Carry = %v", got)
	}
	if Carry(map[string]string{"other": "x"}, nil) != nil {
		t.Error("nothing to carry must not allocate labels")
	}
}

func TestFormatUntil(t *testing.T) {
	for d, want := range map[time.Duration]string{
		24 * time.Hour: "24h", 90 * time.Minute: "1h30m", 30 * time.Minute: "30m", 0: "0", 90 * time.Second: "1m30s",
	} {
		if got := FormatUntil(d); got != want {
			t.Errorf("FormatUntil(%v) = %s, want %s", d, got, want)
		}
		if back, err := ParseUntil(FormatUntil(d)); err != nil || back != d {
			t.Errorf("ParseUntil(%s) = %v, %v", FormatUntil(d), back, err)
		}
	}
}
