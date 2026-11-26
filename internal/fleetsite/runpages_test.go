// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package fleetsite

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// FS-19: a bar label is the span name after its last slash, trimmed and cut
// to maxLabelCells monospace cells, a wide rune counting two.
func TestSpanLabel(t *testing.T) {
	for name, want := range map[string]string{
		"ateenv.v1alpha.EnvironmentService/CreateEnvironment": "CreateEnvironment",
		"substrate.ateapi.v1.Control/SuspendActor":            "SuspendActor",
		"ate_env_fleet.acquire":                               "ate_env_fleet.acquire",
		"trailing/":                                           "trailing/",
		"a/ x ":                                               "x",
		"svc/ ":                                               "svc/",
		" a/b ":                                               "b",
		"":                                                    "",
		strings.Repeat("é", maxLabelCells):                    strings.Repeat("é", maxLabelCells),
		strings.Repeat("é", maxLabelCells+1):                  strings.Repeat("é", maxLabelCells-1) + "…",
		strings.Repeat("界", maxLabelCells/2):                  strings.Repeat("界", maxLabelCells/2),
		strings.Repeat("界", maxLabelCells/2+1):                strings.Repeat("界", maxLabelCells/2-1) + "…",
		strings.Repeat("ᄀ", maxLabelCells/2+1):                strings.Repeat("ᄀ", maxLabelCells/2-1) + "…",
		strings.Repeat("⌚", maxLabelCells/2+1):                strings.Repeat("⌚", maxLabelCells/2-1) + "…",
		strings.Repeat("✅", maxLabelCells/2+1):                strings.Repeat("✅", maxLabelCells/2-1) + "…",
	} {
		if got := spanLabel(name); got != want {
			t.Errorf("spanLabel(%q) = %q, want %q", name, got, want)
		}
	}
}

// FS-19: a label goes inside its bar when it fits, else to the right when it
// fits there, else ending left of the bar. An error label starts with a
// cross.
func TestBarLabelPlacement(t *testing.T) {
	name := "x.Service/" + strings.Repeat("m", 10) // 10 runes: 68 units with padding
	label := strings.Repeat("m", 10)
	// A run of waterfallWidth ms puts each span at its own offset and width.
	for _, tc := range []struct {
		desc      string
		sp        WaterfallSpan
		x         float64
		end       bool
		wantLabel string
	}{
		{"wide bar", WaterfallSpan{Name: name, OffsetMS: 100, DurMS: 200}, 103, false, label},
		{"narrow bar", WaterfallSpan{Name: name, OffsetMS: 100, DurMS: 10}, 113, false, label},
		{"exact fit inside", WaterfallSpan{Name: name, OffsetMS: 0, DurMS: 68}, 3, false, label},
		{"one short of inside", WaterfallSpan{Name: name, OffsetMS: 0, DurMS: 67}, 70, false, label},
		{"exact fit on the right", WaterfallSpan{Name: name, OffsetMS: 880, DurMS: 12}, 895, false, label},
		{"one short of the right", WaterfallSpan{Name: name, OffsetMS: 881, DurMS: 12}, 878, true, label},
		{"sliver at the right edge", WaterfallSpan{Name: name, OffsetMS: 950, DurMS: 5}, 947, true, label},
		{"error", WaterfallSpan{Name: name, OffsetMS: 100, DurMS: 500, Error: true}, 103, false, "✕ " + label},
		// 13 cells, 86.6 units: the cross takes two cells but 3 bytes.
		{"error, exact fit by cells", WaterfallSpan{Name: name, OffsetMS: 0, DurMS: 87, Error: true}, 3, false, "✕ " + label},
		{"error, one short by cells", WaterfallSpan{Name: name, OffsetMS: 0, DurMS: 86, Error: true}, 89, false, "✕ " + label},
		{"no name", WaterfallSpan{OffsetMS: 100, DurMS: 500}, 0, false, ""},
	} {
		b := barOf(tc.sp, waterfallWidth, 0)
		if b.Label != tc.wantLabel || b.LabelEnd != tc.end || abs(b.LabelX-tc.x) > 1e-9 {
			t.Errorf("%s: label %q at %v end %v, want %q at %v end %v", tc.desc, b.Label, b.LabelX, b.LabelEnd, tc.wantLabel, tc.x, tc.end)
		}
	}
}

func abs(v float64) float64 { return max(v, -v) }

// FS-19: services get colors in the order they first appear, wrapping after
// serviceColors; a service with only error bars gets none.
func TestColorByService(t *testing.T) {
	var bars []waterfallBar
	for _, s := range []string{"fleet", "api", "!erronly", "fleet", "!fleet", "", "s3", "s4", "s5", "s6"} {
		errored := strings.HasPrefix(s, "!")
		bars = append(bars, waterfallBar{WaterfallSpan: WaterfallSpan{Service: strings.TrimPrefix(s, "!"), Error: errored}})
	}
	keys := colorByService(bars)
	var got []string
	for _, k := range keys {
		got = append(got, fmt.Sprintf("%s=%d", k.Name, k.Color))
	}
	if want := "fleet=0 api=1 =2 s3=3 s4=4 s5=5 s6=0"; strings.Join(got, " ") != want {
		t.Errorf("legend %q, want %q", strings.Join(got, " "), want)
	}
	for i, want := range map[int]int{0: 0, 1: 1, 3: 0, 5: 2, 6: 3, 7: 4, 8: 5, 9: 0} {
		if bars[i].Color != want {
			t.Errorf("bar %d color %d, want %d", i, bars[i].Color, want)
		}
	}
}

// FS-19: the Run page labels each bar, colors it by service with a legend,
// marks an error bar with a hatch, an outline and a cross, keeps the full
// name in the title, and escapes span names in labels.
func TestRunPageWaterfallLabels(t *testing.T) {
	ts := newRunsSite(t)
	dir := ts.srv.cfg.Runs.store.(DirRuns).Dir
	writeFile(t, filepath.Join(dir, "v1", "runs", "grpo-e3", "spans.jsonl"), strings.Join([]string{
		otlpLine(t, "ate-env-fleet", "",
			otlpSpanOf(runTrace, runSpanID, "", RunSpanName, 0, 1000, false),
			otlpSpanOf(runTrace, "1111111111111111", runSpanID, `<b>x</b>/<img src=x onerror=alert(1)>`, 10, 400, false)),
		otlpLine(t, "ate-env-api", "",
			otlpSpanOf(runTrace, "2222222222222222", "1111111111111111", "ateenv.v1alpha.EnvironmentService/CreateEnvironment", 20, 300, true),
			otlpSpanOf(runTrace, "4444444444444444", "1111111111111111", "ateenv.v1alpha.ProcessService/GetProcess", 995, 996, false)),
	}, "\n")+"\n")
	_, body := ts.get(t, "/runs/trainer/grpo-e3")
	if strings.Contains(body, "<img") || strings.Contains(body, "<b>x") {
		t.Error("a span name renders as markup")
	}
	for _, want := range []string{
		`<span class="swatch svc-0"></span>ate-env-fleet <span class="swatch svc-1"></span>ate-env-api <span class="swatch span-error"></span>error ✕`,
		`<svg viewBox="0 0 960 66" width="960" height="66"`,
		// The run span: full width, labeled inside.
		`<rect class="svc-0" x="0.0" y="0" width="960.0" height="14"></rect><text class="span-label" x="3.0" y="0" dy="10.5">ate_env_fleet.run</text>`,
		// The name after its last slash, escaped.
		`<text class="span-label" x="12.6" y="16" dy="10.5">&lt;img src=x onerror=alert(1)&gt;</text>`,
		// The error bar: hatched, outlined, a cross, the full name in the title.
		`<title>ateenv.v1alpha.EnvironmentService/CreateEnvironment (ate-env-api): 280.0 ms at +20.0 ms, error</title><rect class="span-error" fill="url(#span-err)" x="19.2" y="32" width="268.8" height="14"></rect><text class="span-label" x="22.2" y="32" dy="10.5">✕ CreateEnvironment</text>`,
		// A sliver at the right edge: its label ends left of it.
		`<rect class="svc-1" x="955.2" y="48" width="1.0" height="14"></rect><text class="span-label end" x="952.2" y="48" dy="10.5">GetProcess</text>`,
		`<pattern id="span-err"`,
		// The span table says "error" in words.
		`ateenv.v1alpha.EnvironmentService/CreateEnvironment (error)</td>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the Run page lacks %q", want)
		}
	}
}

// FS-19: a run without errors has no error key, and a span without a
// service shows as "no service".
func TestRunPageLegendWithoutErrors(t *testing.T) {
	ts := newRunsSite(t)
	dir := ts.srv.cfg.Runs.store.(DirRuns).Dir
	writeFile(t, filepath.Join(dir, "v1", "runs", "grpo-e4", "spans.jsonl"),
		otlpLine(t, "", "", otlpSpanOf(runTrace, runSpanID, "", RunSpanName, 0, 10, false))+"\n")
	_, body := ts.get(t, "/runs/trainer/grpo-e4")
	if !strings.Contains(body, `<p class="legend"><span class="swatch svc-0"></span>no service </p>`) {
		t.Error("the legend does not name a span without a service, or shows an error key")
	}
}

// FS-19: the longest label, of wide runes on an error span, stays inside the
// waterfall and off its bar wherever the bar is.
func TestBarLabelAlwaysFits(t *testing.T) {
	name := "x/" + strings.Repeat("界", maxLabelCells)
	for x := 0.0; x < waterfallWidth; x += 7 {
		for _, w := range []float64{0, 1, 50, 200, 300, 500} {
			b := barOf(WaterfallSpan{Name: name, OffsetMS: x, DurMS: w, Error: true}, waterfallWidth, 0)
			lw := labelWidth(b.Label)
			start, end := b.LabelX, b.LabelX+lw
			if b.LabelEnd {
				start, end = b.LabelX-lw, b.LabelX
			}
			inside := start >= b.X && end <= b.X+b.Width
			if b.Label == "" || start < 0 || end > waterfallWidth || !inside && end > b.X && start < b.X+b.Width {
				t.Fatalf("bar at %v width %v: label %q from %v to %v", b.X, b.Width, b.Label, start, end)
			}
		}
	}
}
