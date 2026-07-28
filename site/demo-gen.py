#!/usr/bin/env python3
"""Generate site/demo.gif — the swarmexec before/after demo (docker exec vs swarmexec).

The GIF is committed, so you only rerun this to change the animation. Needs
Pillow and the Ubuntu Mono fonts (fonts-ubuntu; the paths below are for
Debian/Ubuntu). Usage, from the repo root:

    python3 site/demo-gen.py site/demo.gif
"""
import sys
from PIL import Image, ImageDraw, ImageFont

OUT = sys.argv[1] if len(sys.argv) > 1 else "demo.gif"

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


def type_cmd(prompt, cmd, color=FG, bold=True, step=2):
    new_row()
    for seg in prompt:
        put(*seg)
    put("", color, bold)
    i = 0
    while i < len(cmd):
        i += step
        t, c, b = rows[-1][0 - 1]
        rows[-1][-1] = (cmd[:i], c, b)
        snap(60)
    snap(320)


def out(text, color=MUTED, bold=False):
    new_row()
    put(text, color, bold)


LOCAL = [("you@laptop", GREEN, False), (":~", MUTED, False), ("$ ", AQUA, True)]
NODE  = [("you@node-3", RED, False), (":~", MUTED, False), ("$ ", AQUA, True)]


def comment(prompt, text):
    new_row()
    for seg in prompt:
        put(*seg)
    put("# " + text, COMMENT, False)


# ============================ SCENE A: docker exec ==========================
snap(500)
type_cmd(LOCAL, "docker node ls")
out("ID          HOSTNAME   STATUS   AVAILABILITY   MANAGER"); snap(90)
out("9r2x8f…     node-1     Ready    Active         Leader"); snap(90)
out("7k4m2q…     node-2     Ready    Active"); snap(90)
out("2p8q6t…     node-3     Ready    Active"); snap(750)
comment(LOCAL, "…which node runs \"web\"? no idea."); snap(950)
type_cmd(LOCAL, "docker service ps web")
out("node-3", BLUE); snap(800)
type_cmd(LOCAL, "ssh node-3"); snap(400)
type_cmd(NODE, "docker exec -it a1b2c3d4e5f6 sh")
new_row(); put("/ ", MUTED); put("# ", GREEN, True); put("…finally.", COMMENT)
snap(1300)

TR = len(frames)
snap(1700, cursor=False,
     caption=("docker exec: find the node, SSH in, then exec",
              "one command does all of it"))

# ============================ SCENE B: swarmexec ============================
rows.clear()
snap(400)
type_cmd(LOCAL, "swarmexec exec web -- sh")
new_row(); put("/ ", MUTED); put("# ", GREEN, True)
put("you're in", AQUA, True); put(" — any node, one command", MUTED)
snap(2500)
snap(1400)

# ============================ save (shared 64-color palette) =================
combo = Image.new("RGB", (W, H * 2))
combo.paste(frames[TR - 2], (0, 0))   # text-heavy scene A frame
combo.paste(frames[TR], (0, H))        # dimmed transition card
pal = combo.convert("P", palette=Image.ADAPTIVE, colors=64)
qf = [f.quantize(palette=pal, dither=Image.NONE) for f in frames]
qf[0].save(OUT, save_all=True, append_images=qf[1:], duration=durations,
           loop=0, optimize=True, disposal=2)
print(f"wrote {OUT}: {len(frames)} frames, {sum(durations)/1000:.1f}s")
