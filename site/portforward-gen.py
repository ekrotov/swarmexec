#!/usr/bin/env python3
"""Generate site/portforward.gif — the swarmexec port-forward demo.

Sibling of demo-gen.py: same scripted-terminal mechanism, palette, theme and
save pipeline. It does NOT need a live cluster — every line is scripted. The
scene shows `swarmexec port-forward` bringing up a forward to a task on a
remote node, then a plain `curl localhost:<port>` succeeding against it — no
SSH, no published port, no ingress.

The command/output strings mirror the real CLI:
  * `port-forward <target> [local:]remote` with the kubectl-style `9090:8080`
    local:remote spelling (client/internal/cli/portforward.go).
  * the runtime line `forwarding 127.0.0.1:9090 -> <12hex>:8080 (node-3) —
    press Ctrl-C to stop` (runForwarder(), same file; container id is shortID,
    i.e. the first 12 chars).

The GIF is committed, so you only rerun this to change the animation. Needs
Pillow and the Ubuntu Mono fonts (fonts-ubuntu; the paths below are for
Debian/Ubuntu). Usage, from the repo root:

    python3 site/portforward-gen.py site/portforward.gif
"""
import sys
from PIL import Image, ImageDraw, ImageFont

OUT = sys.argv[1] if len(sys.argv) > 1 else "portforward.gif"

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
    """Place a fully typed command line in one frame — used for the hero
    first frame so the marquee `port-forward` command is legible immediately."""
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


LOCAL = [("you@laptop", GREEN, False), (":~", MUTED, False), ("$ ", AQUA, True)]

# ============================ SCENE: port-forward ===========================
# Frame 0: the payoff command, fully legible right away.
show_cmd(LOCAL, "swarmexec port-forward web 9090:8080")
snap(1200)

# The forward comes up (mirrors runForwarder's stdout line).
out()
put("forwarding ", MUTED)
put("127.0.0.1:9090", AQUA, True)
put(" -> ", MUTED)
put("a1b2c3d4e5f6", BLUE)
put(":8080 ", MUTED)
put("(node-3)", BLUE)
put(" — press Ctrl-C to stop", COMMENT)
snap(600)
comment(LOCAL, "8080 is NOT published — no ingress, no SSH to node-3")
snap(1300)

# A second terminal: hit it as if it were local.
out(); snap(300)
type_cmd(LOCAL, "curl -s localhost:9090/api/health")
out('{"status":"ok","served_by":"web.2","node":"node-3"}', GREEN)
snap(300)
new_row()
put("-> ", AQUA, True)
put("you just reached a container on node-3", AQUA, True)
put(" — from your laptop", MUTED)
snap(2200)

TR = len(frames)
snap(1900, cursor=False,
     caption=("port-forward from your laptop",
              "any task's port — no ingress, no SSH"))

# ============================ save (shared 64-color palette) =================
combo = Image.new("RGB", (W, H * 2))
combo.paste(frames[TR - 1], (0, 0))   # text-heavy final scene frame
combo.paste(frames[TR], (0, H))        # dimmed caption card
pal = combo.convert("P", palette=Image.ADAPTIVE, colors=64)
qf = [f.quantize(palette=pal, dither=Image.NONE) for f in frames]
qf[0].save(OUT, save_all=True, append_images=qf[1:], duration=durations,
           loop=0, optimize=True, disposal=2)
print(f"wrote {OUT}: {len(frames)} frames, {sum(durations)/1000:.1f}s")
