// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package logfmt

import (
	"strings"
	"testing"
)

func lines(s string) []string { return strings.Split(strings.TrimSpace(s), "\n") }

func TestDetect(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  string
		ok    bool
	}{
		{
			name: "json",
			lines: lines(`
{"level":"info","msg":"listening","addr":":8080"}
{"level":"warn","msg":"slow query","ms":812}
{"level":"error","msg":"upstream refused"}`),
			want: "json", ok: true,
		},
		{
			// Every GELF line is also valid JSON and carries a level, so this is
			// the case that decides whether the order of the sniffers is right.
			name: "gelf beats json",
			lines: lines(`
{"version":"1.1","host":"node-a","short_message":"listening","level":6}
{"version":"1.1","host":"node-a","short_message":"slow query","level":4}
{"version":"1.1","host":"node-a","short_message":"upstream refused","level":3}`),
			want: "gelf", ok: true,
		},
		{
			name: "logfmt",
			lines: lines(`
level=info msg="listening" addr=:8080
level=warn msg="slow query" ms=812
level=error msg="upstream refused"`),
			want: "logfmt", ok: true,
		},
		{
			name: "classic",
			lines: lines(`
2026-09-22 10:00:00 INFO  listening on :8080
2026-09-22 10:00:01 WARN  slow query (812ms)
2026-09-22 10:00:02 ERROR upstream refused`),
			want: "classic", ok: true,
		},
		{
			// An access log has no level and no structure. Saying "classic, but
			// I could not tell" is the honest answer; claiming a format would
			// make --min-level silently drop everything.
			name: "no verdict on an access log",
			lines: lines(`
10.0.0.5 - - [22/Sep/2026:10:00:00 +0000] "GET /health HTTP/1.1" 200 2
10.0.0.5 - - [22/Sep/2026:10:00:01 +0000] "GET /index.html HTTP/1.1" 200 5120
10.0.0.6 - - [22/Sep/2026:10:00:02 +0000] "POST /api/v1/orders HTTP/1.1" 201 88`),
			want: "classic", ok: false,
		},
		{
			// Two lines is one unusual line away from a wrong verdict.
			name:  "too few lines",
			lines: lines("{\"level\":\"info\",\"msg\":\"a\"}\n{\"level\":\"info\",\"msg\":\"b\"}"),
			want:  "classic", ok: false,
		},
		{
			name:  "nothing at all",
			lines: nil,
			want:  "classic", ok: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := Detect(c.lines)
			if got.Name() != c.want || ok != c.ok {
				t.Errorf("Detect = %s/%v, want %s/%v", got.Name(), ok, c.want, c.ok)
			}
		})
	}
}

// A structured logger that also prints stack traces must still be recognised:
// the continuation lines are not JSON, and demanding every line would hand the
// stream to the raw format precisely when the operator needs the level filter.
func TestDetectToleratesUnparsableLines(t *testing.T) {
	l := lines(`
{"level":"error","msg":"panic"}
goroutine 1 [running]:
main.main()
{"level":"info","msg":"restarted"}
{"level":"info","msg":"listening"}
{"level":"info","msg":"ready"}`)
	if got, ok := Detect(l); got.Name() != "json" || !ok {
		t.Errorf("Detect = %s/%v, want json/true", got.Name(), ok)
	}
}

// A plain log with one JSON line in it is a plain log. The threshold is what
// keeps a single lucky line from carrying the vote.
func TestDetectIgnoresAStrayStructuredLine(t *testing.T) {
	l := lines(`
2026-09-22 10:00:00 INFO  listening
2026-09-22 10:00:01 INFO  ready
{"level":"info","msg":"a config dump the app printed once"}
2026-09-22 10:00:02 WARN  slow`)
	if got, ok := Detect(l); got.Name() != "classic" || !ok {
		t.Errorf("Detect = %s/%v, want classic/true", got.Name(), ok)
	}
}

// `docker logs --timestamps` prepends an RFC3339 stamp to every line. The
// detector has to see past it, or -t would silently defeat detection.
func TestDetectSeesPastADockerTimestampPrefix(t *testing.T) {
	l := lines(`
2026-09-22T10:00:00.1Z {"level":"info","msg":"listening"}
2026-09-22T10:00:01.2Z {"level":"warn","msg":"slow"}
2026-09-22T10:00:02.3Z {"level":"error","msg":"refused"}`)
	if got, ok := Detect(l); got.Name() != "json" || !ok {
		t.Errorf("Detect = %s/%v, want json/true", got.Name(), ok)
	}
}

// A query string is key=value and is not a log record. Requiring a msg or level
// key is what keeps an access log out of the logfmt bucket.
func TestDetectDoesNotCallAQueryStringLogfmt(t *testing.T) {
	l := lines(`
GET /search?q=shoes&page=2 200
GET /search?q=hats&page=1 200
GET /cart?id=99 404`)
	if got, ok := Detect(l); ok {
		t.Errorf("Detect = %s/%v, want no verdict", got.Name(), ok)
	}
}

// The sample is the tail, not the head: a container's first lines are its
// framework's startup banner, which routinely logs nothing like the app.
func TestDetectSamplesTheMostRecentLines(t *testing.T) {
	var l []string
	for i := 0; i < DetectSample+50; i++ {
		l = append(l, "starting up, this is the banner nobody logs like")
	}
	for i := 0; i < DetectSample; i++ {
		l = append(l, `{"level":"info","msg":"steady state"}`)
	}
	if got, ok := Detect(l); got.Name() != "json" || !ok {
		t.Errorf("Detect = %s/%v, want json/true", got.Name(), ok)
	}
}

// Blank lines are structure, not evidence. Counting them as failures would
// punish exactly the loggers that separate their records.
func TestDetectSkipsBlankLines(t *testing.T) {
	l := []string{`{"level":"info","msg":"a"}`, "", `{"level":"info","msg":"b"}`, "   ", `{"level":"info","msg":"c"}`}
	if got, ok := Detect(l); got.Name() != "json" || !ok {
		t.Errorf("Detect = %s/%v, want json/true", got.Name(), ok)
	}
}
