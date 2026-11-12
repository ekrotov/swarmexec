#!/usr/bin/env python3
"""Generate site/og-image.png — the 1200x630 social preview card.

Link previews (og:image / twitter:image) used to point at demo.gif: 860x470,
animated, ~400 KB. Most previewers show only a GIF's first frame, crop it to
their own aspect ratio, and some refuse a file that size. This card is static,
has the 1.91:1 shape the previewers expect, and stays small.

It is drawn in the landing page's own look — the same paper and ink, the
aqua of the mark, Archivo for the headline and JetBrains Mono for the
command — with the fonts read straight from site/fonts/, so no system font
is involved and the card matches the page on any machine that runs this.

The PNG is committed, so you only rerun this to change the card. Needs
Pillow (with FreeType that reads woff2, as any current wheel does). Usage,
from the repo root:

    python3 site/og-gen.py site/og-image.png
"""
import os
import sys

from PIL import Image, ImageDraw, ImageFont

OUT = sys.argv[1] if len(sys.argv) > 1 else "og-image.png"
HERE = os.path.dirname(os.path.abspath(__file__))
FONTS = os.path.join(HERE, "fonts")

W, H = 1200, 630

# --- palette: landing.css :root (light) ------------------------------------
PAPER = (0xEC, 0xEE, 0xE9)
CARD = (0xF7, 0xF8, 0xF5)
INK = (0x14, 0x20, 0x1E)
INK2 = (0x3F, 0x4D, 0x4A)
MUTED = (0x5F, 0x6D, 0x69)
RULE = (0xC4, 0xCC, 0xC7)
AQUA = (0x0A, 0x76, 0x80)
TERM = (0x14, 0x20, 0x22)
T_INK = (0xD5, 0xE1, 0xDD)
T_DIM = (0x7F, 0x92, 0x8D)
T_AQUA = (0x55, 0xD3, 0xDB)


def archivo(size, weight, width):
    f = ImageFont.truetype(os.path.join(FONTS, "archivo-latin-4c18bb.woff2"), size)
    f.set_variation_by_axes([weight, width])
    return f


def mono(size, weight=400):
    f = ImageFont.truetype(os.path.join(FONTS, "jetbrains-mono-latin-0a7197.woff2"), size)
    try:
        f.set_variation_by_axes([weight])
    except (OSError, ValueError):
        pass  # a static cut has no axes; its own weight is fine
    return f


img = Image.new("RGB", (W, H), PAPER)
d = ImageDraw.Draw(img)

M = 72  # outer margin

# --- the mark: the favicon's chevron and underscore, scaled -----------------
def mark(x, y, s):
    k = s / 100.0
    pts = [(x + 28 * k, y + 33 * k), (x + 48 * k, y + 50 * k), (x + 28 * k, y + 67 * k)]
    d.line(pts, fill=AQUA, width=round(10 * k), joint="curve")
    r = 5 * k
    for px, py in (pts[0], pts[2]):
        d.ellipse([px - r, py - r, px + r, py + r], fill=AQUA)
    d.rounded_rectangle([x + 54 * k, y + 61 * k, x + 80 * k, y + 70 * k], radius=4.5 * k, fill=AQUA)


mark(M - 14, 52, 64)
d.text((M + 50, 66), "swarmexec", font=mono(30, 700), fill=INK)

# --- headline ----------------------------------------------------------------
head = archivo(92, 780, 66)
d.text((M, 150), "Every container in your Swarm,", font=head, fill=INK)
d.text((M, 246), "one terminal away.", font=head, fill=INK)

sub = archivo(31, 500, 100)
d.text((M, 362), "exec, logs and port-forward into any task on any node —", font=sub, fill=INK2)
d.text((M, 402), "no SSH to the node, no published port.", font=sub, fill=INK2)

# --- a terminal strip with the one-liner ------------------------------------
ty = 462
d.rounded_rectangle([M, ty, W - M, ty + 74], radius=10, fill=TERM)
cmd = mono(30, 500)
x = M + 28
d.text((x, ty + 20), "$", font=cmd, fill=T_AQUA)
x += d.textlength("$ ", font=cmd)
d.text((x, ty + 20), "swarmexec exec web.2 -- sh", font=cmd, fill=T_INK)
x += d.textlength("swarmexec exec web.2 -- sh   ", font=cmd)
d.text((x, ty + 20), "# replica 2, any node", font=cmd, fill=T_DIM)

# --- footer line -------------------------------------------------------------
d.line([(M, 572), (W - M, 572)], fill=RULE, width=2)
small = mono(22, 500)
d.text((M, 584), "swarm-exec.cloud-surfers.net", font=small, fill=AQUA)
right = "Apache-2.0 · Docker Swarm"
d.text((W - M - d.textlength(right, font=small), 584), right, font=small, fill=MUTED)

# A small palette keeps the file small; the card has a handful of flat colours
# plus antialiasing, which 64 colours carry without visible banding.
img.convert("P", palette=Image.Palette.ADAPTIVE, colors=64).save(OUT, optimize=True)
print(f"wrote {OUT} ({W}x{H}, {os.path.getsize(OUT)} bytes)")
