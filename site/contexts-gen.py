#!/usr/bin/env python3
"""Generate site/contexts.gif — switching cluster without losing your place.

Fourth sibling of demo-gen.py / portforward-gen.py / ui-gen.py / report-gen.py:
same canvas, window chrome, palette, fonts and save pipeline. Like ui-gen.py it
PAINTS THE APP on a character grid rather than replaying a shell transcript.

What it shows, and why this scene and not another: switching cluster used to
rebuild the whole UI, so the story is not "you can switch" — it is that **you
come back to where you were**. The cursor is parked on a service deep in the
tree before leaving and is still there on return, which is the only frame that
proves it.

Mirrors the real client:

  * tab bar   — ui_tabbar.go + uiTabList: SEVEN tabs. Contexts is not among them
                any more (ui_sidebar.go): it is the column on the right.
  * sidebar   — renderContexts() in ui_contexts.go: no header row, one context
                per line, "▶ " active / "· " visited-and-connected (green) /
                "  " untouched. Framed like tview draws a bordered Box, and the
                border takes the accent colour while it holds the keyboard.
  * clipping  — the tree is cut off at the sidebar with one blank column between
                (the spacer item in the body Flex), exactly as tview clips.
  * footer    — helpFor("containers") / helpFor("contexts") and updateStatus().

Both clusters are invented — no real host, service or id from anyone's cluster.

The GIF is committed, so you only rerun this to change the animation. Needs
Pillow and the Ubuntu Mono fonts (fonts-ubuntu). From the repo root:

    python3 site/contexts-gen.py site/contexts.gif
"""
import sys
from PIL import Image, ImageDraw, ImageFont

OUT = sys.argv[1] if len(sys.argv) > 1 else "contexts.gif"

# --- window chrome (matches the sibling GIFs / the site theme) --------------
BG = (13, 17, 23)
BAR = (22, 27, 34)
CHROME_BORDER = (48, 54, 61)
RED_DOT, YELLOW_DOT, GREEN_DOT = (248, 81, 73), (210, 153, 34), (63, 185, 80)

# --- the TUI's own palette (client/internal/cli/theme.go) -------------------
P_BG = (13, 17, 23)
P_BORDER = (63, 74, 90)
P_ACCENT = (45, 212, 191)
P_TEXT = (230, 237, 243)
P_DIM = (148, 163, 184)
P_FADE = (100, 116, 139)

T_RED = (255, 0, 0)
T_YELLOW = (255, 255, 0)
T_WHITE = (255, 255, 255)
T_GRAY = (128, 128, 128)
T_AQUA = (0, 255, 255)
T_ORANGE = (255, 165, 0)
T_GREEN = (0, 128, 0)      # tcell "green", what renderContexts uses for "visited"
# selStyle in runUI(): every table in this UI highlights its cursor row teal on
# white — a fixed style, NOT the row's own colour the way the tree inverts it.
SEL_BG, SEL_FG = (0, 128, 128), (255, 255, 255)
SHIELD = (124, 148, 176)

FONT = "/usr/share/fonts/truetype/ubuntu/UbuntuMono-R.ttf"
FONTB = "/usr/share/fonts/truetype/ubuntu/UbuntuMono-B.ttf"
W, H = 860, 470
BAR_H = 38

FS = 13
REG = ImageFont.truetype(FONT, FS)
BLD = ImageFont.truetype(FONTB, FS)
CW = REG.getlength("M")
CH = 17
COLS, ROWS = 117, 25
X0, Y0 = 3, 42
ASC, DESC = REG.getmetrics()
TY = (CH - (ASC + DESC)) // 2

TREE_TOP = 2
HELP_ROW = ROWS - 2
STATUS_ROW = ROWS - 1

# --- sidebar geometry (ui_sidebar.go: width follows the longest name) -------
SB_W = 16                  # columns, including the border
SB_C0 = COLS - SB_W        # first sidebar column
CLIP_C = SB_C0 - 1         # the blank spacer column the body Flex inserts

# ============================ the two (invented) clusters ====================
# stack, name, mode, running, desired, image, ports, risky
PROD = [
    ("shop", "shop_api", "replicated", 3, 3, "acme/shop-api:1.8.2", "", True),
    ("shop", "shop_db", "replicated", 1, 1, "postgres:16.4", "", False),
    ("shop", "shop_redis", "replicated", 1, 1, "redis:7.4-alpine", "", False),
    ("shop", "shop_web", "replicated", 2, 2, "nginx:1.27-alpine", "*:8080->80/tcp", False),
    ("telemetry", "telemetry_grafana", "replicated", 1, 1, "grafana/grafana:11.2.0",
     "*:3000->3000/tcp", False),
    ("telemetry", "telemetry_loki", "replicated", 1, 1, "grafana/loki:3.1.1", "", False),
    ("telemetry", "telemetry_promtail", "global", 5, 5, "grafana/promtail:3.1.1", "", False),
    ("", "traefik", "global", 5, 5, "traefik:v3.1", "*:443->443/tcp", True),
]

STAGING = [
    ("shop", "shop_api", "replicated", 1, 1, "acme/shop-api:1.9.0-rc3", "", True),
    ("shop", "shop_db", "replicated", 1, 1, "postgres:16.4", "", False),
    ("shop", "shop_web", "replicated", 1, 1, "nginx:1.27-alpine", "*:8080->80/tcp", False),
    ("", "traefik", "global", 2, 2, "traefik:v3.1", "*:443->443/tcp", False),
]

CLUSTERS = {
    "prod": dict(services=PROD, nodes=5, agents=5),
    "staging": dict(services=STAGING, nodes=2, agents=2),
}

# The context list is the same whichever cluster is visible (docker's own store).
CONTEXTS = ["prod", "staging", "edge", "default"]


def widths(services):
    return (max(len(s[1]) for s in services),
            max(len(s[2]) for s in services),
            max(len("%d/%d" % (s[3], s[4])) for s in services),
            max(len(s[5]) for s in services))


def service_color(running, desired):
    if desired == 0:
        return T_GRAY
    if running == 0:
        return T_RED
    if running != desired:
        return T_ORANGE
    return T_AQUA


def service_row(svc, w):
    _, name, mode, run, des, image, ports, risky = svc
    name_w, mode_w, repl_w, image_w = w
    b = "🛡 " if risky else "  "
    b += "%-*s  %-*s  %-*s" % (name_w, name, mode_w, mode, repl_w, "%d/%d" % (run, des))
    b += "  %-*s" % (image_w, image)
    if ports:
        b += "  %s" % ports
    return b.rstrip()


# ============================ character-grid canvas ==========================
EMPTY = (" ", P_TEXT, None, False)
LINE_ART = {"│", "─", "├", "╰"}
# ▶ is the active-context marker (renderContexts); the fonts here do not
# carry it, so it is drawn as a vector like its smaller sibling ▸.
VECTORS = {"▾", "▸", "▶", "●", "🛡"}


class Grid:
    def __init__(self):
        self.cells = [[EMPTY] * COLS for _ in range(ROWS)]

    def put(self, r, c, text, fg=P_TEXT, bg=None, bold=False):
        for ch in text:
            if 0 <= r < ROWS and 0 <= c < COLS:
                self.cells[r][c] = (ch, fg, bg, bold)
            c += 1
        return c

    def segs(self, r, c, segs, base=P_TEXT, bg=None, bold=False):
        for text, fg in segs:
            c = self.put(r, c, text, fg if fg else base, bg, bold)
        return c

    def clip_right(self, first_col, r0, r1):
        """What tview does at the sidebar's edge: the content simply stops."""
        for r in range(r0, r1):
            for c in range(first_col, COLS):
                self.cells[r][c] = EMPTY


# ============================ vector glyphs ==================================
def draw_line_art(d, ch, x, y, color):
    mx, my = x + CW / 2, y + CH / 2
    if ch in ("│", "├", "╰"):
        y1 = y + CH if ch != "╰" else my
        d.line([(mx, y), (mx, y1)], fill=color, width=1)
    if ch in ("─",):
        d.line([(x, my), (x + CW, my)], fill=color, width=1)
    if ch in ("├", "╰"):
        d.line([(mx, my), (x + CW, my)], fill=color, width=1)


GW, SS = 9, 8
_glyphs = {}


def glyph_mask(ch):
    if ch in _glyphs:
        return _glyphs[ch]
    m = Image.new("L", (GW * SS, CH * SS), 0)
    d = ImageDraw.Draw(m)
    mx, my, s = GW * SS / 2, CH * SS / 2, SS
    if ch == "▾":
        d.polygon([(mx - 3.4 * s, my - 2 * s), (mx + 3.4 * s, my - 2 * s), (mx, my + 2.4 * s)], fill=255)
    elif ch == "▸":
        d.polygon([(mx - 2 * s, my - 3.4 * s), (mx - 2 * s, my + 3.4 * s), (mx + 2.4 * s, my)], fill=255)
    elif ch == "▶":
        d.polygon([(mx - 2.6 * s, my - 4 * s), (mx - 2.6 * s, my + 4 * s), (mx + 3 * s, my)], fill=255)
    elif ch == "●":
        d.ellipse([mx - 2.2 * s, my - 2.2 * s, mx + 2.2 * s, my + 2.2 * s], fill=255)
    elif ch == "🛡":
        d.polygon([(mx - 3 * s, my - 4.4 * s), (mx + 3 * s, my - 4.4 * s),
                   (mx + 3 * s, my + 0.4 * s), (mx, my + 4.6 * s), (mx - 3 * s, my + 0.4 * s)],
                  fill=255)
    m = m.resize((GW, CH), Image.LANCZOS)
    _glyphs[ch] = m
    return m


def draw_vector(img, ch, x, y, color):
    img.paste(color, (int(round(x + CW / 2 - GW / 2)), int(round(y))), glyph_mask(ch))


# ============================ frame rendering ================================
frames, durations = [], []


def paint_grid(img, d, grid):
    for r in range(ROWS):
        row = grid.cells[r]
        c = 0
        while c < COLS:
            bg = row[c][2]
            if bg is None:
                c += 1
                continue
            c1 = c
            while c1 < COLS and row[c1][2] == bg:
                c1 += 1
            d.rectangle([X0 + c * CW, Y0 + r * CH,
                         X0 + c1 * CW - 1, Y0 + (r + 1) * CH - 1], fill=bg)
            c = c1
        run, run_c, run_style = "", 0, None

        def flush():
            nonlocal run, run_c, run_style
            if run.strip():
                fg, bold = run_style
                d.text((X0 + run_c * CW, Y0 + r * CH + TY), run,
                       font=BLD if bold else REG, fill=fg)
            run, run_c, run_style = "", 0, None

        for c in range(COLS):
            ch, fg, _, bold = row[c]
            x, y = X0 + c * CW, Y0 + r * CH
            if ch in LINE_ART:
                flush()
                draw_line_art(d, ch, x, y, fg)
            elif ch in VECTORS:
                flush()
                draw_vector(img, ch, x, y, SHIELD if ch == "🛡" else fg)
            elif ch == "━":
                flush()
                d.rectangle([x, y + CH / 2 - 1, x + CW - 1, y + CH / 2], fill=fg)
            else:
                if run_style != (fg, bold):
                    flush()
                    run_c, run_style = c, (fg, bold)
                run += ch
        flush()


def draw_sidebar_frame(d, focused):
    """The sidebar's border and title, drawn the way tview paints a bordered Box
    (rounded corners from Borders.*). It takes the accent colour while it holds
    the keyboard — the one visible answer to "where are my keystrokes going"."""
    col = P_ACCENT if focused else P_BORDER
    bx0, by0 = X0 + SB_C0 * CW, Y0 + TREE_TOP * CH
    bx1, by1 = X0 + COLS * CW - 1, Y0 + HELP_ROW * CH - 1
    d.rounded_rectangle([bx0, by0, bx1, by1], radius=5, outline=col, width=1)
    title = " contexts "
    tcol = SB_C0 + 1 + ((SB_W - 2) - len(title)) // 2
    d.rectangle([X0 + tcol * CW, by0, X0 + (tcol + len(title)) * CW - 1, by0 + CH - 1],
                fill=P_BG)
    d.text((X0 + tcol * CW, by0 + TY), title, font=REG, fill=col)


def render(grid, focused=False, caption=None, key=None):
    img = Image.new("RGB", (W, H), BG)
    d = ImageDraw.Draw(img)
    d.rectangle([0, 0, W - 1, H - 1], outline=CHROME_BORDER, width=1)
    d.rectangle([1, 1, W - 2, BAR_H], fill=BAR)
    for i, c in enumerate((RED_DOT, YELLOW_DOT, GREEN_DOT)):
        d.ellipse([18 + i * 22, BAR_H // 2 - 6, 30 + i * 22, BAR_H // 2 + 6], fill=c)
    d.text((W / 2, BAR_H / 2), "swarm clusters — operator terminal",
           font=ImageFont.truetype(FONT, 19), fill=(139, 148, 158), anchor="mm")
    if key:
        f = ImageFont.truetype(FONTB, 15)
        w = f.getlength(key) + 18
        d.rounded_rectangle([W - 24 - w, BAR_H / 2 - 11, W - 24, BAR_H / 2 + 11],
                            radius=6, outline=P_ACCENT, width=1)
        d.text((W - 24 - w / 2, BAR_H / 2), key, font=f, fill=P_ACCENT, anchor="mm")

    paint_grid(img, d, grid)
    draw_sidebar_frame(d, focused)

    if caption:
        ov = Image.new("RGBA", (W, H), (13, 17, 23, 210))
        img = Image.alpha_composite(img.convert("RGBA"), ov).convert("RGB")
        d = ImageDraw.Draw(img)
        big = ImageFont.truetype(FONTB, 27)
        med = ImageFont.truetype(FONT, 20)
        d.text((W / 2, H / 2 - 22), caption[0], font=big, fill=(230, 237, 243), anchor="mm")
        d.text((W / 2, H / 2 + 22), caption[1], font=med, fill=(57, 197, 207), anchor="mm")
    return img


def snap(ms, grid, **kw):
    frames.append(render(grid, **kw))
    durations.append(ms)


# ============================ the app's chrome ===============================
# uiTabList after the sidebar change: seven tabs, Contexts no longer among them.
TABS = [("Stacks/Services", "containers"), ("Volumes", "volumes"),
        ("Forwards", "forwards"), ("Networks", "networks"),
        ("Secrets", "secrets"), ("Nodes", "nodes"), ("Configs", "configs")]


def draw_tabbar(g, active="containers"):
    g.put(1, 0, "─" * COLS, P_BORDER)
    col = 1
    for i, (label, key) in enumerate(TABS):
        on = key == active
        start = col
        col = g.put(0, col, " ")
        col = g.put(0, col, label, P_ACCENT if on else P_DIM, bold=on)
        col = g.put(0, col, " ")
        col = g.put(0, col, str(i + 1), P_ACCENT if on else P_FADE)
        col = g.put(0, col, " ")
        if on:
            g.put(1, start, "━" * (col - start), P_ACCENT)
        col += 1


def keys(*pairs):
    out = []
    for k, label in zip(pairs[::2], pairs[1::2]):
        out.append((k, T_YELLOW))
        out.append((label, T_WHITE))
    return out


HELP_TREE = keys(
    " ?", " help  ", "q", " quit   ", "j/k", " move  ", "Enter", " open  ",
    "L", " logs  ", "h/l", " fold  ", "i", " inspect  ", "/", " search  ",
    "c", " contexts")
# helpFor("contexts") — the sidebar's footer while it holds the keyboard.
HELP_SIDEBAR = keys(
    " ?", " help  ", "q", " quit   ", "j/k", " up/down  ", "Esc", " back  ",
    "Enter/u", " switch  ", "i", " details  ", "n", " new  ", "d", " delete")


def draw_footer(g, help_segs, cluster):
    info = CLUSTERS[cluster]
    g.segs(HELP_ROW, 0, help_segs)
    g.segs(STATUS_ROW, 0, [
        (" ", None), ("ctx", T_AQUA), (" %s  ·  " % cluster, T_WHITE),
        ("%d" % info["nodes"], T_AQUA), (" nodes · ", T_WHITE),
        ("%d" % info["agents"], T_AQUA), ("/%d agents" % info["nodes"], T_WHITE),
    ], base=T_WHITE)


# ============================ the tree =======================================
def tree_rows(cluster):
    services = CLUSTERS[cluster]["services"]
    w = widths(services)
    stacks = []
    for stack, *_ in services:
        name = stack or "(no stack)"
        if name not in stacks:
            stacks.append(name)
    stacks.sort(key=lambda n: (n == "(no stack)", n))

    rows = []
    for stack in stacks:
        members = [s for s in services if (s[0] or "(no stack)") == stack]
        run = sum(s[3] for s in members)
        des = sum(s[4] for s in members)
        risky = sum(1 for s in members if s[7])
        segs = [("▾ %s  " % stack, None),
                ("(%d svc · %d/%d)" % (len(members), run, des), T_GRAY)]
        if risky:
            segs += [("  ", None), ("🛡 %d" % risky, T_RED)]
        rows.append(dict(level=0, segs=segs, color=service_color(run, des), svc=None))
        for j, svc in enumerate(members):
            rows.append(dict(level=1, segs=[("▸ " + service_row(svc, w), None)],
                             color=service_color(svc[3], svc[4]), svc=svc[1],
                             last=j == len(members) - 1))
    return rows


def draw_tree(g, rows, cursor):
    for i, row in enumerate(rows):
        r = TREE_TOP + i
        if r >= HELP_ROW:
            break
        sel = i == cursor
        bg = row["color"] if sel else None
        base = P_BG if sel else row["color"]
        if row["level"] == 0:
            g.segs(r, 0, row["segs"], base=base, bg=bg)
        else:
            g.put(r, 0, "╰" if row["last"] else "├", P_BORDER)
            g.put(r, 1, "──", P_BORDER)
            g.segs(r, 3, row["segs"], base=base, bg=bg)


# ============================ the sidebar ====================================
def draw_sidebar(g, active, visited, cursor=None):
    """renderContexts(): no header, one name per row, marker then name.

    ▶ active · visited-and-still-connected (green, so switching there is
    immediate) · blank for one never opened this session."""
    for i, name in enumerate(CONTEXTS):
        r = TREE_TOP + 1 + i
        if name == active:
            mark, col = "▶ ", T_AQUA
        elif name in visited:
            mark, col = "· ", T_GREEN
        else:
            mark, col = "  ", T_WHITE
        sel = cursor is not None and i == cursor
        text = (mark + name).ljust(SB_W - 2)
        g.put(r, SB_C0 + 1, text, SEL_FG if sel else col, SEL_BG if sel else None)


def screen(cluster, cursor, visited, focused=False, sb_cursor=None):
    g = Grid()
    draw_tabbar(g)
    draw_tree(g, tree_rows(cluster), cursor)
    g.clip_right(CLIP_C, TREE_TOP, HELP_ROW)   # the tree stops at the sidebar
    draw_sidebar(g, cluster, visited, sb_cursor)
    draw_footer(g, HELP_SIDEBAR if focused else HELP_TREE, cluster)
    return g


# ============================ the scene ======================================
# The cursor sits on telemetry_loki — deep enough in the tree that a reset would
# be unmistakable, which is the whole point of the last frame. Row 7: the shop
# stack row, its four services, the telemetry stack row, grafana, then loki.
LOKI = 7

# 1. On prod, working in the tree. The sidebar is simply there, saying where we
#    are and what else exists.
snap(2000, screen("prod", LOKI, visited=set()))

# 2. "c" hands the keyboard to the sidebar: the border takes the accent colour
#    and the footer switches to its keys.
snap(1200, screen("prod", LOKI, set(), focused=True, sb_cursor=0), key="c")

# 3. Down to staging, and switch.
snap(1100, screen("prod", LOKI, set(), focused=True, sb_cursor=1), key="j")

# 4. The switch: staging's tree, staging's footer. prod is marked "·" — still
#    connected, so coming back costs nothing.
snap(2400, screen("staging", 0, visited={"prod"}), key="Enter")
snap(1500, screen("staging", 2, visited={"prod"}), key="j")
snap(1100, screen("staging", 2, visited={"prod"}, focused=True, sb_cursor=1), key="c")
snap(1100, screen("staging", 2, visited={"prod"}, focused=True, sb_cursor=0), key="k")

# 5. Back on prod — and the cursor is still on telemetry_loki. That is the claim
#    this whole animation exists to make.
snap(2800, screen("prod", LOKI, visited={"staging"}), key="Enter")

TR = len(frames)
snap(2000, screen("prod", LOKI, visited={"staging"}),
     caption=("switch cluster, keep your place",
              "position, filters and port-forwards survive the switch"))

# ============================ save (shared palette) ==========================
combo = Image.new("RGB", (W, H * 3))
combo.paste(frames[0], (0, 0))
combo.paste(frames[3], (0, H))
combo.paste(frames[TR], (0, H * 2))
pal = combo.convert("P", palette=Image.ADAPTIVE, colors=128)
qf = [f.quantize(palette=pal, dither=Image.NONE) for f in frames]
qf[0].save(OUT, save_all=True, append_images=qf[1:], duration=durations,
           loop=0, optimize=True, disposal=2)
print(f"wrote {OUT}: {len(frames)} frames, {sum(durations)/1000:.1f}s")
