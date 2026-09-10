#!/usr/bin/env python3
"""Generate site/ui.gif — the swarmexec TUI demo (`swarmexec ui`).

Third sibling of demo-gen.py / portforward-gen.py: same canvas, window chrome,
palette family, fonts and save pipeline. The difference is what is inside the
window: the other two are shell transcripts, this one PAINTS THE APP — a
full-screen tview UI on a character grid — so the frames are composed cell by
cell instead of line by line.

Everything drawn here mirrors the real client, so the still matches what a user
actually sees:

  * tab bar        — ui_tabbar.go (Draw) + uiTabList in ui_state.go: " Label N ",
                     active tab in the accent colour + a thin ━ underline over a
                     faint ─ baseline across the strip.
  * theme          — theme.go (palette): bg #0d1117, border #3f4a5a,
                     accent #2dd4bf, text #e6edf3, dim #94a3b8, fade #64748b.
                     Colour *tags* ([red]/[gray]/[aqua]/[yellow]/[white]) use
                     tcell's W3C values, as tcell resolves them.
  * tree           — renderContainers/markStack (ui_containers.go) and
                     serviceRow/securityBadge/serviceColor (ui.go): three levels
                     (stack → service → container), stack rows
                     "▾ name  (N svc · R/D)  🛡 K", service rows in
                     `docker service ls` style with a leading shield slot,
                     container leaves "<12hex>  <node>  slot N  up <age>".
                     tview's own tree graphics (├── / ╰── / │) and the
                     selected-row inversion are reproduced.
  * footer         — helpFor("containers") + updateStatus() (ui.go), two rows.
  * risks overlay  — showSecurityRisks (ui_security.go): centred 84-col box,
                     backdrop dimmed by config.DefaultUIDim (0.6), the service
                     under the cursor pre-highlighted (tview draws a highlighted
                     region with fg/bg swapped).

The cluster is invented (a `shop` stack, a `telemetry` stack, one unstacked
`traefik`) — no real host, service or id from anyone's cluster.

Glyphs the Ubuntu Mono fonts do not carry (tree lines, ▾ ▸ ●, the 🛡 emoji) are
drawn as vectors on the cell grid; at 13 px that is also crisper than a
fallback font would be.

The GIF is committed, so you only rerun this to change the animation. Needs
Pillow and the Ubuntu Mono fonts (fonts-ubuntu; the paths below are for
Debian/Ubuntu). Usage, from the repo root:

    python3 site/ui-gen.py site/ui.gif
"""
import sys
from PIL import Image, ImageDraw, ImageFont

OUT = sys.argv[1] if len(sys.argv) > 1 else "ui.gif"

# --- window chrome (matches the sibling GIFs / the site theme) --------------
BG = (13, 17, 23)
BAR = (22, 27, 34)
CHROME_BORDER = (48, 54, 61)
RED_DOT, YELLOW_DOT, GREEN_DOT = (248, 81, 73), (210, 153, 34), (63, 185, 80)

# --- the TUI's own palette (client/internal/cli/theme.go) -------------------
P_BG = (13, 17, 23)       # palette.bg      #0d1117
P_BORDER = (63, 74, 90)   # palette.border  #3f4a5a
P_ACCENT = (45, 212, 191)  # palette.accent  #2dd4bf
P_TEXT = (230, 237, 243)  # palette.text    #e6edf3
P_DIM = (148, 163, 184)   # palette.textDim #94a3b8
P_FADE = (100, 116, 139)  # palette.textFade #64748b

# tcell colour-tag values (tcell/color.go ColorValues) — what [red] etc. mean.
T_RED = (255, 0, 0)
T_YELLOW = (255, 255, 0)
T_WHITE = (255, 255, 255)
T_GRAY = (128, 128, 128)
T_AQUA = (0, 255, 255)
T_ORANGE = (255, 165, 0)
T_BLACK = (0, 0, 0)

# The 🛡 badge is an emoji: terminals paint it from the emoji font and ignore the
# cell's foreground colour (see securityBadge()), so it keeps one fixed tint.
SHIELD = (124, 148, 176)

BACKDROP_DIM = 0.6  # config.DefaultUIDim

FONT = "/usr/share/fonts/truetype/ubuntu/UbuntuMono-R.ttf"
FONTB = "/usr/share/fonts/truetype/ubuntu/UbuntuMono-B.ttf"
W, H = 860, 470
BAR_H = 38

# Character cell geometry. Ubuntu Mono advances exactly 0.56 em, so CW is the
# real per-character advance and text runs can be drawn as whole strings.
FS = 13
REG = ImageFont.truetype(FONT, FS)
BLD = ImageFont.truetype(FONTB, FS)
CW = REG.getlength("M")
CH = 17
COLS, ROWS = 117, 25          # the simulated terminal: 117×25
X0, Y0 = 3, 42                # top-left pixel of cell (0,0)
ASC, DESC = REG.getmetrics()
TY = (CH - (ASC + DESC)) // 2  # per-cell vertical text offset

# Rows inside the app: two tab-bar rows, the tree, then the two footer rows.
TREE_TOP = 2
HELP_ROW = ROWS - 2
STATUS_ROW = ROWS - 1

# ============================ the (invented) cluster =========================
# stack, name, mode, running, desired, image, ports, risks[(severity,title,detail)]
SERVICES = [
    ("shop", "shop_api", "replicated", 3, 3, "acme/shop-api:1.8.2", "",
     [("high", "secret in environment variable",
       "the env var STRIPE_API_KEY holds a literal value in the service spec; "
       "use a Docker secret or a *_FILE reference instead"),
      ("low", "no user set",
       "no non-root user is set; the container runs as the image's default "
       "user, which is often root")]),
    ("shop", "shop_db", "replicated", 1, 1, "postgres:16.4", "", []),
    ("shop", "shop_redis", "replicated", 1, 1, "redis:7.4-alpine", "", []),
    ("shop", "shop_web", "replicated", 2, 2, "nginx:1.27-alpine", "*:8080->80/tcp", []),
    ("shop", "shop_worker", "replicated", 2, 2, "acme/shop-worker:1.8.2", "", []),
    ("telemetry", "telemetry_grafana", "replicated", 1, 1, "grafana/grafana:11.2.0",
     "*:3000->3000/tcp", []),
    ("telemetry", "telemetry_loki", "replicated", 1, 1, "grafana/loki:3.1.1", "", []),
    ("telemetry", "telemetry_promtail", "global", 5, 5, "grafana/promtail:3.1.1", "", []),
    ("", "registry", "replicated", 1, 1, "registry:2.8.3", "*:5000->5000/tcp", []),
    ("", "traefik", "global", 5, 5, "traefik:v3.1", "*:443->443/tcp, *:80->80/tcp",
     [("high", "runs as root",
       "the service explicitly runs its container as root (User=0)")]),
]

# Running tasks per service (id, node, slot, uptime) — only the expanded service
# is ever shown, but the column widths are computed cluster-wide, as in
# renderContainers().
CONTAINERS = {
    "shop_api": [("7c1e9a4f30b2", "node-2", 1, "6d4h"),
                 ("b48d2f1c60ae", "node-4", 2, "6d4h"),
                 ("3e9a7d05c1f4", "node-5", 3, "2d11h")],
}

NAME_W = max(len(s[1]) for s in SERVICES)
MODE_W = max(len(s[2]) for s in SERVICES)
REPL_W = max(len("%d/%d" % (s[3], s[4])) for s in SERVICES)
IMAGE_W = max(len(s[5]) for s in SERVICES)
NODE_W = max(len(c[1]) for cs in CONTAINERS.values() for c in cs)
SLOT_W = max(len("slot %d" % c[2]) for cs in CONTAINERS.values() for c in cs)


def actionable(risks):
    """secscan.Actionable: informational (low) findings do not badge."""
    return any(sev != "low" for sev, _, _ in risks)


def service_color(running, desired):
    """serviceColor() in ui.go."""
    if desired == 0:
        return T_GRAY
    if running == 0:
        return T_RED
    if running != desired:
        return T_ORANGE
    return T_AQUA


def service_row(svc):
    """serviceRow() in ui.go — docker service ls style, shield slot first."""
    _, name, mode, run, des, image, ports, risks = svc
    b = "🛡 " if actionable(risks) else "  "
    b += "%-*s  %-*s  %-*s" % (NAME_W, name, MODE_W, mode, REPL_W, "%d/%d" % (run, des))
    b += "  %-*s" % (IMAGE_W, image)
    if ports:
        b += "  %s" % ports
    return b.rstrip()


def leaf_row(c):
    """The container-leaf label built in renderContainers()."""
    cid, node, slot, up = c
    return "%-12s  %-*s  %-*s  up %s" % (cid, NODE_W, node, SLOT_W, "slot %d" % slot, up)


# ============================ character-grid canvas ==========================
EMPTY = (" ", P_TEXT, None, False)
LINE_ART = {"│", "─", "├", "╰"}
VECTORS = {"▾", "▸", "●", "🛡"}


class Grid:
    """A ROWS×COLS terminal screen: each cell is (char, fg, bg, bold)."""

    def __init__(self):
        self.cells = [[EMPTY] * COLS for _ in range(ROWS)]

    def put(self, r, c, text, fg=P_TEXT, bg=None, bold=False):
        for ch in text:
            if 0 <= r < ROWS and 0 <= c < COLS:
                self.cells[r][c] = (ch, fg, bg, bold)
            c += 1
        return c

    def segs(self, r, c, segs, base=P_TEXT, bg=None, bold=False):
        """Draw [(text, colour-or-None)] runs; None means the base colour."""
        for text, fg in segs:
            c = self.put(r, c, text, fg if fg else base, bg, bold)
        return c


def dim(color):
    """dimBehind()/blendToward(): fade a colour toward the theme background."""
    if color is None:
        color = P_BG
    return tuple(int(v + (b - v) * BACKDROP_DIM) for v, b in zip(color, P_BG))


# ============================ vector glyphs ==================================
def draw_line_art(d, ch, x, y, color):
    """Tree graphics (tview draws these with Borders.* runes)."""
    mx, my = x + CW / 2, y + CH / 2
    if ch in ("│", "├", "╰"):
        y1 = y + CH if ch != "╰" else my
        d.line([(mx, y), (mx, y1)], fill=color, width=1)
    if ch in ("─",):
        d.line([(x, my), (x + CW, my)], fill=color, width=1)
    if ch in ("├", "╰"):
        d.line([(mx, my), (x + CW, my)], fill=color, width=1)


GW, SS = 9, 8          # glyph box width in px, supersampling factor
_glyphs = {}


def glyph_mask(ch):
    """▾ ▸ ● and the 🛡 emoji badge as antialiased cell-sized masks. Drawn as
    vectors (and cached) because at 13 px a fallback font renders these worse."""
    if ch in _glyphs:
        return _glyphs[ch]
    m = Image.new("L", (GW * SS, CH * SS), 0)
    d = ImageDraw.Draw(m)
    mx, my, s = GW * SS / 2, CH * SS / 2, SS
    if ch == "▾":
        d.polygon([(mx - 3.4 * s, my - 2 * s), (mx + 3.4 * s, my - 2 * s), (mx, my + 2.4 * s)], fill=255)
    elif ch == "▸":
        d.polygon([(mx - 2 * s, my - 3.4 * s), (mx - 2 * s, my + 3.4 * s), (mx + 2.4 * s, my)], fill=255)
    elif ch == "●":
        d.ellipse([mx - 2.6 * s, my - 2.6 * s, mx + 2.6 * s, my + 2.6 * s], fill=255)
    elif ch == "🛡":
        # A small shield: straight shoulders, tapered point.
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


def paint_grid(img, d, grid, dim_rows=None):
    """Paint the character grid. dim_rows=(r0,r1) fades that row band toward the
    background — what dimBehind() does to the backdrop under an open overlay."""
    for r in range(ROWS):
        row = grid.cells[r]
        dimmed = bool(dim_rows) and dim_rows[0] <= r < dim_rows[1]
        # Backgrounds first, as runs, so a selected row is one rectangle.
        c = 0
        while c < COLS:
            bg = row[c][2]
            if bg is None:
                c += 1
                continue
            c1 = c
            while c1 < COLS and row[c1][2] == bg:
                c1 += 1
            fill = dim(bg) if dimmed else bg
            d.rectangle([X0 + c * CW, Y0 + r * CH,
                         X0 + c1 * CW - 1, Y0 + (r + 1) * CH - 1], fill=fill)
            c = c1
        # Then the glyphs: ASCII as runs, everything else per cell.
        run, run_c, run_style = "", 0, None
        def flush():
            nonlocal run, run_c, run_style
            if run.strip():
                fg, bold = run_style
                d.text((X0 + run_c * CW, Y0 + r * CH + TY), run,
                       font=BLD if bold else REG, fill=dim(fg) if dimmed else fg)
            run, run_c, run_style = "", 0, None
        for c in range(COLS):
            ch, fg, _, bold = row[c]
            x, y = X0 + c * CW, Y0 + r * CH
            col = dim(fg) if dimmed else fg
            if ch in LINE_ART:
                flush()
                draw_line_art(d, ch, x, y, col)
            elif ch in VECTORS:
                flush()
                shield = dim(SHIELD) if dimmed else SHIELD
                draw_vector(img, ch, x, y, shield if ch == "🛡" else col)
            elif ch == "━":
                flush()
                d.rectangle([x, y + CH / 2 - 1, x + CW - 1, y + CH / 2], fill=col)
            else:
                if run_style != (fg, bold):
                    flush()
                    run_c, run_style = c, (fg, bold)
                run += ch
        flush()


def render(grid, overlay=None, caption=None, key=None):
    """One frame: window chrome + the app grid (+ an overlay, + a caption card)."""
    img = Image.new("RGB", (W, H), BG)
    d = ImageDraw.Draw(img)
    d.rectangle([0, 0, W - 1, H - 1], outline=CHROME_BORDER, width=1)
    d.rectangle([1, 1, W - 2, BAR_H], fill=BAR)
    for i, c in enumerate((RED_DOT, YELLOW_DOT, GREEN_DOT)):
        d.ellipse([18 + i * 22, BAR_H // 2 - 6, 30 + i * 22, BAR_H // 2 + 6], fill=c)
    d.text((W / 2, BAR_H / 2), "swarm cluster — operator terminal",
           font=ImageFont.truetype(FONT, 19), fill=(139, 148, 158), anchor="mm")
    if key:  # the key just pressed, shown in the window chrome (not the app)
        f = ImageFont.truetype(FONTB, 15)
        w = f.getlength(key) + 18
        d.rounded_rectangle([W - 24 - w, BAR_H / 2 - 11, W - 24, BAR_H / 2 + 11],
                            radius=6, outline=P_ACCENT, width=1)
        d.text((W - 24 - w / 2, BAR_H / 2), key, font=f, fill=P_ACCENT, anchor="mm")

    if overlay is None:
        paint_grid(img, d, grid)
    else:
        box, ogrid, title = overlay
        r0, r1, c0, c1 = box
        # The backdrop recedes over everything the overlay page covers — which is
        # every row but the two footer ones, so the overlay's own key hints there
        # stay crisp (centered()/overlayFooterReserve).
        paint_grid(img, d, grid, dim_rows=(0, HELP_ROW))
        bx0, by0 = X0 + c0 * CW, Y0 + r0 * CH
        bx1, by1 = X0 + c1 * CW - 1, Y0 + r1 * CH - 1
        d.rectangle([bx0, by0, bx1, by1], fill=P_BG)
        d.rounded_rectangle([bx0 + CW / 2, by0 + CH / 2, bx1 - CW / 2, by1 - CH / 2],
                            radius=5, outline=P_BORDER, width=1)
        # Box title, centred on the top border (tview's Box.DrawForSubclass).
        tcol = c0 + 1 + ((c1 - c0 - 2) - len(title)) // 2
        d.rectangle([X0 + tcol * CW, by0, X0 + (tcol + len(title)) * CW - 1,
                     by0 + CH - 1], fill=P_BG)
        d.text((X0 + tcol * CW, by0 + TY), title, font=REG, fill=P_ACCENT)
        paint_grid(img, d, ogrid)

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
TABS = [("Stacks/Services", "containers"), ("Volumes", "volumes"),
        ("Forwards", "forwards"), ("Networks", "networks"),
        ("Secrets", "secrets"), ("Contexts", "contexts"), ("Nodes", "nodes")]


def draw_tabbar(g, active="containers"):
    """tabStrip.Draw(): " Label N " cells, accent + ━ under the active one."""
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


def draw_footer(g, help_segs):
    g.segs(HELP_ROW, 0, help_segs)
    g.segs(STATUS_ROW, 0, [
        (" ", None), ("ctx", T_AQUA), (" prod  ·  ", T_WHITE),
        ("5", T_AQUA), (" nodes · ", T_WHITE), ("5", T_AQUA), ("/5 agents", T_WHITE),
    ], base=T_WHITE)


def keys(*pairs):
    """helpFor()/footerKeys(): [yellow]key[white] label pairs."""
    out = []
    for k, label in zip(pairs[::2], pairs[1::2]):
        out.append((k, T_YELLOW))
        out.append((label, T_WHITE))
    return out


# helpFor("containers") with the default keymap (keymap.go keyActions).
HELP_CONTAINERS = keys(
    " ?", " help  ", "q", " quit   ", "j/k", " up/down  ", "Enter", " expand/menu  ",
    "L", " logs  ", "h/l", " fold  ", "i", " inspect  ", "/", " search  ",
    "p", " forward  ", "!", " risks  ", "s", " stacks")
# pushOverlayHelp(footerKeys("j/k", "scroll", "Esc", "close"))
HELP_OVERLAY = keys(" j/k", " scroll  ", "Esc", " close ")


# ============================ the tree =======================================
def tree_rows(expanded):
    """The visible rows renderContainers() would build, grouped by stack."""
    stacks = []
    for stack, *_ in SERVICES:
        name = stack or "(no stack)"
        if name not in stacks:
            stacks.append(name)
    stacks.sort(key=lambda n: (n == "(no stack)", n))  # unstacked sinks last

    rows = []
    for stack in stacks:
        members = [s for s in SERVICES if (s[0] or "(no stack)") == stack]
        run = sum(s[3] for s in members)
        des = sum(s[4] for s in members)
        risky = sum(1 for s in members if actionable(s[7]))
        segs = [("▾ %s  " % stack, None),
                ("(%d svc · %d/%d)" % (len(members), run, des), T_GRAY)]
        if risky:
            segs += [("  ", None), ("🛡 %d" % risky, T_RED)]
        rows.append(dict(level=0, segs=segs, color=service_color(run, des), svc=None))
        for j, svc in enumerate(members):
            name = svc[1]
            open_ = name in expanded
            kids = CONTAINERS.get(name, []) if open_ else []
            rows.append(dict(level=1, segs=[(("▾ " if open_ else "▸ ") + service_row(svc), None)],
                             color=service_color(svc[3], svc[4]), svc=name,
                             last=j == len(members) - 1))
            for k, c in enumerate(kids):
                rows.append(dict(level=2, segs=[(leaf_row(c), None)], color=P_TEXT,
                                 svc=name, last=k == len(kids) - 1,
                                 parent_last=j == len(members) - 1))
    return rows


def draw_tree(g, rows, cursor):
    """Paint the rows plus tview's tree graphics and the selected-row inversion."""
    for i, row in enumerate(rows):
        r = TREE_TOP + i
        if r >= HELP_ROW:
            break
        sel = i == cursor
        bg = row["color"] if sel else None
        base = P_BG if sel else row["color"]
        if row["level"] == 0:
            g.segs(r, 0, row["segs"], base=base, bg=bg)
        elif row["level"] == 1:
            g.put(r, 0, "╰" if row["last"] else "├", P_BORDER)
            g.put(r, 1, "──", P_BORDER)
            g.segs(r, 3, row["segs"], base=base, bg=bg)
        else:
            if not row["parent_last"]:
                g.put(r, 0, "│", P_BORDER)
            g.put(r, 3, "╰" if row["last"] else "├", P_BORDER)
            g.put(r, 4, "──", P_BORDER)
            g.segs(r, 6, row["segs"], base=base, bg=bg)


def screen(expanded, cursor, help_segs=HELP_CONTAINERS):
    g = Grid()
    draw_tabbar(g)
    draw_tree(g, tree_rows(expanded), cursor)
    draw_footer(g, help_segs)
    return g


# ============================ the risks overlay ==============================
def wrap(text, first, rest):
    """tview's word wrap: the first line continues an indented run, the rest
    start at column 0 of the box's text area."""
    out, line, width = [], "", first
    for word in text.split(" "):
        if line and len(line) + 1 + len(word) > width:
            out.append(line)
            line, width = word, rest
        else:
            line = word if not line else line + " " + word
    out.append(line)
    return out


SEV = {"high": ("● high", T_RED), "medium": ("● medium", T_YELLOW), "low": ("● low", T_GRAY)}
SEV_ORDER = ["low", "medium", "high"]


def max_severity(risks):
    """secscan.MaxSeverity()."""
    return max((f[0] for f in risks), key=SEV_ORDER.index)


def risks_overlay(highlight):
    """showSecurityRisks(): the 84×h centred box and its text."""
    flagged = [s for s in SERVICES if actionable(s[7])]
    bw, bh = 84, ROWS - 2          # centered(tv, 84, 26), clamped to the screen
    c0 = (COLS - bw) // 2
    box = (0, bh, c0, c0 + bw)
    g = Grid()
    inner, r = c0 + 1, 1
    lines = []
    lines.append([("  ", None),
                  ("%d of %d service(s) flagged — checks: root user, secret in env"
                   % (len(flagged), len(SERVICES)), T_GRAY)])
    for s in flagged:
        name, risks = s[1], s[7]
        lines.append([])
        label, col = SEV[max_severity(risks)]
        hl = name == highlight
        if hl:
            # tview paints a highlighted region with fg/bg swapped (textview.go).
            lines.append([(name, T_BLACK, T_AQUA), ("  ", T_BLACK, P_TEXT),
                          (label, T_BLACK, col)])
        else:
            lines.append([(name, T_AQUA), ("  ", None), (label, col)])
        for sev, title, detail in risks:
            slab, scol = SEV[sev]
            head = [("  ", None), (slab, scol), ("  ", None), (title, T_WHITE),
                    (" — ", None)]
            used = sum(len(t[0]) for t in head)
            body = wrap(detail, bw - 2 - used, bw - 2)
            lines.append(head + [(body[0], T_GRAY)])
            for cont in body[1:]:
                lines.append([(cont, T_GRAY)])
    for line in lines:
        c = inner
        for seg in line:
            text, fg = seg[0], seg[1]
            cellbg = seg[2] if len(seg) > 2 else None
            c = g.put(r, c, text, fg if fg else P_TEXT, cellbg)
        r += 1
    return box, g, " security risks "


# ============================ storyboard =====================================
# Row indexes in the default (all stacks open, all services collapsed) tree:
#   0 shop            1 shop_api   2 shop_db   3 shop_web   4 shop_worker
#   5 telemetry       6 telemetry_grafana      7 telemetry_loki
#   8 (no stack)      9 traefik
# Expanding shop_api inserts its three container leaves at 2..4.

# 1. The payoff frame: the whole cluster as a stack → service tree, cursor on the
#    first service exactly where renderContainers() leaves it at startup.
snap(1700, screen(set(), 1))

# 2. Enter expands the flagged service — its tasks show up, one per node.
snap(1000, screen({"shop_api"}, 1), key="Enter")

# 3. Walk down into the containers.
snap(560, screen({"shop_api"}, 2), key="j")
snap(560, screen({"shop_api"}, 3), key="j")
snap(900, screen({"shop_api"}, 4), key="j")

# 4. "!" opens the security-risks overlay, with the service under the cursor
#    pre-selected.
base = screen({"shop_api"}, 4, HELP_OVERLAY)
box, ogrid, title = risks_overlay("shop_api")
snap(3000, base, overlay=(box, ogrid, title), key="!")

TR = len(frames)
snap(2000, base, overlay=(box, ogrid, title),
     caption=("swarmexec ui — the whole cluster in one TUI",
              "stacks · services · containers — logs, exec, risks"))

# ============================ save (shared palette) ==========================
# Same pipeline as the sibling generators — one adaptive palette built from the
# frames that carry the most colour, then every frame quantized against it, so
# the GIF has no per-frame palette shift. 128 colours rather than their 64: this
# scene is 13 px text on a dark background, and the extra steps are what keep the
# antialiased glyphs from going chunky.
combo = Image.new("RGB", (W, H * 3))
combo.paste(frames[0], (0, 0))          # the text-heavy tree
combo.paste(frames[TR - 1], (0, H))     # the overlay
combo.paste(frames[TR], (0, H * 2))     # the dimmed caption card
pal = combo.convert("P", palette=Image.ADAPTIVE, colors=128)
qf = [f.quantize(palette=pal, dither=Image.NONE) for f in frames]
qf[0].save(OUT, save_all=True, append_images=qf[1:], duration=durations,
           loop=0, optimize=True, disposal=2)
print(f"wrote {OUT}: {len(frames)} frames, {sum(durations)/1000:.1f}s")
