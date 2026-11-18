#!/usr/bin/env python3
"""Generates the README diagram, how-subactl-works.svg and its Chinese
version, from one layout, so the two never drift apart.

    python3 docs/assets/diagram.py
"""
import os

T = {
    "en": dict(
        file="how-subactl-works.svg",
        title="How Subagent Controller Works",
        desc="A lead's lease holds its workers' leases, each with a hard memory limit. "
             "A worker over its limit is ended with everything it started; subactld "
             "never oversells memory, ends whole groups and keeps the machine up.",
        machine="ONE MACHINE",
        lead="lead", worker="worker", fixer="fixer",
        done="done", over="over limit", over2="all of it ended", running="running",
        other="other programs", reserve="reserve",
        spawn="spawn", events="events",
        daemon="the daemon",
        p1="Never oversells", p2="Ends whole groups", p3="Keeps the machine up",
        codes="One code per lease",
        c200="done", c413="over its limit", c503="machine short", c502="parent ended",
    ),
    "zh": dict(
        file="how-subactl-works.zh-CN.svg",
        title="Subagent Controller 如何工作",
        desc="lead 的 lease 里装着各个 worker 的 lease，每个都有内存硬限。超限的 worker "
             "连同它启动的一切被终止；subactld 从不超卖内存，整组终止，保住整台机器。",
        machine="一台机器",
        lead="lead", worker="worker", fixer="fixer",
        done="完成", over="超出上限", over2="整组终止", running="运行中",
        other="其他程序", reserve="预留线",
        spawn="spawn", events="事件",
        daemon="daemon",
        p1="绝不超卖", p2="整组终止", p3="保住机器",
        codes="每个 lease 一个终止码",
        c200="完成", c413="超出自身上限", c503="机器内存不足", c502="父节点结束",
    ),
}

FONT = ("-apple-system, BlinkMacSystemFont, 'Inter', 'Segoe UI', 'PingFang SC', "
        "'Microsoft YaHei', 'Noto Sans CJK SC', Helvetica, Arial, sans-serif")

INK, MUTED, FAINT, LINE = "#0f172a", "#64748b", "#94a3b8", "#e2e8f0"


def chip(x, y, w, text, dot, ended=False):
    stroke = 'stroke="#fda4af" stroke-dasharray="4 3"' if ended else f'stroke="{LINE}"'
    color = "#be123c" if ended else "#334155"
    return (f'<rect x="{x}" y="{y}" width="{w}" height="30" rx="15" fill="#ffffff" {stroke}/>'
            f'<circle cx="{x + 16}" cy="{y + 15}" r="4" fill="{dot}"/>'
            f'<text x="{x + 28}" y="{y + 20}" font-size="14" fill="{color}">{text}</text>')


def pill(x, y, text, fill, color, w=None, size=13):
    w = w or (len(text) * 8 + 20)
    return (f'<rect x="{x}" y="{y}" width="{w}" height="24" rx="12" fill="{fill}"/>'
            f'<text x="{x + w / 2}" y="{y + 17}" font-size="{size}" font-weight="600" '
            f'fill="{color}" text-anchor="middle">{text}</text>')


def check(cx, cy):
    return (f'<circle cx="{cx}" cy="{cy}" r="13" fill="#eef2ff"/>'
            f'<path d="M{cx - 5.5},{cy + 0.5} l3.8,3.8 l7.2,-7.6" fill="none" stroke="#4f46e5" '
            f'stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"/>')


def worker(x, y, s, size, state):
    w, h = 164, 232
    if state == "over":
        card = f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="14" fill="#fff1f2" stroke="#fecdd3"/>'
    else:
        card = f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="14" fill="#ffffff" stroke="#e0e7ff" filter="url(#soft)"/>'
    out = [card,
           f'<text x="{x + 16}" y="{y + 30}" font-size="15" font-weight="700" fill="{INK}">{s["worker"]}</text>',
           pill(x + w - 70, y + 13, size, "#eef2ff" if state != "over" else "#ffe4e6",
                "#4338ca" if state != "over" else "#be123c", w=56)]
    if state == "done":
        out += [chip(x + 14, y + 50, 136, "node", "#10b981"),
                chip(x + 14, y + 88, 136, "tsc", "#10b981"),
                chip(x + 14, y + 126, 136, "dev server", "#10b981")]
        out.append(f'<rect x="{x + 14}" y="{y + 180}" width="136" height="38" rx="12" fill="#ecfdf5"/>'
                   f'<text x="{x + 82}" y="{y + 205}" font-size="15" font-weight="700" fill="#047857" '
                   f'text-anchor="middle">200 · {s["done"]}</text>')
    elif state == "over":
        out += [chip(x + 14, y + 50, 136, "node", "#fb7185", True),
                chip(x + 14, y + 88, 136, "chrome ×11", "#fb7185", True),
                chip(x + 14, y + 126, 136, "dev server", "#fb7185", True)]
        out.append(f'<rect x="{x + 14}" y="{y + 170}" width="136" height="48" rx="12" fill="#ffe4e6"/>'
                   f'<text x="{x + 82}" y="{y + 191}" font-size="15" font-weight="700" fill="#be123c" '
                   f'text-anchor="middle">413 · {s["over"]}</text>'
                   f'<text x="{x + 82}" y="{y + 209}" font-size="12.5" fill="#be123c" '
                   f'text-anchor="middle">{s["over2"]}</text>')
    else:
        out.append(chip(x + 14, y + 50, 136, "node", "#6366f1"))
        out.append(f'<rect x="{x + 14}" y="{y + 90}" width="136" height="78" rx="12" fill="#eef2ff" '
                   f'stroke="#a5b4fc" stroke-dasharray="5 4"/>'
                   f'<text x="{x + 28}" y="{y + 114}" font-size="14" font-weight="700" fill="#3730a3">{s["fixer"]}</text>'
                   + pill(x + 92, y + 98, "1 GiB", "#ffffff", "#4338ca", w=50, size=12)
                   + chip(x + 24, y + 128, 116, "jest", "#6366f1"))
        out.append(f'<rect x="{x + 14}" y="{y + 180}" width="136" height="38" rx="12" fill="#eef2ff"/>'
                   f'<circle cx="{x + 46}" cy="{y + 199}" r="4.5" fill="#6366f1"/>'
                   f'<text x="{x + 88}" y="{y + 205}" font-size="15" font-weight="700" fill="#4338ca" '
                   f'text-anchor="middle">{s["running"]}</text>')
    return "".join(out)


def svg(s):
    o = [f'<svg xmlns="http://www.w3.org/2000/svg" width="960" height="560" viewBox="0 0 960 560" font-family="{FONT}">',
         f'<title>{s["title"]}</title><desc>{s["desc"]}</desc>',
         '<defs>'
         '<linearGradient id="bg" x1="0" y1="0" x2="1" y2="1"><stop offset="0" stop-color="#fafbff"/><stop offset="1" stop-color="#f1f4fb"/></linearGradient>'
         '<linearGradient id="brand" x1="0" y1="0" x2="1" y2="1"><stop offset="0" stop-color="#6366f1"/><stop offset="1" stop-color="#a855f7"/></linearGradient>'
         '<pattern id="hatch" width="8" height="8" patternUnits="userSpaceOnUse" patternTransform="rotate(45)"><rect width="8" height="8" fill="#f8fafc"/><line x1="0" y1="0" x2="0" y2="8" stroke="#e2e8f0" stroke-width="3"/></pattern>'
         '<filter id="soft" x="-10%" y="-10%" width="120%" height="130%"><feDropShadow dx="0" dy="2" stdDeviation="4" flood-color="#1e293b" flood-opacity="0.07"/></filter>'
         '<marker id="tip" viewBox="0 0 10 10" refX="8" refY="5" markerWidth="7" markerHeight="7" orient="auto"><path d="M1,1 L9,5 L1,9" fill="none" stroke="#818cf8" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"/></marker>'
         '</defs>',
         '<rect x="0.5" y="0.5" width="959" height="559" rx="22" fill="url(#bg)" stroke="#e2e8f0"/>',
         f'<text x="480" y="58" font-size="26" font-weight="800" fill="{INK}" letter-spacing="-0.3" text-anchor="middle">{s["title"]}</text>',
         # the machine
         '<rect x="40" y="84" width="572" height="440" rx="18" fill="#ffffff" stroke="#e2e8f0" filter="url(#soft)"/>',
         f'<text x="62" y="114" font-size="12" font-weight="700" fill="{FAINT}" letter-spacing="1.6">{s["machine"]}</text>',
         # the lead's lease
         '<rect x="58" y="128" width="536" height="296" rx="16" fill="#f5f7ff" stroke="#c7d2fe"/>',
         pill(76, 144, s["lead"], "url(#brand)", "#ffffff", w=60, size=14),
         f'<text x="148" y="162" font-size="16" font-weight="700" fill="#312e81">9 GiB</text>',
         worker(76, 180, s, "3 GiB", "done"),
         worker(250, 180, s, "3 GiB", "over"),
         worker(424, 180, s, "2.5 GiB", "running"),
         # outside the agents
         f'<rect x="58" y="440" width="262" height="66" rx="14" fill="#ffffff" stroke="#cbd5e1" stroke-dasharray="6 5"/>',
         f'<text x="189" y="479" font-size="15" font-weight="600" fill="{MUTED}" text-anchor="middle">{s["other"]}</text>',
         '<rect x="332" y="440" width="262" height="66" rx="14" fill="url(#hatch)" stroke="#e2e8f0"/>',
         f'<text x="463" y="479" font-size="15" font-weight="600" fill="{MUTED}" text-anchor="middle">{s["reserve"]}</text>',
         # requests and events
         '<path d="M598,206 C618,206 620,196 640,196" fill="none" stroke="#818cf8" stroke-width="2" marker-end="url(#tip)"/>',
         f'<text x="620" y="186" font-size="12" font-weight="600" fill="#6366f1" text-anchor="middle">{s["spawn"]}</text>',
         '<path d="M640,268 C620,268 618,278 598,278" fill="none" stroke="#818cf8" stroke-width="2" marker-end="url(#tip)"/>',
         f'<text x="620" y="300" font-size="12" font-weight="600" fill="#6366f1" text-anchor="middle">{s["events"]}</text>',
         # the daemon
         '<rect x="644" y="84" width="276" height="240" rx="18" fill="#ffffff" stroke="#e2e8f0" filter="url(#soft)"/>',
         '<rect x="664" y="104" width="40" height="40" rx="11" fill="url(#brand)"/>',
         '<path d="M684,113 l10,4 v7 c0,6 -4.5,10 -10,12 c-5.5,-2 -10,-6 -10,-12 v-7 z" fill="none" stroke="#ffffff" stroke-width="2.2" stroke-linejoin="round"/>',
         f'<text x="716" y="122" font-size="19" font-weight="800" fill="{INK}">subactld</text>',
         f'<text x="716" y="141" font-size="13" fill="{MUTED}">{s["daemon"]}</text>',
         check(678, 184), f'<text x="702" y="190" font-size="16" font-weight="600" fill="{INK}">{s["p1"]}</text>',
         check(678, 230), f'<text x="702" y="236" font-size="16" font-weight="600" fill="{INK}">{s["p2"]}</text>',
         check(678, 276), f'<text x="702" y="282" font-size="16" font-weight="600" fill="{INK}">{s["p3"]}</text>',
         # the codes
         '<rect x="644" y="340" width="276" height="184" rx="18" fill="#ffffff" stroke="#e2e8f0" filter="url(#soft)"/>',
         f'<text x="664" y="372" font-size="15" font-weight="700" fill="{INK}">{s["codes"]}</text>',
    ]
    rows = [("200", "#ecfdf5", "#047857", s["c200"]), ("413", "#ffe4e6", "#be123c", s["c413"]),
            ("503", "#fef3c7", "#b45309", s["c503"]), ("502", "#f1f5f9", "#475569", s["c502"])]
    for i, (code, fill, color, text) in enumerate(rows):
        y = 390 + i * 32
        o.append(pill(664, y, code, fill, color, w=50, size=13.5))
        o.append(f'<text x="726" y="{y + 17}" font-size="15" fill="#334155">{text}</text>')
    o.append("</svg>")
    return "\n".join(o) + "\n"


here = os.path.dirname(os.path.abspath(__file__))
for s in T.values():
    with open(os.path.join(here, s["file"]), "w", encoding="utf-8") as f:
        f.write(svg(s))
    print("wrote", s["file"])
