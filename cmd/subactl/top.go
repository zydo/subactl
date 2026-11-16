package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/zydo/subactl/api"
)

// topView is everything one frame of "subactl top" shows.
type topView struct {
	Host   api.Host
	Leases []api.Lease // whole trees, pre-order
	Events []api.Event // most recent last
	Now    time.Time
	Width  int
}

// bar draws usage against a limit, e.g. [######....].
func bar(used, limit int64, width int) string {
	if limit <= 0 {
		return "[" + strings.Repeat(" ", width) + "]"
	}
	n := int(float64(width) * float64(used) / float64(limit))
	n = max(0, min(n, width))
	fill := "#"
	if used >= limit*9/10 {
		fill = "!" // at or past the NEAR_LIMIT line
	}
	return "[" + strings.Repeat(fill, n) + strings.Repeat(".", width-n) + "]"
}

func short(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func clip(s string, width int) string {
	r := []rune(s)
	if width > 0 && len(r) > width {
		return string(r[:width-1]) + "…"
	}
	return s
}

// renderTop draws one frame as plain lines. It is a pure function of the
// view, so the layout can be tested without a terminal.
func renderTop(v topView) []string {
	h := v.Host
	pct := 0
	if h.EffectiveCapacity > 0 {
		pct = int(100 * h.Allocated / h.EffectiveCapacity)
	}
	head := fmt.Sprintf("subactl top   capacity %s   allocated %s (%d%%)   external %s   reserve %s   %s/%s",
		api.FormatBytes(h.EffectiveCapacity), api.FormatBytes(h.Allocated), pct,
		api.FormatBytes(h.External), api.FormatBytes(h.Reserve), h.Backend, h.MemBasis)
	if h.CapacityShrunk {
		head += "   CAPACITY SHRUNK: no new leases"
	}
	lines := []string{head, ""}
	lines = append(lines, fmt.Sprintf("%-26s %-9s %-8s %-12s %19s %5s %7s  %s", "LEASE", "KIND", "STATE", "MEMORY", "USED / LIMIT", "PIDS", "AGE", "ACTIVITY"))

	// Tree drawing needs to know, per depth, whether more siblings follow.
	last := map[api.LeaseID]bool{}
	byID := map[api.LeaseID]api.Lease{}
	for _, ls := range v.Leases {
		byID[ls.ID] = ls
		if n := len(ls.Children); n > 0 {
			last[ls.Children[n-1]] = true
		}
	}
	live := 0
	for _, ls := range v.Leases {
		if ls.State == api.StateReaped {
			continue // ended leases are in the events below
		}
		live++
		prefix := ""
		if ls.Parent != "" {
			// Walk up to draw the rails of the ancestors.
			var rails []string
			for p := byID[ls.Parent]; p.Parent != ""; p = byID[p.Parent] {
				if last[p.ID] {
					rails = append([]string{"   "}, rails...)
				} else {
					rails = append([]string{"│  "}, rails...)
				}
			}
			prefix = strings.Join(rails, "")
			if last[ls.ID] {
				prefix += "└─ "
			} else {
				prefix += "├─ "
			}
		}
		var used, pids int64
		if ls.Usage != nil {
			used, pids = ls.Usage.Memory, ls.Usage.Pids
		}
		activity := "-"
		if a := ls.Activity; a != nil {
			activity = a.State
			if a.Tool != "" && a.State == "working" {
				activity += fmt.Sprintf(" (%s #%d)", a.Tool, a.Seq)
			}
			if idle := v.Now.Sub(a.At); idle > 30*time.Second {
				activity += ", silent " + short(idle)
			}
		}
		lines = append(lines, fmt.Sprintf("%-26s %-9s %-8s %-12s %8s / %-8s %5d %7s  %s",
			clip(prefix+string(ls.ID), 26), clip(ls.Kind, 9), ls.State, bar(used, ls.Envelope.Memory, 10),
			api.FormatBytes(used), api.FormatBytes(ls.Envelope.Memory), pids, short(v.Now.Sub(ls.CreatedAt)), activity))
	}
	if live == 0 {
		lines = append(lines, "(no live leases)")
	}
	if len(v.Events) > 0 {
		lines = append(lines, "", "RECENT EVENTS")
		for _, e := range v.Events {
			detail := ""
			switch e.Type {
			case "TERMINATED":
				detail = fmt.Sprintf("%v %v", e.Data["code"], e.Data["name"])
			case "NEAR_LIMIT":
				if u, ok := e.Data["usage"].(float64); ok {
					detail = "at " + api.FormatBytes(int64(u))
				}
			case "CAPACITY_SHRUNK", "IDLE_DONE", "STALLED":
				detail = fmt.Sprint(e.Data["reason"], " ", e.Data["then"])
				detail = strings.TrimSpace(strings.ReplaceAll(detail, "<nil>", ""))
			}
			lines = append(lines, fmt.Sprintf("%s  %-16s %-12s %s", e.At.Local().Format("15:04:05"), e.Type, e.Lease, detail))
		}
	}
	for i := range lines {
		lines[i] = clip(lines[i], v.Width)
	}
	return lines
}

// top is an ordinary client of the API: it shows the lead trees the caller
// may see, refreshed every second, and knows nothing about any harness.
func (c *cli) top(ctx context.Context, args []string) (int, error) {
	fs := c.flags("top")
	once := fs.Bool("once", false, "print one frame and exit")
	interval := fs.Duration("interval", time.Second, "refresh interval")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	frame := func(events []api.Event, width int) ([]string, error) {
		host, err := c.api.Host(ctx)
		if err != nil {
			return nil, err
		}
		leases, err := c.api.Leases(ctx, "")
		if err != nil {
			return nil, err
		}
		return renderTop(topView{Host: host, Leases: leases, Events: events, Now: time.Now(), Width: width}), nil
	}
	out, isFile := c.out.(*os.File)
	if *once || !isFile || !term.IsTerminal(int(out.Fd())) {
		lines, err := frame(nil, 0)
		if err != nil {
			return 1, err
		}
		fmt.Fprintln(c.out, strings.Join(lines, "\n"))
		return 0, nil
	}

	// Full screen: the alternate screen buffer, raw input, q to quit.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if in, ok := stdin.(*os.File); ok && term.IsTerminal(int(in.Fd())) {
		if old, err := term.MakeRaw(int(in.Fd())); err == nil {
			defer term.Restore(int(in.Fd()), old)
			go func() {
				buf := make([]byte, 1)
				for {
					if _, err := in.Read(buf); err != nil || buf[0] == 'q' || buf[0] == 3 {
						cancel()
						return
					}
				}
			}()
		}
	}
	events := make(chan api.Event, 64)
	go func() {
		_ = c.api.Events(ctx, func(e api.Event) error {
			if e.Type != "STATE" && e.Type != "RESIZED" {
				select {
				case events <- e:
				default:
				}
			}
			return nil
		})
	}()
	io.WriteString(out, "\x1b[?1049h\x1b[?25l") // alternate screen, hide the cursor
	defer io.WriteString(out, "\x1b[?25h\x1b[?1049l")
	var recent []api.Event
	tick := time.NewTicker(*interval)
	defer tick.Stop()
	for {
		width, height, err := term.GetSize(int(out.Fd()))
		if err != nil {
			width, height = 120, 40
		}
		lines, err := frame(recent, width)
		if err != nil {
			return 1, err
		}
		if len(lines) > height {
			lines = lines[:height]
		}
		// Home, then each line cleared to its end: no flicker, no leftovers.
		io.WriteString(out, "\x1b[H"+strings.Join(lines, "\x1b[K\r\n")+"\x1b[K\x1b[J")
		select {
		case <-ctx.Done():
			return 0, nil
		case <-tick.C:
		case e := <-events:
			recent = append(recent, e)
			if len(recent) > 8 {
				recent = recent[len(recent)-8:]
			}
		}
	}
}
