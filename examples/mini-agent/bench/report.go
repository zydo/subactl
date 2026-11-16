package bench

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// GroupData is everything measured for one experiment group.
type GroupData struct {
	Name   string
	Runs   []Summary
	Series [][]Sample // one series per run, in run order
}

// LoadResults reads the groups found under dir: <dir>/<group>/summary.json
// and <dir>/<group>/run-N/series.jsonl.
func LoadResults(dir string) ([]GroupData, error) {
	var out []GroupData
	for _, name := range []string{"A", "B", "C"} {
		data, err := os.ReadFile(filepath.Join(dir, name, "summary.json"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		g := GroupData{Name: name}
		if err := json.Unmarshal(data, &g.Runs); err != nil {
			return nil, fmt.Errorf("%s/summary.json: %w", name, err)
		}
		for _, r := range g.Runs {
			series, err := readSeries(filepath.Join(dir, name, fmt.Sprintf("run-%d", r.Run), "series.jsonl"))
			if err != nil {
				return nil, err
			}
			g.Series = append(g.Series, series)
		}
		out = append(out, g)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no group results under %s", dir)
	}
	return out, nil
}

func readSeries(path string) ([]Sample, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Sample
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var s Sample
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, s)
	}
	return out, sc.Err()
}

// Verdict is the outcome of one hypothesis.
type Verdict struct {
	ID       string
	Claim    string
	Holds    *bool // nil when the data to decide is missing
	Evidence string
}

func verdict(id, claim string, holds bool, evidence string) Verdict {
	return Verdict{ID: id, Claim: claim, Holds: &holds, Evidence: evidence}
}

func undecided(id, claim, why string) Verdict {
	return Verdict{ID: id, Claim: claim, Evidence: why}
}

func (v Verdict) String() string {
	switch {
	case v.Holds == nil:
		return "undecided"
	case *v.Holds:
		return "holds"
	}
	return "refuted"
}

func find(groups []GroupData, name string) *GroupData {
	for i := range groups {
		if groups[i].Name == name {
			return &groups[i]
		}
	}
	return nil
}

func mean(g *GroupData, f func(Summary) float64) float64 {
	var sum float64
	for _, r := range g.Runs {
		sum += f(r)
	}
	return sum / float64(len(g.Runs))
}

// each lists a per-run value for a group, e.g. "A: 30, 18, 6".
func each(g *GroupData, f func(Summary) string) string {
	vals := make([]string, len(g.Runs))
	for i, r := range g.Runs {
		vals[i] = f(r)
	}
	return g.Name + ": " + strings.Join(vals, ", ")
}

// Judge decides the design's four hypotheses from the data. A hypothesis
// that the data does not support is reported as refuted, not softened.
func Judge(groups []GroupData) []Verdict {
	a, b, c := find(groups, "A"), find(groups, "B"), find(groups, "C")
	const (
		h1 = "After the usage-limit kill, group A leaves processes behind in every run; groups B and C never do."
		h2 = "Group A hits the chamber's own memory cap, or melts down, in at least one run; groups B and C never do."
		h3 = "Group C's mean utilisation is at least 15 points above group B's, and it loses no more work."
		h4 = "Under external interference, B and C use the safety net at most once per run, and planners are warned before it acts."
	)
	if a == nil || b == nil || c == nil {
		why := "needs the results of groups A, B and C"
		return []Verdict{undecided("H1", h1, why), undecided("H2", h2, why), undecided("H3", h3, why), undecided("H4", h4, why)}
	}
	var out []Verdict

	orphans := func(r Summary) string { return fmt.Sprint(r.OrphansAfter) }
	ok := true
	for _, r := range a.Runs {
		ok = ok && r.OrphansAfter > 0
	}
	for _, g := range []*GroupData{b, c} {
		for _, r := range g.Runs {
			ok = ok && r.OrphansAfter == 0
		}
	}
	out = append(out, verdict("H1", h1, ok, "processes left after 60s, per run. "+each(a, orphans)+". "+each(b, orphans)+". "+each(c, orphans)+"."))

	hits := func(r Summary) string {
		if r.Meltdown {
			return "meltdown"
		}
		return fmt.Sprint(r.HostOOM)
	}
	aHit := false
	for _, r := range a.Runs {
		aHit = aHit || r.HostOOM > 0 || r.Meltdown
	}
	ok = aHit
	for _, g := range []*GroupData{b, c} {
		for _, r := range g.Runs {
			ok = ok && r.HostOOM == 0 && !r.Meltdown
		}
	}
	out = append(out, verdict("H2", h2, ok, "times the chamber's own cap was hit, per run. "+each(a, hits)+". "+each(b, hits)+". "+each(c, hits)+"."))

	util := func(r Summary) float64 { return r.UtilMean }
	lost := func(r Summary) float64 { return float64(r.WorkLost) }
	bu, cu, bl, cl := mean(b, util), mean(c, util), mean(b, lost), mean(c, lost)
	out = append(out, verdict("H3", h3, cu-bu >= 0.15 && cl <= bl, fmt.Sprintf(
		"mean utilisation B %.1f%%, C %.1f%% (difference %+.1f points, 15 needed); mean work lost B %.1f, C %.1f subtasks.",
		100*bu, 100*cu, 100*(cu-bu), bl, cl)))

	net := func(r Summary) string {
		s := fmt.Sprint(r.SafetyNet)
		if r.WarnedBeforeNet != nil && !*r.WarnedBeforeNet {
			s += " (not warned first)"
		}
		return s
	}
	ok = true
	for _, g := range []*GroupData{b, c} {
		for _, r := range g.Runs {
			ok = ok && r.SafetyNet <= 1 && (r.WarnedBeforeNet == nil || *r.WarnedBeforeNet)
		}
	}
	out = append(out, verdict("H4", h4, ok, "safety-net terminations per run. "+each(b, net)+". "+each(c, net)+"."))
	return out
}

// WriteSeriesParquet saves the raw one-second series of every run.
func WriteSeriesParquet(path string, groups []GroupData) error {
	cols := []Column{
		{Name: "group", Strings: []string{}}, {Name: "run", Int32s: []int32{}}, {Name: "t", Doubles: []float64{}},
		{Name: "ws", Int64s: []int64{}}, {Name: "agents_ws", Int64s: []int64{}}, {Name: "pids", Int64s: []int64{}},
		{Name: "oom_kills", Int64s: []int64{}}, {Name: "cap_hits", Int64s: []int64{}},
	}
	for _, g := range groups {
		for i, series := range g.Series {
			for _, s := range series {
				cols[0].Strings = append(cols[0].Strings, g.Name)
				cols[1].Int32s = append(cols[1].Int32s, int32(g.Runs[i].Run))
				cols[2].Doubles = append(cols[2].Doubles, s.T)
				cols[3].Int64s = append(cols[3].Int64s, s.WS)
				cols[4].Int64s = append(cols[4].Int64s, s.AgentsWS)
				cols[5].Int64s = append(cols[5].Int64s, s.Pids)
				cols[6].Int64s = append(cols[6].Int64s, s.OOMKills)
				cols[7].Int64s = append(cols[7].Int64s, s.CapHits)
			}
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return WriteParquet(f, cols)
}

// Colours per group, readable on white and distinct without relying on
// red against green.
var groupColour = map[string]string{"A": "#c0392b", "B": "#2c6fbb", "C": "#1b8a5a"}

var groupLabel = map[string]string{
	"A": "A: without subactl",
	"B": "B: static envelopes",
	"C": "C: suggested envelopes that grow",
}

// MemoryChart draws the chamber's working set over time, one line per
// run, one colour per group, with the chamber's cap as a dashed line.
func MemoryChart(groups []GroupData) string {
	const w, h, left, right, top, bottom = 900.0, 420.0, 60.0, 20.0, 40.0, 50.0
	var maxT, capGiB float64
	for _, g := range groups {
		for i, series := range g.Series {
			capGiB = max(capGiB, float64(g.Runs[i].ChamberBytes)/gib)
			for _, s := range series {
				maxT = max(maxT, s.T)
			}
		}
	}
	if maxT == 0 || capGiB == 0 {
		return ""
	}
	maxY := capGiB * 1.08
	x := func(t float64) float64 { return left + (w-left-right)*t/maxT }
	y := func(v float64) float64 { return top + (h-top-bottom)*(1-v/maxY) }
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %.0f %.0f" font-family="sans-serif" font-size="12">`+"\n", w, h)
	fmt.Fprintf(&b, `<rect width="%.0f" height="%.0f" fill="white"/>`+"\n", w, h)
	fmt.Fprintf(&b, `<text x="%.0f" y="20" font-size="14" font-weight="bold">Working set of the blast chamber over time</text>`+"\n", left)
	// Axes and grid.
	for v := 0.0; v <= capGiB; v += max(1, float64(int(capGiB/6))) {
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="#e5e5e5"/><text x="%.1f" y="%.1f" text-anchor="end">%.0f</text>`+"\n",
			left, y(v), w-right, y(v), left-6, y(v)+4, v)
	}
	for m := 0.0; m*60 <= maxT; m += max(1, float64(int(maxT/60/8))) {
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="#e5e5e5"/><text x="%.1f" y="%.1f" text-anchor="middle">%.0f</text>`+"\n",
			x(m*60), top, x(m*60), h-bottom, x(m*60), h-bottom+16, m)
	}
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="middle">minutes since the lead started</text>`+"\n", left+(w-left-right)/2, h-12)
	fmt.Fprintf(&b, `<text transform="translate(16 %.1f) rotate(-90)" text-anchor="middle">GiB (working set)</text>`+"\n", top+(h-top-bottom)/2)
	fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="#444" stroke-dasharray="6 4"/><text x="%.1f" y="%.1f" text-anchor="end" fill="#444">chamber cap %.0f GiB</text>`+"\n",
		left, y(capGiB), w-right, y(capGiB), w-right-4, y(capGiB)-5, capGiB)
	for _, g := range groups {
		for _, series := range g.Series {
			var pts []string
			// Every fifth second is plenty for a chart this size.
			for i := 0; i < len(series); i += 5 {
				pts = append(pts, fmt.Sprintf("%.1f,%.1f", x(series[i].T), y(float64(series[i].WS)/gib)))
			}
			fmt.Fprintf(&b, `<polyline fill="none" stroke="%s" stroke-width="1.2" stroke-opacity="0.75" points="%s"/>`+"\n", groupColour[g.Name], strings.Join(pts, " "))
		}
	}
	for i, g := range groups {
		lx := left + 10 + float64(i)*260
		fmt.Fprintf(&b, `<line x1="%.1f" y1="32" x2="%.1f" y2="32" stroke="%s" stroke-width="3"/><text x="%.1f" y="36">%s</text>`+"\n", lx, lx+24, groupColour[g.Name], lx+30, groupLabel[g.Name])
	}
	b.WriteString("</svg>\n")
	return b.String()
}

type metric struct {
	title string
	value func(Summary) float64
	unit  string
}

var chartMetrics = []metric{
	{"Processes left after 60s", func(s Summary) float64 { return float64(s.OrphansAfter) }, ""},
	{"Hits of the chamber cap", func(s Summary) float64 { return float64(s.HostOOM) }, ""},
	{"Packages completed", func(s Summary) float64 { return 100 * s.Completion }, "%"},
	{"Subtasks lost and redone", func(s Summary) float64 { return float64(s.WorkLost) }, ""},
	{"Mean utilisation", func(s Summary) float64 { return 100 * s.UtilMean }, "%"},
}

// MetricsChart draws one small panel per metric: a bar per group for the
// mean, with a tick for each run so the spread is visible.
func MetricsChart(groups []GroupData) string {
	const panelW, h, top, bottom = 180.0, 300.0, 60.0, 40.0
	w := panelW * float64(len(chartMetrics))
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %.0f %.0f" font-family="sans-serif" font-size="12">`+"\n", w, h)
	fmt.Fprintf(&b, `<rect width="%.0f" height="%.0f" fill="white"/>`+"\n", w, h)
	for i, m := range chartMetrics {
		x0 := panelW * float64(i)
		var top1 float64
		for _, g := range groups {
			for _, r := range g.Runs {
				top1 = max(top1, m.value(r))
			}
		}
		if top1 == 0 {
			top1 = 1
		}
		y := func(v float64) float64 { return top + (h-top-bottom)*(1-v/top1) }
		fmt.Fprintf(&b, `<text x="%.1f" y="22" text-anchor="middle" font-weight="bold">%s</text>`+"\n", x0+panelW/2, m.title)
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="#999"/>`+"\n", x0+20, h-bottom, x0+panelW-10, h-bottom)
		barW := (panelW - 50) / float64(len(groups))
		for j := range groups {
			g := &groups[j]
			bx := x0 + 25 + float64(j)*barW
			avg := mean(g, m.value)
			fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="%s" fill-opacity="0.8"/>`+"\n",
				bx+4, y(avg), barW-8, h-bottom-y(avg), groupColour[g.Name])
			for _, r := range g.Runs {
				fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="#222" stroke-width="1"/>`+"\n",
					bx+barW/2-6, y(m.value(r)), bx+barW/2+6, y(m.value(r)))
			}
			fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="middle">%s</text>`+"\n", bx+barW/2, h-bottom+16, g.Name)
			fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="middle" fill="#222">%s%s</text>`+"\n", bx+barW/2, y(avg)-5, trim(avg), m.unit)
		}
	}
	b.WriteString("</svg>\n")
	return b.String()
}

func trim(v float64) string {
	s := fmt.Sprintf("%.1f", v)
	return strings.TrimSuffix(s, ".0")
}

// Markdown renders the report: the verdicts first, then the numbers they
// rest on.
func Markdown(groups []GroupData, verdicts []Verdict) string {
	var b strings.Builder
	b.WriteString("# Controlled experiment: results\n\n")
	b.WriteString("Generated by `bench report` from the measurements in this directory. Nothing here is written by hand.\n\n")
	b.WriteString("## Hypotheses\n\n| | hypothesis | verdict | evidence |\n| --- | --- | --- | --- |\n")
	for _, v := range verdicts {
		fmt.Fprintf(&b, "| %s | %s | **%s** | %s |\n", v.ID, v.Claim, v, v.Evidence)
	}
	b.WriteString("\n![Working set over time](memory.svg)\n\n![Metrics by group](metrics.svg)\n\n")
	b.WriteString("## Means by group\n\n| group | runs | peak working set | cap hits | processes left | memory they held | completion | work lost | utilisation | safety net | makespan |\n| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for i := range groups {
		g := &groups[i]
		fmt.Fprintf(&b, "| %s | %d | %.1f GiB | %.1f | %.1f | %.2f GiB | %.0f%% | %.1f | %.1f%% | %.1f | %.1f min |\n",
			groupLabel[g.Name], len(g.Runs),
			mean(g, func(s Summary) float64 { return s.HostPeakWS }),
			mean(g, func(s Summary) float64 { return float64(s.HostOOM) }),
			mean(g, func(s Summary) float64 { return float64(s.OrphansAfter) }),
			mean(g, func(s Summary) float64 { return s.LeakedWSAfter }),
			100*mean(g, func(s Summary) float64 { return s.Completion }),
			mean(g, func(s Summary) float64 { return float64(s.WorkLost) }),
			100*mean(g, func(s Summary) float64 { return s.UtilMean }),
			mean(g, func(s Summary) float64 { return float64(s.SafetyNet) }),
			mean(g, func(s Summary) float64 { return s.Makespan }))
	}
	b.WriteString("\n## Every run\n\n| group | run | peak working set | cap hits | OOM kills | meltdown | processes left | memory they held | completion | work lost | utilisation | safety net | termination codes |\n| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, g := range groups {
		for _, r := range g.Runs {
			var codes []string
			for code, n := range r.Codes {
				codes = append(codes, fmt.Sprintf("%s×%d", code, n))
			}
			sort.Strings(codes)
			fmt.Fprintf(&b, "| %s | %d | %.1f GiB | %d | %d | %v | %d | %.2f GiB | %.0f%% (of %d) | %d | %.1f%% | %d | %s |\n",
				g.Name, r.Run, r.HostPeakWS, r.HostOOM, r.OOMKills, r.Meltdown, r.OrphansAfter, r.LeakedWSAfter,
				100*r.Completion, r.Packages, r.WorkLost, 100*r.UtilMean, r.SafetyNet, strings.Join(codes, " "))
		}
	}
	b.WriteString("\nThe one-second series of every run is in `series.parquet` (columns: group, run, t, ws, agents_ws, pids, oom_kills, cap_hits; memory is working set in bytes).\n")
	return b.String()
}

// WriteReport writes report.md, the two charts and the Parquet series
// into dir.
func WriteReport(dir string, groups []GroupData) ([]Verdict, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	verdicts := Judge(groups)
	files := map[string]string{
		"report.md":   Markdown(groups, verdicts),
		"memory.svg":  MemoryChart(groups),
		"metrics.svg": MetricsChart(groups),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			return nil, err
		}
	}
	return verdicts, WriteSeriesParquet(filepath.Join(dir, "series.parquet"), groups)
}
