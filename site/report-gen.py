#!/usr/bin/env python3
"""Generate site/report.gif — the swarmexec cluster security report.

Sibling of demo-gen.py and portforward-gen.py: same scripted-terminal
mechanism, palette, theme and save pipeline, and like them it needs no live
cluster — every line is scripted.

What it has that the siblings do not is `clear()`. The story here is a FILE,
and the interesting part of that file does not fit on one 16-row terminal at a
legible font size. Rather than shrink the text or truncate the report into
something that no longer makes its point, the scene turns three pages: the
command, the summary, and the cluster findings.

Every string mirrors real output, because a demo that shows output the tool
does not produce is a bug report waiting to happen:
  * `wrote <file> — <n> finding(s), <n> above low` is reportSummaryLine()
    (client/internal/cli/ui_securityreport.go), printed to stderr by
    runSecurityReport (securitycmd.go).
  * the report's headings, the scanned-counts line, the summary table and the
    "Not covered" wording are Report.Markdown()
    (client/internal/secscan/report.go).
  * the two cluster findings are networkEncryptionAnalyzer and
    autolockAnalyzer (client/internal/secscan/cluster_analyzers.go).
  * the figures are a real run against a 3-node cluster.

The GIF is committed, so you only rerun this to change the animation. Needs
Pillow and the Ubuntu Mono fonts (fonts-ubuntu; the paths below are for
Debian/Ubuntu). Usage, from the repo root:

    python3 site/report-gen.py site/report.gif
"""
import sys
from PIL import Image, ImageDraw, ImageFont

OUT = sys.argv[1] if len(sys.argv) > 1 else "report.gif"

# --- palette (matches the site theme) --------------------------------------
BG      = (13, 17, 23)
BAR     = (22, 27, 34)
BORDER  = (48, 54, 61)
FG      = (230, 237, 243)
MUTED   = (139, 148, 158)
COMMENT = (99, 110, 123)
AQUA    = (57, 197, 207)
BLUE    = (88, 166, 255)
GREEN   = (63, 185, 80)
RED     = (248, 81, 73)
YELLOW  = (210, 153, 34)

FONT  = "/usr/share/fonts/truetype/ubuntu/UbuntuMono-R.ttf"
FONTB = "/usr/share/fonts/truetype/ubuntu/UbuntuMono-B.ttf"
FS, LH, PAD_X, BAR_H, TOP = 19, 26, 24, 38, 52
W, H = 860, 470

REG = ImageFont.truetype(FONT, FS)
BLD = ImageFont.truetype(FONTB, FS)
CW = REG.getlength("M")

frames, durations = [], []
rows = []  # each row: list of (text, color, bold)


def new_row():
    rows.append([])


def clear():
    """Start a new page. The report is longer than one screen, and the part
    worth showing is not the first sixteen lines of it."""
    del rows[:]


def put(text, color=FG, bold=False):
    if not rows:
        new_row()
    rows[-1].append((text, color, bold))


def render(cursor=True, caption=None):
    img = Image.new("RGB", (W, H), BG)
    d = ImageDraw.Draw(img)
    d.rectangle([0, 0, W - 1, H - 1], outline=BORDER, width=1)
    d.rectangle([1, 1, W - 2, BAR_H], fill=BAR)
    for i, c in enumerate((RED, YELLOW, GREEN)):
        d.ellipse([18 + i * 22, BAR_H // 2 - 6, 30 + i * 22, BAR_H // 2 + 6], fill=c)
    d.text((W / 2, BAR_H / 2), "swarm cluster — operator terminal",
           font=REG, fill=MUTED, anchor="mm")
    y, last_x, last_y = TOP, PAD_X, TOP
    for r, row in enumerate(rows):
        x = PAD_X
        for (text, color, bold) in row:
            f = BLD if bold else REG
            d.text((x, y), text, font=f, fill=color)
            x += f.getlength(text)
        if r == len(rows) - 1:
            last_x, last_y = x, y
        y += LH
    if cursor and rows:
        d.rectangle([last_x + 1, last_y + 2, last_x + CW, last_y + FS + 4], fill=AQUA)
    if caption:
        ov = Image.new("RGBA", (W, H), (13, 17, 23, 210))
        img = Image.alpha_composite(img.convert("RGBA"), ov).convert("RGB")
        d = ImageDraw.Draw(img)
        big = ImageFont.truetype(FONTB, 27)
        med = ImageFont.truetype(FONT, 20)
        d.text((W / 2, H / 2 - 22), caption[0], font=big, fill=FG, anchor="mm")
        d.text((W / 2, H / 2 + 22), caption[1], font=med, fill=AQUA, anchor="mm")
    return img


def snap(ms, cursor=True, caption=None):
    frames.append(render(cursor=cursor, caption=caption))
    durations.append(ms)


def type_cmd(prompt, cmd, color=FG, bold=True, step=2, hold=320):
    new_row()
    for seg in prompt:
        put(*seg)
    put("", color, bold)
    i = 0
    while i < len(cmd):
        i += step
        t, c, b = rows[-1][-1]
        rows[-1][-1] = (cmd[:i], c, b)
        snap(60)
    snap(hold)


def show_cmd(prompt, cmd, color=FG, bold=True):
    """Place a fully typed command line in one frame — the hero first frame, so
    the command is legible the instant the slide appears."""
    new_row()
    for seg in prompt:
        put(*seg)
    put(cmd, color, bold)


def out(text=None, color=MUTED, bold=False):
    new_row()
    if text is not None:
        put(text, color, bold)


def comment(prompt, text):
    new_row()
    for seg in prompt:
        put(*seg)
    put("# " + text, COMMENT, False)


def sev(label, count, color):
    """One row of the summary table, severity coloured the way the report's
    reader will colour it in their head anyway."""
    new_row()
    put("| ", MUTED)
    put(f"{label:<7}", color, True)
    put(" | ", MUTED)
    put(f"{count:>3}", FG)
    put(" |", MUTED)


LOCAL = [("you@laptop", GREEN, False), (":~", MUTED, False), ("$ ", AQUA, True)]

# ===================== PAGE 1: the command and what it covers ===============
# Frame 0: the payoff command, fully legible right away.
show_cmd(LOCAL, "swarmexec security report -o security.md")
snap(1200)

out()
put("wrote ", MUTED)
put("security.md", AQUA, True)
put(" — 122 finding(s), ", MUTED)
put("25 above low", YELLOW, True)
snap(650)

comment(LOCAL, "the ui's \"!\" overlay scans service specs")
snap(450)
comment(LOCAL, "the report adds the cluster: networks, secrets, nodes")
snap(1200)

# ===================== PAGE 2: the summary ==================================
clear()
out("# Security report — prod", FG, True)
snap(600)
out()
out("Generated 2026-09-15T10:04:11Z by swarmexec v1.17.0.", COMMENT)
snap(200)
out()
out("Scanned 47 services, 15 networks, 37 secrets, 9 configs and 3 nodes.", MUTED)
snap(650)
out()
out("## Summary", AQUA, True)
out()
new_row()
put("| Severity | Findings |", MUTED)
snap(200)
sev("high", 15, RED)
snap(180)
sev("medium", 13, YELLOW)
snap(180)
sev("low", 99, MUTED)
snap(550)
out()
new_row()
put("25", YELLOW, True)
put(" finding(s) above low, across ", MUTED)
put("23", YELLOW, True)
put(" subject(s).", MUTED)
snap(1300)

# ===================== PAGE 3: what only a cluster scan finds ===============
clear()
out("## Cluster", AQUA, True)
out()
new_row()
put("### Network ", FG, True)
put("`gateway`", BLUE, True)
new_row()
put("| ", MUTED)
put("medium", YELLOW, True)
put(" | overlay traffic is not encrypted", FG)
snap(900)

out()
new_row()
put("### Swarm ", FG, True)
put("`swarm`", BLUE, True)
new_row()
put("| ", MUTED)
put("medium", YELLOW, True)
put(" | managers are not autolocked", FG)
snap(950)

out()
out("## Not covered", AQUA, True)
out()
out("- The node agents were not queried, so agent version", MUTED)
out("  skew was not checked.", MUTED)
snap(850)

out()
new_row()
put("-> ", AQUA, True)
put("an absent finding means \"not found\"", AQUA, True)
put(" — never \"not looked at\"", MUTED)
snap(1500)

TR = len(frames)
snap(1600, cursor=False,
     caption=("a security report for the whole cluster",
              "services, networks, secrets, nodes — as Markdown"))

# ============================ save (shared 64-color palette) =================
combo = Image.new("RGB", (W, H * 2))
combo.paste(frames[TR - 1], (0, 0))   # text-heavy final scene frame
combo.paste(frames[TR], (0, H))        # dimmed caption card
pal = combo.convert("P", palette=Image.ADAPTIVE, colors=64)
qf = [f.quantize(palette=pal, dither=Image.NONE) for f in frames]
qf[0].save(OUT, save_all=True, append_images=qf[1:], duration=durations,
           loop=0, optimize=True, disposal=2)
print(f"wrote {OUT}: {len(frames)} frames, {sum(durations)/1000:.1f}s")
